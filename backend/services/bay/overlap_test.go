package bay_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/athandoan/unified-service-scheduler/services/bay"
	bayv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/bay/v1"
	"github.com/athandoan/unified-service-scheduler/shared/pkg/integrationtest"
)

func setupBay(t *testing.T, pool *pgxpool.Pool) (dealerID, bayID string) {
	t.Helper()
	dealerID, bayID = integrationtest.NewUUID(t), integrationtest.NewUUID(t)
	_, err := pool.Exec(context.Background(),
		`INSERT INTO service_bay (id, dealership_id, active) VALUES ($1, $2, true)`,
		bayID, dealerID)
	require.NoError(t, err)
	integrationtest.CleanupTable(t, pool, "service_bay", "id", bayID)
	return dealerID, bayID
}

// Two concurrent Reserves on one bay, same future interval: exactly one wins,
// one gets FailedPrecondition, and exactly one live HELD occupation exists.
// Removing the GiST exclusion makes both Reserves succeed and this test fail.
func TestBayReserve_SameBayOverlap_OnlyOneWins(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.BayDSN)
	dealerID, _ := setupBay(t, pool)

	start := integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), 1, 10, 0) // Monday 10:00 Europe/London, future
	end := start.Add(30 * time.Minute)

	srv := bay.NewServer(pool)
	ctx := context.Background()

	const n = 2
	results := make([]*bayv1.ReserveResponse, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate // release both goroutines together: concurrent, not serialized
			results[i], errs[i] = srv.Reserve(ctx, &bayv1.ReserveRequest{
				DealershipId: dealerID,
				Start:        timestamppb.New(start),
				End:          timestamppb.New(end),
				RequestId:    "overlap-same-bay-" + integrationtest.NewUUID(t),
			})
		}(i)
	}
	close(gate)
	wg.Wait()

	wins := 0
	var winnerOccID string
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			wins++
			winnerOccID = results[i].GetOccupationId()
		} else {
			t.Logf("bay goroutine %d lost the race (expected for exactly one): %v", i, errs[i])
		}
	}
	require.Equal(t, 1, wins, "exactly one bay Reserve must win")

	var status string
	var rowCount int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status FROM bay_occupation WHERE id = $1`, winnerOccID).Scan(&status))
	require.Equal(t, "HELD", status)
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM bay_occupation
		WHERE service_bay_id = (SELECT service_bay_id FROM bay_occupation WHERE id = $1)
		  AND status IN ('HELD', 'CONFIRMED')
		  AND tstzrange(start_at, end_at, '[)') && tstzrange($2::timestamptz, $3::timestamptz, '[)')`,
		winnerOccID, start, end).Scan(&rowCount))
	require.Equal(t, 1, rowCount, "exactly one live occupation on this bay in the interval")

	// TTL: hold_expires_at ≈ now + 120s, fixed in SQL (no env override).
	var holdExpiry time.Time
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT hold_expires_at FROM bay_occupation WHERE id = $1`, winnerOccID).Scan(&holdExpiry))
	require.WithinDuration(t, time.Now().Add(120*time.Second), holdExpiry, 15*time.Second)
}

// Confirm is idempotent: confirming the same occupation twice must succeed
// both times (the second is a no-op on the already-CONFIRMED row), and the row
// must end CONFIRMED. Fails if Confirm treats CONFIRMED as FailedPrecondition.
func TestBayConfirm_Twice_IdempotentSuccess(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.BayDSN)
	dealerID, _ := setupBay(t, pool)

	start := integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), 4, 9, 0) // Thursday 09:00 local
	end := start.Add(30 * time.Minute)

	srv := bay.NewServer(pool)
	ctx := context.Background()
	first, err := srv.Reserve(ctx, &bayv1.ReserveRequest{
		DealershipId: dealerID,
		Start:        timestamppb.New(start),
		End:          timestamppb.New(end),
		RequestId:    "confirm-twice-bay-" + integrationtest.NewUUID(t),
	})
	require.NoError(t, err)

	occID := first.GetOccupationId()
	_, err = srv.Confirm(ctx, &bayv1.ConfirmRequest{OccupationId: occID})
	require.NoError(t, err, "first Confirm of HELD must succeed")
	_, err = srv.Confirm(ctx, &bayv1.ConfirmRequest{OccupationId: occID})
	require.NoError(t, err, "second Confirm of CONFIRMED must be a no-op success, not FailedPrecondition")

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM bay_occupation WHERE id = $1`, occID).Scan(&status))
	require.Equal(t, "CONFIRMED", status)
}

