// The three public occupancy tests (lock, Phase 3). All run the public HTTP
// saga (POST /appointments) concurrently against real Postgres + real GiST.
package booking_test

import (
	"context"
	"github.com/athandoan/unified-service-scheduler/shared/pkg/integrationtest"
	"github.com/stretchr/testify/require"
	"testing"
)

// TestSameTechOverlap_PublicHTTP: 1 tech, >=2 bays, same start, concurrent.
// Exactly one 201; the loser 409s at Technician GiST; one live occupation on
// that tech. Two bays keep Bay from masking a dropped technician GiST: with a
// single bay both racers would collide on Bay first and the test could pass
// even if the technician EXCLUDE were gone.
func TestSameTechOverlap_PublicHTTP_OnlyOneConfirms(t *testing.T) {
	e := newEnv(t, 1, 2) // 1 tech, >=2 bays so Bay cannot mask a tech GiST drop
	start := integrationtest.NextWeekdayAt(t, e.zone, 1, 10, 0)
	codes := concurrentConfirms(t, e, 2, start)
	wins := 0
	for _, c := range codes {
		if c == 201 {
			wins++
		} else {
			require.Equal(t, 409, c, "loser must be 409, got %d", c)
		}
	}
	require.Equal(t, 1, wins, "exactly one confirm must win on one tech")
	// Exactly one live occupation on that tech.
	var live int
	require.NoError(t, e.techPool.QueryRow(context.Background(), `
		SELECT count(*) FROM tech_occupation o
		JOIN technician t ON t.id = o.technician_id
		WHERE t.dealership_id = $1 AND o.status IN ('HELD','CONFIRMED')`,
		e.dealerID).Scan(&live))
	require.Equal(t, 1, live, "exactly one live tech occupation after the race")
}

// TestSameBayOverlap_PublicHTTP: >=2 techs, 1 bay, same start, concurrent.
// The tech loser loses at Bay (two qualified techs, one bay): exactly one 201,
// the other 409 with a dead Bay claim. Removing Bay GiST fails this test —
// both would confirm on different techs.
func TestSameBayOverlap_PublicHTTP_OnlyOneConfirms(t *testing.T) {
	e := newEnv(t, 2, 1) // 2 techs, 1 bay
	start := integrationtest.NextWeekdayAt(t, e.zone, 2, 11, 0)
	codes := concurrentConfirms(t, e, 2, start)
	wins := 0
	for _, c := range codes {
		if c == 201 {
			wins++
		} else {
			require.Equal(t, 409, c, "loser must be 409, got %d", c)
		}
	}
	require.Equal(t, 1, wins, "exactly one confirm must win on one bay")
	// Exactly one live bay occupation — the invariant that dies without Bay GiST.
	var live int
	require.NoError(t, e.bayPool.QueryRow(context.Background(), `
		SELECT count(*) FROM bay_occupation o
		JOIN service_bay b ON b.id = o.service_bay_id
		WHERE b.dealership_id = $1 AND o.status IN ('HELD','CONFIRMED')`,
		e.dealerID).Scan(&live))
	require.Equal(t, 1, live, "exactly one live bay occupation after the race")
}

// TestCompensate_BayFailReleasesTech: >=1 tech, 0 bays. POST /appointments
// 409s at Bay; the technician DB must show the tech occupation RELEASED (or
// absent from HELD|CONFIRMED) — the saga compensated.
func TestCompensate_BayFailReleasesTech(t *testing.T) {
	e := newEnv(t, 1, 0) // 1 tech, 0 bays
	start := integrationtest.NextWeekdayAt(t, e.zone, 3, 9, 0)
	code, _ := e.post(t, "/appointments", e.confirmBody(start), map[string]string{
		"Idempotency-Key": "compensate-" + integrationtest.NewUUID(t),
		"X-Request-Id":    integrationtest.NewUUID(t),
	})
	require.Equal(t, 409, code, "no bay: Booking must 409")
	// Read the TECHNICIAN DB: the reservation must have been compensated.
	var live int
	require.NoError(t, e.techPool.QueryRow(context.Background(), `
		SELECT count(*) FROM tech_occupation o
		JOIN technician t ON t.id = o.technician_id
		WHERE t.dealership_id = $1 AND o.status IN ('HELD','CONFIRMED')`,
		e.dealerID).Scan(&live))
	require.Zero(t, live, "tech occupation must be RELEASED (not HELD|CONFIRMED) after bay fail")
}
