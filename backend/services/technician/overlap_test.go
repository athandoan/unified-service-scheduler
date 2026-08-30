package technician_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/athandoan/unified-service-scheduler/services/technician"
	techv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/technician/v1"
	"github.com/athandoan/unified-service-scheduler/shared/pkg/integrationtest"
)

// setupTech inserts one test-owned technician (Europe/London), one skill for
// serviceTypeID with the given duration, and Mon–Fri 08:00–17:00 local shifts.
func setupTech(t *testing.T, pool *pgxpool.Pool, serviceTypeID string, skillMinutes int) (dealerID, techID string) {
	t.Helper()
	ctx := context.Background()
	dealerID, techID = integrationtest.NewUUID(t), integrationtest.NewUUID(t)

	_, err := pool.Exec(ctx,
		`INSERT INTO technician (id, dealership_id, active, timezone) VALUES ($1, $2, true, 'Europe/London')`,
		techID, dealerID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO technician_skill (technician_id, service_type_id, duration_minutes) VALUES ($1, $2, $3)`,
		techID, serviceTypeID, skillMinutes)
	require.NoError(t, err)
	for wd := 1; wd <= 5; wd++ {
		_, err = pool.Exec(ctx,
			`INSERT INTO technician_shift (technician_id, weekday, start_minutes, end_minutes) VALUES ($1, $2, 480, 1020)`,
			techID, wd)
		require.NoError(t, err)
	}
	integrationtest.CleanupTable(t, pool, "technician", "id", techID)
	return dealerID, techID
}

// Two concurrent Reserves on one qualified technician, same overlapping future
// interval: exactly one wins, one gets FailedPrecondition, and exactly one live
// HELD occupation exists with the ~120s hold TTL. Removing the GiST exclusion
// makes both Reserves succeed and this test fail.
func TestTechnicianReserve_SameTechOverlap_OnlyOneWins(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.TechnicianDSN)
	serviceTypeID := integrationtest.NewUUID(t)
	dealerID, _ := setupTech(t, pool, serviceTypeID, 30)

	start := integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), 1, 10, 0) // Monday 10:00 Europe/London, future
	latestEnd := start.Add(2 * time.Hour)

	srv := technician.NewServer(pool)
	ctx := context.Background()

	const n = 2
	results := make([]*techv1.ReserveResponse, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate // release both goroutines together: concurrent, not serialized
			results[i], errs[i] = srv.Reserve(ctx, &techv1.ReserveRequest{
				DealershipId:  dealerID,
				ServiceTypeId: serviceTypeID,
				Start:         timestamppb.New(start),
				LatestEnd:     timestamppb.New(latestEnd),
				RequestId:     "overlap-same-tech-" + integrationtest.NewUUID(t),
			})
		}(i)
	}
	close(gate)
	wg.Wait()

	wins := 0
	var winnerOccID, winnerTechID string
	for i := 0; i < n; i++ {
		if errs[i] == nil {
			wins++
			winnerOccID, winnerTechID = results[i].GetOccupationId(), results[i].GetTechnicianId()
		} else {
			t.Logf("tech goroutine %d lost the race (expected for exactly one): %v", i, errs[i])
		}
	}
	require.Equal(t, 1, wins, "exactly one concurrent Reserve must win on one tech")

	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT 1 FROM technician WHERE id = $1`, winnerTechID).Scan(new(any)),
		"winner occupation must point at the seeded tech")

	var status string
	var rowCount int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status FROM tech_occupation WHERE id = $1`, winnerOccID).Scan(&status))
	require.Equal(t, "HELD", status)
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT count(*) FROM tech_occupation
		WHERE technician_id = $1 AND status IN ('HELD', 'CONFIRMED')
		  AND tstzrange(start_at, end_at, '[)') && tstzrange($2::timestamptz, $2::timestamptz + interval '30 minutes', '[)')`,
		winnerTechID, start).Scan(&rowCount))
	require.Equal(t, 1, rowCount, "exactly one live occupation on this tech in the interval")

	// TTL: hold_expires_at ≈ now + 120s (fixed in SQL, no env override).
	var holdExpiry time.Time
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT hold_expires_at FROM tech_occupation WHERE id = $1`, winnerOccID).Scan(&holdExpiry))
	require.WithinDuration(t, time.Now().Add(120*time.Second), holdExpiry, 15*time.Second)
}

// Lifecycle: a second Reserve overlapping a CONFIRMED occupation must fail
// (GiST predicate includes CONFIRMED); after Release the slot is free again.
func TestTechnicianReserve_ConfirmBlocksOverlap_ReleaseFrees(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.TechnicianDSN)
	serviceTypeID := integrationtest.NewUUID(t)
	dealerID, _ := setupTech(t, pool, serviceTypeID, 30)

	zone := integrationtest.TestZone(t)
	start := integrationtest.NextWeekdayAt(t, zone, 2, 11, 0) // Tuesday 10:00 local
	latestEnd := start.Add(2 * time.Hour)

	srv := technician.NewServer(pool)
	ctx := context.Background()
	req := &techv1.ReserveRequest{
		DealershipId:  dealerID,
		ServiceTypeId: serviceTypeID,
		Start:         timestamppb.New(start),
		LatestEnd:     timestamppb.New(latestEnd),
		RequestId:     "lifecycle-" + integrationtest.NewUUID(t),
	}
	first, err := srv.Reserve(ctx, req)
	require.NoError(t, err)

	// Confirm in place: HELD → CONFIRMED, no second claim.
	_, err = srv.Confirm(ctx, &techv1.ConfirmRequest{OccupationId: first.GetOccupationId()})
	require.NoError(t, err)

	var confirmed string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT status FROM tech_occupation WHERE id = $1`, first.GetOccupationId()).Scan(&confirmed))
	require.Equal(t, "CONFIRMED", confirmed)

	// Overlapping Reserve on the same tech fails even after confirm.
	_, err = srv.Reserve(ctx, &techv1.ReserveRequest{
		DealershipId:  dealerID,
		ServiceTypeId: serviceTypeID,
		Start:         timestamppb.New(start.Add(15 * time.Minute)),
		LatestEnd:     timestamppb.New(latestEnd),
		RequestId:     "overlap-after-confirm-" + integrationtest.NewUUID(t),
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "FailedPrecondition")

	// Release drops the claim out of the GiST predicate; the slot is rebookable.
	_, err = srv.Release(ctx, &techv1.ReleaseRequest{OccupationId: first.GetOccupationId()})
	require.NoError(t, err)
	again, err := srv.Reserve(ctx, req)
	require.NoError(t, err, "Reserve after Release must succeed")
	require.NotEqual(t, first.GetOccupationId(), again.GetOccupationId())
}