// Release is idempotent: releasing twice must succeed both times, and releasing
// an unknown occupation id must also succeed (no-op). Fails if Release 409s on
// the second call or on an unknown id.
func TestBayRelease_TwiceAndUnknown_NoOpSuccess(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.BayDSN)
	dealerID, _ := setupBay(t, pool)

	start := integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), 4, 11, 0) // Thursday 11:00 local
	end := start.Add(30 * time.Minute)

	srv := bay.NewServer(pool)
	ctx := context.Background()
	first, err := srv.Reserve(ctx, &bayv1.ReserveRequest{
		DealershipId: dealerID,
		Start:        timestamppb.New(start),
		End:          timestamppb.New(end),
		RequestId:    "release-twice-bay-" + integrationtest.NewUUID(t),
	})
	require.NoError(t, err)

	occID := first.GetOccupationId()
	_, err = srv.Release(ctx, &bayv1.ReleaseRequest{OccupationId: occID})
	require.NoError(t, err, "first Release of HELD must succeed")
	_, err = srv.Release(ctx, &bayv1.ReleaseRequest{OccupationId: occID})
	require.NoError(t, err, "second Release of RELEASED must be a no-op success, not FailedPrecondition")

	// Unknown occupation id: Release is a no-op success too.
	_, err = srv.Release(ctx, &bayv1.ReleaseRequest{OccupationId: integrationtest.NewUUID(t)})
	require.NoError(t, err, "Release of an unknown occupation id must be a no-op success")

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM bay_occupation WHERE id = $1`, occID).Scan(&status))
	require.Equal(t, "RELEASED", status)
}

// Confirm of a RELEASED occupation must fail with FailedPrecondition — the
// claim was dropped and must not be resurrected in place.
func TestBayConfirm_Released_Fails(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.BayDSN)
	dealerID, _ := setupBay(t, pool)

	start := integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), 5, 9, 0) // Friday 09:00 local
	end := start.Add(30 * time.Minute)

	srv := bay.NewServer(pool)
	ctx := context.Background()
	first, err := srv.Reserve(ctx, &bayv1.ReserveRequest{
		DealershipId: dealerID,
		Start:        timestamppb.New(start),
		End:          timestamppb.New(end),
		RequestId:    "confirm-released-bay-" + integrationtest.NewUUID(t),
	})
	require.NoError(t, err)

	occID := first.GetOccupationId()
	_, err = srv.Release(ctx, &bayv1.ReleaseRequest{OccupationId: occID})
	require.NoError(t, err)

	_, err = srv.Confirm(ctx, &bayv1.ConfirmRequest{OccupationId: occID})
	require.Error(t, err, "Confirm of a RELEASED occupation must fail")
	require.Contains(t, err.Error(), "FailedPrecondition")

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM bay_occupation WHERE id = $1`, occID).Scan(&status))
	require.Equal(t, "RELEASED", status)
}

// Confirm of an expired HELD must fail with FailedPrecondition and must not
// confirm it as a side effect: backdate hold_expires_at, Confirm fails, and the
// row stays HELD (never CONFIRMED).
func TestBayConfirm_ExpiredHold_FailsAndStaysHeld(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.BayDSN)
	dealerID, _ := setupBay(t, pool)

	start := integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), 5, 11, 0) // Friday 11:00 local
	end := start.Add(30 * time.Minute)

	srv := bay.NewServer(pool)
	ctx := context.Background()
	first, err := srv.Reserve(ctx, &bayv1.ReserveRequest{
		DealershipId: dealerID,
		Start:        timestamppb.New(start),
		End:          timestamppb.New(end),
		RequestId:    "confirm-expired-bay-" + integrationtest.NewUUID(t),
	})
	require.NoError(t, err)

	occID := first.GetOccupationId()
	_, err = pool.Exec(ctx,
		`UPDATE bay_occupation SET hold_expires_at = now() - interval '1 second' WHERE id = $1`,
		occID)
	require.NoError(t, err)

	_, err = srv.Confirm(ctx, &bayv1.ConfirmRequest{OccupationId: occID})
	require.Error(t, err, "Confirm of an expired HELD must fail")
	require.Contains(t, err.Error(), "FailedPrecondition")

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM bay_occupation WHERE id = $1`, occID).Scan(&status))
	require.Equal(t, "HELD", status, "expired HELD must not become CONFIRMED")
}

// Two eligible bays: occupy the hash LIMIT 1 winner, then Reserve with the same
// request_id must claim the other. Fails if pick SQL ignores live occupations.
func TestBayReserve_SkipsBusyHashWinner(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.BayDSN)
	dealerID, bayA := setupBay(t, pool)
	_, bayB := setupBay(t, pool)
	_, err := pool.Exec(context.Background(),
		`UPDATE service_bay SET dealership_id = $1 WHERE id = $2`, dealerID, bayB)
	require.NoError(t, err)

	requestID := "skip-busy-bay-" + integrationtest.NewUUID(t)
	var hashWinner string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT id FROM service_bay WHERE id IN ($1, $2) ORDER BY hashtext(id::text || $3) LIMIT 1`,
		bayA, bayB, requestID).Scan(&hashWinner))

	start := integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), 3, 10, 0)
	end := start.Add(30 * time.Minute)
	srv := bay.NewServer(pool)
	first, err := srv.Reserve(context.Background(), &bayv1.ReserveRequest{
		DealershipId: dealerID,
		Start:        timestamppb.New(start),
		End:          timestamppb.New(end),
		RequestId:    requestID,
	})
	require.NoError(t, err)
	require.Equal(t, hashWinner, first.GetServiceBayId())

	second, err := srv.Reserve(context.Background(), &bayv1.ReserveRequest{
		DealershipId: dealerID,
		Start:        timestamppb.New(start),
		End:          timestamppb.New(end),
		RequestId:    requestID,
	})
	require.NoError(t, err, "second Reserve must pick the free peer, not 409 the busy hash winner")
	require.NotEqual(t, first.GetServiceBayId(), second.GetServiceBayId())
}
