//go:build e2e

// Local black-box E2E suite against the compose stack: every HTTP call goes to
// the Gateway on :8080 (GATEWAY_URL overrides); seeding/cleanup uses the
// compose Postgres on :5432 (catalog/technician/bay/booking DSNs, same as
// shared/pkg/integrationtest). It never talks to :8081/:8082 or gRPC
// :9091/:9092.
//
// Every file carries //go:build e2e so `go test ./...` (the occupancy gate)
// and `go test -short ./...` (units) never run these. The suite FAILS — not
// skips — when the Gateway is unreachable: the composed write path is what is
// under test.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/athandoan/unified-service-scheduler/shared/pkg/integrationtest"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// Fixture identity from harness/fixtures.json — used as identity only, never
// re-seeded. The fixture dealer must never gain technician/bay rows.
const (
	fixtureDealerID      = "11111111-1111-4111-8111-111111111111" // North Workshop (GET-only)
	fixtureCustomerID    = "55555555-5555-4555-8555-555555555555" // Ada Customer
	fixtureVehicleID     = "77777777-7777-4777-8777-777777777777" // Ada's vehicle
	fixtureServiceTypeID = "33333333-3333-4333-8333-333333333333" // Oil change, 30 min
)

var (
	gatewayBase = envOr("GATEWAY_URL", "http://localhost:8080")
	catalogDSN  = envOr("CATALOG_TEST_DSN", "postgres://scheduler:scheduler@localhost:5432/catalog?sslmode=disable")
	techDSN     = envOr("TECHNICIAN_TEST_DSN", "postgres://scheduler:scheduler@localhost:5432/technician?sslmode=disable")
	bayDSN      = envOr("BAY_TEST_DSN", "postgres://scheduler:scheduler@localhost:5432/bay?sslmode=disable")
	bookDSN     = envOr("BOOKING_TEST_DSN", "postgres://scheduler:scheduler@localhost:5432/booking?sslmode=disable")
)

// e2eClient is goroutine-safe and shared by all cases (the concurrency case
// fires requests from non-test goroutines).
var e2eClient = &http.Client{Timeout: 60 * time.Second}

// TestMain fails the suite when the Gateway is not up (dial + GET
// /dealerships, bounded retry) — it is not a testing.Short() skip.
func TestMain(m *testing.M) {
	waitForGateway()
	os.Exit(m.Run())
}

