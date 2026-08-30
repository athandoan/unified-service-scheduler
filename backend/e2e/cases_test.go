//go:build e2e

// The five Phase-1 cases. All HTTP goes through the Gateway on :8080; all
// seeding is test-owned on :5432 with cleanup. The fixture dealer
// 11111111-... (North Workshop) is GET-only: no tech/bay rows are ever added
// to it. The concurrency case does not assert occupation-table counts — the
// occupancy suite (go test ./...) owns those.
package e2e

import (
	"fmt"
	"sync"
	"testing"

	"github.com/athandoan/unified-service-scheduler/shared/pkg/integrationtest"
	"github.com/stretchr/testify/require"
)

// Test1_ListDealerships: GET /dealerships via the Gateway returns 200 and the
// North Workshop fixture. No tech/bay inserts — read-only, no seeding at all.
func Test1_ListDealerships(t *testing.T) {
	code, dealers := getList(t, "/dealerships")
	require.Equal(t, 200, code)
	found := false
	for _, d := range dealers {
		if d["id"] == fixtureDealerID {
			found = true
			require.Equal(t, "North Workshop", d["name"], "fixture dealer name must match")
		}
	}
	require.True(t, found, "GET /dealerships must include North Workshop %s", fixtureDealerID)
}

// Test2_ConfirmHappyPath: test-owned dealer (new UUID) + weekday hours + 1
// tech (timezone copied from dealer, skill for the oil change, shifts covering
// the interval) + 1 bay. POST /appointments (confirm-without-hold,
// Idempotency-Key) → 201 CONFIRMED; GET /appointments/{id} → 200.
func Test2_ConfirmHappyPath(t *testing.T) {
	topo := seedTopology(t, 1, 1) // 1 tech, >=1 bay
	start := futureStart(t, 1)    // Monday 10:00 local, strictly future, inside hours

	code, body := post(t, "/appointments", confirmBody(topo, start), map[string]string{
		"Idempotency-Key": "e2e-confirm-" + integrationtest.NewUUID(t),
	})
	require.Equal(t, 201, code, "confirm-without-hold must create: %s", body)
	require.Equal(t, "CONFIRMED", body["status"])
	apptID, _ := body["id"].(string)
	require.NotEmpty(t, apptID, "appointment id required")

	// end_at comes from the tech's 30-minute skill duration.
	require.Equal(t, start.UTC().Format("2006-01-02T15:04:05Z07:00"), body["start_at"])

	code, got := get(t, "/appointments/"+apptID)
	require.Equal(t, 200, code)
	require.Equal(t, "CONFIRMED", got["status"])
	require.Equal(t, fixtureCustomerID, got["customerId"])
	require.Equal(t, fixtureVehicleID, got["vehicleId"])
	require.Equal(t, fixtureServiceTypeID, got["serviceTypeId"])
	require.Equal(t, topo.dealerID, got["dealershipId"])
	require.NotEmpty(t, got["technicianId"], "Booking assigns the technician")
	require.NotEmpty(t, got["serviceBayId"], "Booking assigns the bay")
}

// Test3_HoldsIdempotencyKeyRules: 1 tech / 1 bay topology. POST /holds without
// Idempotency-Key → 201 HELD; POST /holds with the header → 400 (holds are
// not idempotent; the key is confirm-only).
func Test3_HoldsIdempotencyKeyRules(t *testing.T) {
	topo := seedTopology(t, 1, 1)

	// Without the header: 201 HELD.
	start := futureStart(t, 2)
	code, body := post(t, "/holds", confirmBody(topo, start), nil)
	require.Equal(t, 201, code, "hold without Idempotency-Key must be created: %s", body)
	require.Equal(t, "HELD", body["status"])
	require.NotNil(t, body["hold_expires_at"], "HELD carries an expiry")

	// With the header: 400 — no saga runs, no resources are claimed.
	start2 := futureStart(t, 3)
	code, body = post(t, "/holds", confirmBody(topo, start2), map[string]string{
		"Idempotency-Key": "e2e-hold-" + integrationtest.NewUUID(t),
	})
	require.Equal(t, 400, code, "hold with Idempotency-Key must be rejected: %s", body)
}

// Test4_ConcurrentConfirm_OnlyOneWins: 1 tech / 1 bay, two concurrent POST
// /appointments at the same start → exactly one 201, the other 409. No
// occupation-table assertions here — the occupancy suite owns that invariant.
func Test4_ConcurrentConfirm_OnlyOneWins(t *testing.T) {
	topo := seedTopology(t, 1, 1)
	start := futureStart(t, 4)
	body := confirmBody(topo, start)

	const n = 2
	type result struct {
		code int
		raw  string
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	gate := make(chan struct{}) // fire together — concurrent, not serialized
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			code, raw, err := rawPost("/appointments", body, map[string]string{
				"Idempotency-Key": fmt.Sprintf("e2e-race-%s-%d", integrationtest.NewUUID(t), i),
			})
			if err != nil {
				results[i] = result{code: -1, raw: err.Error()}
				return
			}
			results[i] = result{code: code, raw: string(raw)}
		}(i)
	}
	close(gate)
	wg.Wait()

	wins := 0
	for _, r := range results {
		if r.code == 201 {
			wins++
			continue
		}
		require.Equal(t, 409, r.code, "loser must be 409, got %d (%s)", r.code, r.raw)
	}
	require.Equal(t, 1, wins, "exactly one confirm must win on 1 tech / 1 bay")
}

// Test5_NoBaysConflict: >=1 tech, 0 bays on a test-owned dealer. POST
// /appointments → 409 only. No technician-DB read — the RELEASED check is the
// occupancy suite's compensate test, not this one.
func Test5_NoBaysConflict(t *testing.T) {
	topo := seedTopology(t, 1, 0) // 1 tech, 0 bays
	start := futureStart(t, 5)

	code, body := post(t, "/appointments", confirmBody(topo, start), map[string]string{
		"Idempotency-Key": "e2e-nobay-" + integrationtest.NewUUID(t),
	})
	require.Equal(t, 409, code, "no bays: Booking must 409: %s", body)
}