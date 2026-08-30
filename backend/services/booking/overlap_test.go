// Public occupancy tests (Phase 3): the three tests from the lock. Real
// Postgres + real GiST, real allocator servers, real Catalog handlers, Booking
// HTTP via httptest. Each topology is test-owned (no migration seed):
//
//	Same-tech: 1 tech, >=1 bay      — loser must die at Technician GiST
//	Same-bay:  >=2 techs, 1 bay     — loser must reach and die at Bay GiST
//	Compensate: >=1 tech, 0 bays    — 409 and the tech occupation is RELEASED
//
// Removing either GiST (or the compensate release) makes these fail.
package booking_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/athandoan/unified-service-scheduler/services/bay"
	"github.com/athandoan/unified-service-scheduler/services/booking"
	"github.com/athandoan/unified-service-scheduler/services/catalog"
	"github.com/athandoan/unified-service-scheduler/services/technician"
	bayv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/bay/v1"
	techv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/technician/v1"
	"github.com/athandoan/unified-service-scheduler/shared/pkg/integrationtest"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const (
	techDSN = "postgres://scheduler:scheduler@localhost:5432/technician?sslmode=disable"
	bayDSN  = "postgres://scheduler:scheduler@localhost:5432/bay?sslmode=disable"
	catDSN  = "postgres://scheduler:scheduler@localhost:5432/catalog?sslmode=disable"
	bookDSN = "postgres://scheduler:scheduler@localhost:5432/booking?sslmode=disable"
)

// env is one test's live stack: real handlers over the compose DBs.
type env struct {
	bookingURL string
	client     *http.Client
	techPool   *pgxpool.Pool
	bayPool    *pgxpool.Pool
	bookPool   *pgxpool.Pool
	catPool    *pgxpool.Pool
	dealerID   string
	customerID string
	vehicleID  string
	svcTypeID  string
	techIDs    []string
	bayIDs     []string
	zone       *time.Location
}

