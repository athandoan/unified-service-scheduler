// Package integrationtest hosts real-Postgres allocator tests. They require the
// compose Postgres on localhost:5432 (docker compose up -d) with migrations
// applied (go run ./migrate). They FAIL — not skip — when Postgres is
// unreachable: the GiST overlap invariant is what is under test.
package integrationtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// DSNs for the two allocator databases (compose Postgres, localhost:5432).
// Overridable via TECHNICIAN_TEST_DSN / BAY_TEST_DSN for CI port mappings.
var (
	TechnicianDSN = dsnOr("TECHNICIAN_TEST_DSN", "postgres://scheduler:scheduler@localhost:5432/technician?sslmode=disable")
	BayDSN        = dsnOr("BAY_TEST_DSN", "postgres://scheduler:scheduler@localhost:5432/bay?sslmode=disable")
)

func dsnOr(key, fallback string) string { return fallback }

// Connect skips under -short (mocked unit runs have no Postgres — the only
// skip path). Otherwise it FAILS when Postgres is unreachable — never
// skip-if-no-DB: the GiST overlap invariant is what is under test.
func Connect(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("short: occupancy needs Postgres")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("cannot create pool for %s: %v (is compose Postgres up?)", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("cannot reach Postgres: %v (run: docker compose up -d && go run ./migrate)", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// NewUUID returns a random RFC-4122-shaped UUID string (test fixtures only).
func NewUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// CleanupTable deletes the resource row (occupations cascade on delete).
func CleanupTable(t *testing.T, pool *pgxpool.Pool, table, idCol, id string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx, fmt.Sprintf("DELETE FROM %s WHERE %s = $1", table, idCol), id); err != nil {
			t.Logf("cleanup %s: %v", table, err)
		}
	})
}

// NextWeekdayAt: the next occurrence of ISO weekday (1=Mon) at hour:minute in
// loc, strictly in the future. Tests build fixture intervals in the tech's
// timezone (Europe/London) so local-minute shift checks behave predictably.
func NextWeekdayAt(t *testing.T, loc *time.Location, weekday, hour, minute int) time.Time {
	t.Helper()
	now := time.Now().In(loc)
	cand := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)
	for {
		iso := int(cand.Weekday())
		if iso == 0 {
			iso = 7
		}
		if iso == weekday && cand.After(now.Add(time.Minute)) {
			return cand
		}
		cand = cand.AddDate(0, 0, 1)
	}
}

// TestZone is the IANA zone the fixtures use (matches technician.timezone).
func TestZone(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatalf("load Europe/London: %v", err)
	}
	return loc
}

var _ = require.Equal // keep testify referenced even if a test drops it