// Confirm is idempotent: confirming the same occupation twice must succeed
// both times (the second is a no-op on the already-CONFIRMED row), and the row
// must end CONFIRMED. Fails if Confirm treats CONFIRMED as FailedPrecondition.
func TestTechnicianConfirm_Twice_IdempotentSuccess(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.TechnicianDSN)
	serviceTypeID := integrationtest.NewUUID(t)
	dealerID, _ := setupTech(t, pool, serviceTypeID, 30)

	start := integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), 4, 9, 0) // Thursday 09:00 local
	latestEnd := start.Add(2 * time.Hour)

	srv := technician.NewServer(pool)
	ctx := context.Background()
	first, err := srv.Reserve(ctx, &techv1.ReserveRequest{
		DealershipId:  dealerID,
		ServiceTypeId: serviceTypeID,
		Start:         timestamppb.New(start),
		LatestEnd:     timestamppb.New(latestEnd),
		RequestId:     "confirm-twice-" + integrationtest.NewUUID(t),
	})
	require.NoError(t, err)

	occID := first.GetOccupationId()
	_, err = srv.Confirm(ctx, &techv1.ConfirmRequest{OccupationId: occID})
	require.NoError(t, err, "first Confirm of HELD must succeed")
	_, err = srv.Confirm(ctx, &techv1.ConfirmRequest{OccupationId: occID})
	require.NoError(t, err, "second Confirm of CONFIRMED must be a no-op success, not FailedPrecondition")

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM tech_occupation WHERE id = $1`, occID).Scan(&status))
	require.Equal(t, "CONFIRMED", status)
}

// Release is idempotent: releasing twice must succeed both times, and releasing
// an unknown occupation id must also succeed (no-op). Fails if Release 409s on
// the second call or on an unknown id.
func TestTechnicianRelease_TwiceAndUnknown_NoOpSuccess(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.TechnicianDSN)
	serviceTypeID := integrationtest.NewUUID(t)
	dealerID, _ := setupTech(t, pool, serviceTypeID, 30)

	start := integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), 4, 11, 0) // Thursday 11:00 local
	latestEnd := start.Add(2 * time.Hour)

	srv := technician.NewServer(pool)
	ctx := context.Background()
	first, err := srv.Reserve(ctx, &techv1.ReserveRequest{
		DealershipId:  dealerID,
		ServiceTypeId: serviceTypeID,
		Start:         timestamppb.New(start),
		LatestEnd:     timestamppb.New(latestEnd),
		RequestId:     "release-twice-" + integrationtest.NewUUID(t),
	})
	require.NoError(t, err)

	occID := first.GetOccupationId()
	_, err = srv.Release(ctx, &techv1.ReleaseRequest{OccupationId: occID})
	require.NoError(t, err, "first Release of HELD must succeed")
	_, err = srv.Release(ctx, &techv1.ReleaseRequest{OccupationId: occID})
	require.NoError(t, err, "second Release of RELEASED must be a no-op success, not FailedPrecondition")

	// Unknown occupation id: Release is a no-op success too.
	_, err = srv.Release(ctx, &techv1.ReleaseRequest{OccupationId: integrationtest.NewUUID(t)})
	require.NoError(t, err, "Release of an unknown occupation id must be a no-op success")

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM tech_occupation WHERE id = $1`, occID).Scan(&status))
	require.Equal(t, "RELEASED", status)
}