// newEnv wires: catalog handlers (httptest) → booking service (httptest) with
// in-process allocator gRPC servers on real compose DBs. Fails when Postgres
// is unreachable — never skips.
func newEnv(t *testing.T, nTechs, nBays int) *env {
	t.Helper()
	techPool := integrationtest.Connect(t, techDSN)
	bayPool := integrationtest.Connect(t, bayDSN)
	catPool := integrationtest.Connect(t, catDSN)
	bookPool := integrationtest.Connect(t, bookDSN)
	dealerID := integrationtest.NewUUID(t)
	customerID := integrationtest.NewUUID(t)
	vehicleID := integrationtest.NewUUID(t)
	svcTypeID := integrationtest.NewUUID(t)
	zone := integrationtest.TestZone(t)
	// Catalog fixture rows (ownership + hours + IANA TZ; duration display-only).
	e := &env{
		techPool:   techPool,
		bayPool:    bayPool,
		bookPool:   bookPool,
		catPool:    catPool,
		dealerID:   dealerID,
		customerID: customerID,
		vehicleID:  vehicleID,
		svcTypeID:  svcTypeID,
		zone:       zone,
		client:     http.DefaultClient,
	}
	_, err := catPool.Exec(context.Background(),
		`INSERT INTO dealership (id, name, timezone) VALUES ($1,'Test Dealer',$2)`, dealerID, "Europe/London")
	require.NoError(t, err)
	for wd := 1; wd <= 5; wd++ {
		_, err = catPool.Exec(context.Background(),
			`INSERT INTO opening_hours (dealership_id, weekday, open_minutes, close_minutes) VALUES ($1,$2,480,1020)`,
			dealerID, wd)
		require.NoError(t, err)
	}
	_, err = catPool.Exec(context.Background(),
		`INSERT INTO service_type (id, name, duration_minutes) VALUES ($1,'Test service',30)`, svcTypeID)
	require.NoError(t, err)
	_, err = catPool.Exec(context.Background(),
		`INSERT INTO customer (id, name) VALUES ($1,'Test Customer')`, customerID)
	require.NoError(t, err)
	_, err = catPool.Exec(context.Background(),
		`INSERT INTO vehicle (id, customer_id, vin) VALUES ($1,$2,$3)`, vehicleID, customerID, "VINTEST"+integrationtest.NewUUID(t)[:11])
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = catPool.Exec(ctx, `DELETE FROM vehicle WHERE customer_id = $1`, customerID)
		_, _ = catPool.Exec(ctx, `DELETE FROM customer WHERE id = $1`, customerID)
		_, _ = catPool.Exec(ctx, `DELETE FROM service_type WHERE id = $1`, svcTypeID)
		_, _ = catPool.Exec(ctx, `DELETE FROM opening_hours WHERE dealership_id = $1`, dealerID)
		_, _ = catPool.Exec(ctx, `DELETE FROM dealership WHERE id = $1`, dealerID)
	})
	// Technicians: skill + shifts, timezone copied from the dealership IANA TZ.
	for i := 0; i < nTechs; i++ {
		techID := integrationtest.NewUUID(t)
		_, err = techPool.Exec(context.Background(),
			`INSERT INTO technician (id, dealership_id, active, timezone) VALUES ($1,$2,true,$3)`,
			techID, dealerID, "Europe/London")
		require.NoError(t, err)
		_, err = techPool.Exec(context.Background(),
			`INSERT INTO technician_skill (technician_id, service_type_id, duration_minutes) VALUES ($1,$2,30)`,
			techID, svcTypeID)
		require.NoError(t, err)
		for wd := 1; wd <= 5; wd++ {
			_, err = techPool.Exec(context.Background(),
				`INSERT INTO technician_shift (technician_id, weekday, start_minutes, end_minutes) VALUES ($1,$2,480,1020)`,
				techID, wd)
			require.NoError(t, err)
		}
		integrationtest.CleanupTable(t, techPool, "technician", "id", techID)
		e.techIDs = append(e.techIDs, techID)
	}
	// Bays (0 for the compensate test).
	for i := 0; i < nBays; i++ {
		bayID := integrationtest.NewUUID(t)
		_, err = bayPool.Exec(context.Background(),
			`INSERT INTO service_bay (id, dealership_id, active) VALUES ($1,$2,true)`, bayID, dealerID)
		require.NoError(t, err)
		integrationtest.CleanupTable(t, bayPool, "service_bay", "id", bayID)
		e.bayIDs = append(e.bayIDs, bayID)
	}
	// Real Catalog handlers in-process.
	catSrv := httptest.NewServer(catalog.NewHandler(catPool))
	t.Cleanup(catSrv.Close)
	// Real allocator gRPC servers in-process (buffered listeners).
	techLis := bufconnListen()
	bayLis := bufconnListen()
	techSrv := grpc.NewServer()
	techv1.RegisterTechnicianServiceServer(techSrv, technician.NewServer(techPool))
	baySrv := grpc.NewServer()
	bayv1.RegisterBayServiceServer(baySrv, bay.NewServer(bayPool))
	go techSrv.Serve(techLis)
	go baySrv.Serve(bayLis)
	t.Cleanup(func() { techSrv.Stop(); baySrv.Stop() })
	techConn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(bufconnDialer(techLis)), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	bayConn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(bufconnDialer(bayLis)), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = techConn.Close(); _ = bayConn.Close() })
	// Booking HTTP in-process.
	svc := booking.NewService(
		booking.NewStore(bookPool),
		booking.NewCatalogClient(catSrv.URL),
		booking.Allocators{
			Tech: techv1.NewTechnicianServiceClient(techConn),
			Bay:  bayv1.NewBayServiceClient(bayConn),
		},
	)
	bookSrv := httptest.NewServer(booking.NewHandler(svc))
	t.Cleanup(bookSrv.Close)
	e.bookingURL = bookSrv.URL
	e.client = bookSrv.Client()
	t.Cleanup(func() {
		_, _ = bookPool.Exec(context.Background(),
			`DELETE FROM appointment WHERE dealership_id = $1`, dealerID)
	})
	return e
}

// postJSON posts and returns the status + body.
func (e *env) post(t *testing.T, path string, payload any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, e.bookingURL+path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return e.do(t, req)
}
func (e *env) do(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	res, err := e.client.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// confirmBody builds a confirm-without-hold payload at the given start.
func (e *env) confirmBody(start time.Time) map[string]any {
	return map[string]any{
		"dealershipId":  e.dealerID,
		"customerId":    e.customerID,
		"vehicleId":     e.vehicleID,
		"serviceTypeId": e.svcTypeID,
		"start":         start.UTC().Format(time.RFC3339Nano),
	}
}
func concurrentConfirms(t *testing.T, e *env, n int, start time.Time) (codes2 []int) {
	t.Helper()
	var mu sync.Mutex
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate // concurrent, not serialized
			code, _ := e.post(t, "/appointments", map[string]any{
				"dealershipId":  e.dealerID,
				"customerId":    e.customerID,
				"vehicleId":     e.vehicleID,
				"serviceTypeId": e.svcTypeID,
				"start":         start.UTC().Format(time.RFC3339Nano),
			}, map[string]string{
				"Idempotency-Key": fmt.Sprintf("overlap-%s-%d", integrationtest.NewUUID(t), i),
				"X-Request-Id":    integrationtest.NewUUID(t),
			})
			mu.Lock()
			codes2 = append(codes2, code)
			mu.Unlock()
		}(i)
	}
	close(gate)
	wg.Wait()
	return codes2
}
