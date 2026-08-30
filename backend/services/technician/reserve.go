// Package technician implements the Technician allocator: Reserve / Confirm /
// Release over the technician Postgres (SYSTEM_DESIGN, write-path steps 4 and
// 7). The GiST exclusion on tech_occupation is the race authority; this package
// never calls Catalog and never reads the Booking DB.
//
// Error mapping (documented for Phase 3): every Reserve failure — no candidate
// fit or GiST contention after the retries — is codes.FailedPrecondition, which
// Booking maps to HTTP 409. Confirm of a missing, expired-HELD, or RELEASED
// occupation is FailedPrecondition (never insert a replacement); Confirm of an
// already-CONFIRMED occupation is a no-op success (idempotent). Release is
// idempotent: an unknown or already-RELEASED occupation is a no-op success.
package technician

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	techv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/technician/v1"
)

const (
	// HoldTTL is allocator-owned: 120 seconds, fixed in the claim SQL
	// (hold_expires_at = now() + interval '120 seconds'). No env override;
	// reaper tests backdate hold_expires_at instead.
	HoldTTL = "120 seconds"

	// maxReserveAttempts is the total claim attempts. On GiST exclusion
	// violation (SQLSTATE 23P01) this allocator deletes its own expired HELD
	// rows and retries; Booking never runs that SQL.
	maxReserveAttempts = 3

	// exclusionViolation is SQLSTATE 23P01: exclusion_constraint_violation.
	exclusionViolation = "23P01"
)

// ErrUnavailable: no active qualified technician fits (skill, shift, absence,
// latest_end) or every candidate lost the GiST race after the retries.
var ErrUnavailable = errors.New("no free qualified technician for the interval")

// Server implements techv1.TechnicianServiceServer against one write primary.
type Server struct {
	techv1.UnimplementedTechnicianServiceServer
	pool *pgxpool.Pool
}

// NewServer builds the allocator server on a technician-DB pool.
func NewServer(pool *pgxpool.Pool) *Server { return &Server{pool: pool} }

// Reserve picks one free qualified technician and holds the interval in a
// single INSERT…SELECT on the write primary.
func (s *Server) Reserve(ctx context.Context, req *techv1.ReserveRequest) (*techv1.ReserveResponse, error) {
	if req.GetDealershipId() == "" || req.GetServiceTypeId() == "" ||
		req.GetStart() == nil || req.GetLatestEnd() == nil {
		return nil, status.Error(codes.InvalidArgument,
			"dealership_id, service_type_id, start, latest_end are required")
	}
	start := req.GetStart().AsTime()
	if !start.After(time.Now()) {
		return nil, status.Error(codes.InvalidArgument, "start must be strictly in the future")
	}

	occ, err := reserve(ctx, s.pool, reserveParams{
		dealershipID:  req.GetDealershipId(),
		serviceTypeID: req.GetServiceTypeId(),
		start:         start,
		latestEnd:     req.GetLatestEnd().AsTime(),
		requestID:     req.GetRequestId(),
	})
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
		return nil, status.Error(codes.Internal, "reserve failed")
	}
	return &techv1.ReserveResponse{
		OccupationId: occ.ID,
		TechnicianId: occ.ResourceID,
		End:          timestamppb.New(occ.End), // skill-derived, never a client field
	}, nil
}

// Confirm promotes HELD → CONFIRMED in place if unexpired, and is idempotent:
// an already-CONFIRMED occupation is a no-op success. Missing, expired HELD,
// or RELEASED → FailedPrecondition. Never inserts a replacement occupation and
// never confirms an expired HELD as a side effect.
func (s *Server) Confirm(ctx context.Context, req *techv1.ConfirmRequest) (*techv1.ConfirmResponse, error) {
	if req.GetOccupationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "occupation_id is required")
	}
	// One statement covers both cases: the unexpired-HELD arm promotes the
	// hold; the CONFIRMED arm re-asserts the same state so a retry (double
	// Confirm, redelivered request) succeeds as a no-op. Expired HELD and
	// RELEASED rows match neither arm, so they still fail below.
	tag, err := s.pool.Exec(ctx, `
		UPDATE tech_occupation
		SET status = 'CONFIRMED', hold_expires_at = NULL
		WHERE id = $1 AND (
		  (status = 'HELD' AND (hold_expires_at IS NULL OR hold_expires_at > now()))
		  OR status = 'CONFIRMED'
		)`,
		req.GetOccupationId())
	if err != nil {
		return nil, status.Error(codes.Internal, "confirm failed")
	}
	if tag.RowsAffected() == 0 {
		return nil, status.Error(codes.FailedPrecondition, "occupation not held or hold expired")
	}
	return &techv1.ConfirmResponse{}, nil
}