// Confirm of a RELEASED occupation must fail with FailedPrecondition — the
// claim was dropped and must not be resurrected in place.
func TestTechnicianConfirm_Released_Fails(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.TechnicianDSN)
	serviceTypeID := integrationtest.NewUUID(t)
	dealerID, _ := setupTech(t, pool, serviceTypeID, 30)

	start := integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), 5, 9, 0) // Friday 09:00 local
	latestEnd := start.Add(2 * time.Hour)

	srv := technician.NewServer(pool)
	ctx := context.Background()
	first, err := srv.Reserve(ctx, &techv1.ReserveRequest{
		DealershipId:  dealerID,
		ServiceTypeId: serviceTypeID,
		Start:         timestamppb.New(start),
		LatestEnd:     timestamppb.New(latestEnd),
		RequestId:     "confirm-released-" + integrationtest.NewUUID(t),
	})
	require.NoError(t, err)

	occID := first.GetOccupationId()
	_, err = srv.Release(ctx, &techv1.ReleaseRequest{OccupationId: occID})
	require.NoError(t, err)

	_, err = srv.Confirm(ctx, &techv1.ConfirmRequest{OccupationId: occID})
	require.Error(t, err, "Confirm of a RELEASED occupation must fail")
	require.Contains(t, err.Error(), "FailedPrecondition")

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM tech_occupation WHERE id = $1`, occID).Scan(&status))
	require.Equal(t, "RELEASED", status)
}

// Confirm of an expired HELD must fail with FailedPrecondition and must not
// confirm it as a side effect: backdate hold_expires_at, Confirm fails, and the
// row stays HELD (never CONFIRMED).
func TestTechnicianConfirm_ExpiredHold_FailsAndStaysHeld(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.TechnicianDSN)
	serviceTypeID := integrationtest.NewUUID(t)
	dealerID, _ := setupTech(t, pool, serviceTypeID, 30)

	start := integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), 5, 11, 0) // Friday 11:00 local
	latestEnd := start.Add(2 * time.Hour)

	srv := technician.NewServer(pool)
	ctx := context.Background()
	first, err := srv.Reserve(ctx, &techv1.ReserveRequest{
		DealershipId:  dealerID,
		ServiceTypeId: serviceTypeID,
		Start:         timestamppb.New(start),
		LatestEnd:     timestamppb.New(latestEnd),
		RequestId:     "confirm-expired-" + integrationtest.NewUUID(t),
	})
	require.NoError(t, err)

	occID := first.GetOccupationId()
	_, err = pool.Exec(ctx,
		`UPDATE tech_occupation SET hold_expires_at = now() - interval '1 second' WHERE id = $1`,
		occID)
	require.NoError(t, err)

	_, err = srv.Confirm(ctx, &techv1.ConfirmRequest{OccupationId: occID})
	require.Error(t, err, "Confirm of an expired HELD must fail")
	require.Contains(t, err.Error(), "FailedPrecondition")

	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM tech_occupation WHERE id = $1`, occID).Scan(&status))
	require.Equal(t, "HELD", status, "expired HELD must not become CONFIRMED")
}

// Two eligible techs: occupy the hash LIMIT 1 winner, then Reserve with the
// same request_id must claim the other. Fails if pick SQL ignores live occupations.
func TestTechnicianReserve_SkipsBusyHashWinner(t *testing.T) {
	pool := integrationtest.Connect(t, integrationtest.TechnicianDSN)
	serviceTypeID := integrationtest.NewUUID(t)
	dealerID, techA := setupTech(t, pool, serviceTypeID, 30)
	_, techB := setupTech(t, pool, serviceTypeID, 30)
	_, err := pool.Exec(context.Background(),
		`UPDATE technician SET dealership_id = $1 WHERE id = $2`, dealerID, techB)
	require.NoError(t, err)

	requestID := "skip-busy-" + integrationtest.NewUUID(t)
	var hashWinner string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT id FROM technician WHERE id IN ($1, $2) ORDER BY hashtext(id::text || $3) LIMIT 1`,
		techA, techB, requestID).Scan(&hashWinner))

	start := integrationtest.NextWeekdayAt(t, integrationtest.TestZone(t), 3, 10, 0)
	latestEnd := start.Add(2 * time.Hour)
	srv := technician.NewServer(pool)
	first, err := srv.Reserve(context.Background(), &techv1.ReserveRequest{
		DealershipId:  dealerID,
		ServiceTypeId: serviceTypeID,
		Start:         timestamppb.New(start),
		LatestEnd:     timestamppb.New(latestEnd),
		RequestId:     requestID,
	})
	require.NoError(t, err)
	require.Equal(t, hashWinner, first.GetTechnicianId())

	second, err := srv.Reserve(context.Background(), &techv1.ReserveRequest{
		DealershipId:  dealerID,
		ServiceTypeId: serviceTypeID,
		Start:         timestamppb.New(start),
		LatestEnd:     timestamppb.New(latestEnd),
		RequestId:     requestID,
	})
	require.NoError(t, err, "second Reserve must pick the free peer, not 409 the busy hash winner")
	require.NotEqual(t, first.GetTechnicianId(), second.GetTechnicianId())
}