func waitForGateway() {
	u, err := url.Parse(gatewayBase)
	if err != nil || u.Host == "" {
		fmt.Fprintf(os.Stderr, "e2e: invalid GATEWAY_URL %q: %v\n", gatewayBase, err)
		os.Exit(1)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if up() {
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "e2e: Gateway not reachable at %s (dial/GET /dealerships failed; run: cd backend && docker compose up --build -d)\n", gatewayBase)
			os.Exit(1)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func up() bool {
	conn, err := net.DialTimeout("tcp", hostPort(gatewayBase), 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	resp, err := e2eClient.Get(gatewayBase + "/dealerships")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

func hostPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	p := u.Port()
	if p == "" {
		if u.Scheme == "https" {
			p = "443"
		} else {
			p = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), p)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// connect FAILS (never skips) when Postgres is unreachable — same stance as
// integrationtest.Connect, minus its -short skip (E2E never runs under -short).
func connect(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err, "cannot create pool for %s", dsn)
	require.NoError(t, pool.Ping(ctx), "cannot reach Postgres (%s); run: docker compose up -d && go run ./migrate", dsn)
	return pool
}

// topology is one case's test-owned fixture set. The fixture dealer is never
// touched; each case seeds its own dealer + hours + techs + bays on :5432.
type topology struct {
	dealerID string
	techIDs  []string
	bayIDs   []string
	zone     *time.Location
}

// seedTopology inserts a fresh dealership (Europe/London, Mon–Fri 08:00–17:00
// opening hours) plus nTechs qualified technicians (timezone copied from the
// dealer, skill for the fixture oil change at 30 minutes, weekday shifts
// covering the hours) and nBays service bays. Everything is cleaned up.
func seedTopology(t *testing.T, nTechs, nBays int) *topology {
	t.Helper()
	ctx := context.Background()
	catPool := connect(t, catalogDSN)
	techPool := connect(t, techDSN)
	bayPool := connect(t, bayDSN)
	bookPool := connect(t, bookDSN)

	topo := &topology{
		dealerID: integrationtest.NewUUID(t),
		zone:     integrationtest.TestZone(t),
	}
	const tz = "Europe/London" // the dealership IANA zone, copied onto techs

	_, err := catPool.Exec(ctx,
		`INSERT INTO dealership (id, name, timezone) VALUES ($1,'E2E Test Dealer',$2)`, topo.dealerID, tz)
	require.NoError(t, err)
	for wd := 1; wd <= 5; wd++ { // weekday 1–5, matching a future Mon–Fri start
		_, err = catPool.Exec(ctx,
			`INSERT INTO opening_hours (dealership_id, weekday, open_minutes, close_minutes) VALUES ($1,$2,480,1020)`,
			topo.dealerID, wd)
		require.NoError(t, err)
	}
	for i := 0; i < nTechs; i++ {
		techID := integrationtest.NewUUID(t)
		_, err = techPool.Exec(ctx,
			`INSERT INTO technician (id, dealership_id, active, timezone) VALUES ($1,$2,true,$3)`,
			techID, topo.dealerID, tz) // timezone copied from the dealer
		require.NoError(t, err)
		_, err = techPool.Exec(ctx,
			`INSERT INTO technician_skill (technician_id, service_type_id, duration_minutes) VALUES ($1,$2,30)`,
			techID, fixtureServiceTypeID)
		require.NoError(t, err)
		for wd := 1; wd <= 5; wd++ { // shifts cover the 480–1020 opening window
			_, err = techPool.Exec(ctx,
				`INSERT INTO technician_shift (technician_id, weekday, start_minutes, end_minutes) VALUES ($1,$2,480,1020)`,
				techID, wd)
			require.NoError(t, err)
		}
		topo.techIDs = append(topo.techIDs, techID)
	}
	for i := 0; i < nBays; i++ {
		bayID := integrationtest.NewUUID(t)
		_, err = bayPool.Exec(ctx,
			`INSERT INTO service_bay (id, dealership_id, active) VALUES ($1,$2,true)`, bayID, topo.dealerID)
		require.NoError(t, err)
		topo.bayIDs = append(topo.bayIDs, bayID)
	}

	// Cleanup dealer/tech/bay rows (tech/bay deletes cascade their skill,
	// shift and occupation rows); appointments first so nothing dangles.
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = bookPool.Exec(ctx, `DELETE FROM appointment WHERE dealership_id = $1`, topo.dealerID)
		_, _ = catPool.Exec(ctx, `DELETE FROM opening_hours WHERE dealership_id = $1`, topo.dealerID)
		_, _ = catPool.Exec(ctx, `DELETE FROM dealership WHERE id = $1`, topo.dealerID)
		for _, id := range topo.techIDs {
			_, _ = techPool.Exec(ctx, `DELETE FROM technician WHERE id = $1`, id)
		}
		for _, id := range topo.bayIDs {
			_, _ = bayPool.Exec(ctx, `DELETE FROM service_bay WHERE id = $1`, id)
		}
		bookPool.Close()
		bayPool.Close()
		techPool.Close()
		catPool.Close()
	})
	return topo
}

// futureStart is a strictly future start at 10:00 local on the given ISO
// weekday — inside the seeded 480–1020 hours and shifts (not OpenAPI's past
// example start).
func futureStart(t *testing.T, weekday int) time.Time {
	t.Helper()
	return integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), weekday, 10, 0)
}

// confirmBody builds a confirm/hold payload on the test-owned dealer using the
// fixture customer/vehicle/serviceType as identity only.
func confirmBody(topo *topology, start time.Time) map[string]any {
	return map[string]any{
		"dealershipId":  topo.dealerID,
		"customerId":    fixtureCustomerID,
		"vehicleId":     fixtureVehicleID,
		"serviceTypeId": fixtureServiceTypeID,
		"start":         start.UTC().Format(time.RFC3339Nano),
	}
}

// rawPost/rawGet carry no *testing.T so the concurrency case can fire them
// from non-test goroutines.
func rawPost(path string, body any, headers map[string]string) (int, []byte, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, gatewayBase+path, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return do(req)
}

func rawGet(path string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, gatewayBase+path, nil)
	if err != nil {
		return 0, nil, err
	}
	return do(req)
}

func do(req *http.Request) (int, []byte, error) {
	res, err := e2eClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	return res.StatusCode, out, err
}

func decodeMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	out := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		require.NoError(t, json.Unmarshal(raw, &out), "bad JSON body: %s", raw)
	}
	return out
}

func post(t *testing.T, path string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	code, raw, err := rawPost(path, body, headers)
	require.NoError(t, err)
	return code, decodeMap(t, raw)
}

func get(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	code, raw, err := rawGet(path)
	require.NoError(t, err)
	return code, decodeMap(t, raw)
}

func getList(t *testing.T, path string) (int, []map[string]any) {
	t.Helper()
	code, raw, err := rawGet(path)
	require.NoError(t, err)
	var out []map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		require.NoError(t, json.Unmarshal(raw, &out), "bad JSON array: %s", raw)
	}
	return code, out
}