// Release sets RELEASED (outside the GiST predicate), dropping the claim, and
// is idempotent: an unknown occupation id or an already-RELEASED row is a
// no-op success, so client retries and redeliveries never 409.
func (s *Server) Release(ctx context.Context, req *techv1.ReleaseRequest) (*techv1.ReleaseResponse, error) {
	if req.GetOccupationId() == "" {
		return nil, status.Error(codes.InvalidArgument, "occupation_id is required")
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE tech_occupation SET status = 'RELEASED'
		WHERE id = $1 AND status IN ('HELD', 'CONFIRMED')`,
		req.GetOccupationId())
	if err != nil {
		return nil, status.Error(codes.Internal, "release failed")
	}
	// 0 rows = missing id or already RELEASED: both are no-op successes.
	return &techv1.ReleaseResponse{}, nil
}

// Occupation is one claimed interval on a resource.
type Occupation struct {
	ID         string
	ResourceID string // technician_id
	End        time.Time
}

// reserveParams carries one Reserve claim.
type reserveParams struct {
	dealershipID  string
	serviceTypeID string
	start         time.Time
	latestEnd     time.Time
	requestID     string
}

// reserve claims one technician: one INSERT…SELECT, one transaction, on the
// write primary. Concurrent losers get 23P01 from the GiST constraint — the
// race authority — reap expired HELD rows, and retry.
func reserve(ctx context.Context, pool *pgxpool.Pool, p reserveParams) (Occupation, error) {
	// $1 dealership, $2 service_type, $3 start, $4 latest_end, $5 request_id.
	// The CTE picks the candidate; the INSERT…SELECT claims it. If the CTE is
	// empty, no row is inserted and RETURNING yields pgx.ErrNoRows.
	const claimSQL = `
	INSERT INTO tech_occupation
		(id, technician_id, start_at, end_at, status, hold_expires_at)
	SELECT gen_random_uuid(), picked.technician_id, picked.start_at, picked.end_at, 'HELD', now() + interval '120 seconds'
	FROM (
		SELECT t.id AS technician_id,
		       $3::timestamptz AS start_at,
		       $3::timestamptz + make_interval(mins => sk.duration_minutes) AS end_at
		FROM technician t
		JOIN technician_skill sk
		  ON sk.technician_id = t.id AND sk.service_type_id = $2
		WHERE t.dealership_id = $1
		  AND t.active
		  AND $3::timestamptz + make_interval(mins => sk.duration_minutes) <= $4::timestamptz
		  AND EXISTS (
			SELECT 1 FROM technician_shift sh
			WHERE sh.technician_id = t.id
			  AND sh.weekday = EXTRACT(ISODOW FROM ($3::timestamptz AT TIME ZONE t.timezone))::smallint
			  AND sh.start_minutes <= EXTRACT(HOUR FROM ($3::timestamptz AT TIME ZONE t.timezone)) * 60
			                         + EXTRACT(MINUTE FROM ($3::timestamptz AT TIME ZONE t.timezone))
			  AND sh.end_minutes >= EXTRACT(HOUR FROM (($3::timestamptz + make_interval(mins => sk.duration_minutes)) AT TIME ZONE t.timezone)) * 60
			                         + EXTRACT(MINUTE FROM (($3::timestamptz + make_interval(mins => sk.duration_minutes)) AT TIME ZONE t.timezone))
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM technician_absence a
			WHERE a.technician_id = t.id
			  AND tstzrange(a.start_at, a.end_at, '[)') && tstzrange($3::timestamptz, $3::timestamptz + make_interval(mins => sk.duration_minutes), '[)')
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM tech_occupation o
			WHERE o.technician_id = t.id
			  AND o.status IN ('HELD', 'CONFIRMED')
			  AND tstzrange(o.start_at, o.end_at, '[)') && tstzrange($3::timestamptz, $3::timestamptz + make_interval(mins => sk.duration_minutes), '[)')
		  )
		ORDER BY hashtext(t.id::text || $5)
		LIMIT 1
	) AS picked
	RETURNING id, technician_id, end_at`

	var occ Occupation
	for attempt := 1; attempt <= maxReserveAttempts; attempt++ {
		err := pool.QueryRow(ctx, claimSQL,
			p.dealershipID, p.serviceTypeID, p.start, p.latestEnd, p.requestID,
		).Scan(&occ.ID, &occ.ResourceID, &occ.End)
		if err == nil {
			return occ, nil
		}
		var pgErr *pgconn.PgError
		empty := errors.Is(err, pgx.ErrNoRows)
		collision := errors.As(err, &pgErr) && pgErr.Code == exclusionViolation
		if empty || collision {
			// Zero rows or GiST 23P01: this allocator deletes expired HELD and retries.
			if _, derr := pool.Exec(ctx,
				`DELETE FROM tech_occupation WHERE status = 'HELD' AND hold_expires_at <= now()`); derr != nil {
				return Occupation{}, ErrUnavailable
			}
			continue
		}
		return Occupation{}, err
	}
	return Occupation{}, ErrUnavailable
}
