// Package bay implements the Bay allocator: Reserve / Confirm / Release over
// the bay Postgres write primary (SYSTEM_DESIGN, write-path step 5). Bays are
// homogeneous: Reserve carries the Booking-forwarded [start, end) verbatim — no
// skill, no latest_end, no timezone. The GiST exclusion on bay_occupation is
// the race authority; this package never calls Catalog.
//
// Error mapping (documented for Phase 3): every Reserve failure — no free bay
// or GiST contention after the retries — is codes.FailedPrecondition, which
// Booking maps to HTTP 409. Confirm of a missing, expired-HELD, or RELEASED
// occupation is FailedPrecondition (never insert a replacement); Confirm of an
// already-CONFIRMED occupation is a no-op success (idempotent). Release is
// idempotent: an unknown or already-RELEASED occupation is a no-op success.
package bay

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	bayv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/bay/v1"
)

const (
	// HoldTTL is allocator-owned: 120 seconds, fixed in the claim SQL. No env
	// override; reaper tests backdate hold_expires_at instead.
	HoldTTL = "120 seconds"

	// maxReserveAttempts is the total claim attempts. On GiST exclusion
	// violation (SQLSTATE 23P01) this allocator deletes its own expired HELD
	// rows and retries; Booking never runs that SQL.
	maxReserveAttempts = 3

	// exclusionViolation is SQLSTATE 23P01: exclusion_constraint_violation.
	exclusionViolation = "23P01"
)

// ErrUnavailable: no active bay fits or every candidate lost the GiST race
// after the retries.
var ErrUnavailable = errors.New("no free bay for the interval")

// Server implements bayv1.BayServiceServer against one write primary.
type Server struct {
	bayv1.UnimplementedBayServiceServer
	pool *pgxpool.Pool
}

// NewServer builds the allocator server on a bay-DB pool.
func NewServer(pool *pgxpool.Pool) *Server { return &Server{pool: pool} }

// Reserve picks one free homogeneous bay for the Booking-forwarded [start, end)
// in a single INSERT…SELECT on the write primary.
func (s *Server) Reserve(ctx context.Context, req *bayv1.ReserveRequest) (*bayv1.ReserveResponse, error) {
	if req.GetDealershipId() == "" || req.GetStart() == nil || req.GetEnd() == nil {
		return nil, status.Error(codes.InvalidArgument, "dealership_id, start, end are required")
	}
	start, end := req.GetStart().AsTime(), req.GetEnd().AsTime()
	if !end.After(start) {
		return nil, status.Error(codes.InvalidArgument, "end must be after start")
	}
	if !start.After(time.Now()) {
		return nil, status.Error(codes.InvalidArgument, "start must be strictly in the future")
	}

	occ, err := reserve(ctx, s.pool, reserveParams{
		dealershipID: req.GetDealershipId(),
		start:        start,
		end:          end,
		requestID:    req.GetRequestId(),
	})
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Error(codes.Internal, "reserve failed")
	}
	return &bayv1.ReserveResponse{
		OccupationId: occ.ID,
		ServiceBayId: occ.ResourceID,
	}, nil
}

// Confirm promotes HELD → CONFIRMED in place if unexpired, and is idempotent:
// an already-CONFIRMED occupation is a no-op success. Missing, expired HELD,
// or RELEASED → FailedPrecondition. Never inserts a replacement occupation and
// never confirms an expired HELD as a side effect.
func (s *Server) Confirm(ctx context.Context, req *bayv1.ConfirmRequest) (*bayv1.ConfirmResponse, error) {
	if req.GetOccupationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "occupation_id is required")
	}
	// One statement covers both cases: the unexpired-HELD arm promotes the
	// hold; the CONFIRMED arm re-asserts the same state so a retry (double
	// Confirm, redelivered request) succeeds as a no-op. Expired HELD and
	// RELEASED rows match neither arm, so they still fail below.
	tag, err := s.pool.Exec(ctx, `
		UPDATE bay_occupation
		SET status = 'CONFIRMED', hold_expires_at = NULL
		WHERE id = $1 AND (
		  (status = 'HELD' AND (hold_expires_at IS NULL OR hold_expires_at > now()))
		  OR status = 'CONFIRMED'
		)`,
		req.GetOccupationId())
	if err != nil {
		return nil, status.Error(codes.Internal, "confirm requires an unexpired HELD occupation")
	}
	if tag.RowsAffected() == 0 {
		return nil, status.Error(codes.FailedPrecondition, "occupation not held or hold expired")
	}
	return &bayv1.ConfirmResponse{}, nil
}

// Release sets RELEASED (outside the GiST predicate), dropping the claim, and
// is idempotent: an unknown occupation id or an already-RELEASED row is a
// no-op success, so client retries and redeliveries never 409.
func (s *Server) Release(ctx context.Context, req *bayv1.ReleaseRequest) (*bayv1.ReleaseResponse, error) {
	if req.GetOccupationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "occupation_id is required")
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE bay_occupation SET status = 'RELEASED'
		WHERE id = $1 AND status IN ('HELD', 'CONFIRMED')`,
		req.GetOccupationId())
	if err != nil {
		return nil, status.Error(codes.Internal, "release failed")
	}
	// 0 rows = missing id or already RELEASED: both are no-op successes.
	return &bayv1.ReleaseResponse{}, nil
}

// Occupation is one claimed interval on a resource.
type Occupation struct {
	ID         string
	ResourceID string // service_bay_id
	End        time.Time
}

// reserveParams carries one Reserve claim.
type reserveParams struct {
	dealershipID string
	start        time.Time
	end          time.Time
	requestID    string
}

// reserve claims one bay: one INSERT…SELECT, on the write primary. GiST is
// the race authority; NOT EXISTS overlapping HELD|CONFIRMED skips a busy
// hash-winner so a free peer can be picked. Concurrent losers get 23P01,
// reap expired HELD rows, and retry.
func reserve(ctx context.Context, pool *pgxpool.Pool, p reserveParams) (Occupation, error) {
	// $1 dealership, $2 start, $3 end, $4 request_id. Empty pick → no insert →
	// RETURNING yields pgx.ErrNoRows. The race authority is the GiST exclusion:
	// two concurrent claims of one bay race at the constraint, not in SELECT.
	const claimSQL = `
	INSERT INTO bay_occupation
		(id, service_bay_id, start_at, end_at, status, hold_expires_at)
	SELECT gen_random_uuid(), picked.service_bay_id, picked.start_at, picked.end_at, 'HELD', now() + interval '120 seconds'
	FROM (
		SELECT b.id AS service_bay_id, $2::timestamptz AS start_at, $3::timestamptz AS end_at
		FROM service_bay b
		WHERE b.dealership_id = $1
		  AND b.active
		  AND NOT EXISTS (
			SELECT 1 FROM bay_occupation o
			WHERE o.service_bay_id = b.id
			  AND o.status IN ('HELD', 'CONFIRMED')
			  AND tstzrange(o.start_at, o.end_at, '[)') && tstzrange($2::timestamptz, $3::timestamptz, '[)')
		  )
		ORDER BY hashtext(b.id::text || $4)
		LIMIT 1
	) AS picked
	RETURNING id, service_bay_id, end_at`

	var occ Occupation
	for attempt := 1; attempt <= maxReserveAttempts; attempt++ {
		err := pool.QueryRow(ctx, claimSQL,
			p.dealershipID, p.start, p.end, p.requestID,
		).Scan(&occ.ID, &occ.ResourceID, &occ.End)
		if err == nil {
			return occ, nil
		}
		var pgErr *pgconn.PgError
		empty := errors.Is(err, pgx.ErrNoRows)
		collision := errors.As(err, &pgErr) && pgErr.Code == exclusionViolation
		if empty || collision {
			if _, derr := pool.Exec(ctx,
				`DELETE FROM bay_occupation WHERE status = 'HELD' AND hold_expires_at <= now()`); derr != nil {
				return Occupation{}, ErrUnavailable
			}
			continue
		}
		return Occupation{}, err
	}
	return Occupation{}, ErrUnavailable
}
