// Store: appointment persistence on the booking DB. Opaque ids everywhere; no
// GiST here — the appointment is the business record, not the lock.
package booking

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AppointmentRow is the DB row shape.
type AppointmentRow struct {
	ID                 string
	DealershipID       string
	CustomerID         string
	VehicleID          string
	ServiceTypeID      string
	TechnicianID       *string
	ServiceBayID       *string
	TechOccupationID   *string
	BayOccupationID    *string
	StartAt            time.Time
	EndAt              time.Time
	Status             string
	HoldExpiresAt      *time.Time
	IDempotencyKey     *string
	RequestFingerprint *string
}

// Store wraps the booking pool.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// NewUUID mints a random UUID-shaped id (Booking-side ids are opaque).
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// InsertHeld persists step 6: the appointment row HELD, pointing at both
// occupation ids, end_at = the reserved (skill-derived) end. Always HELD —
// confirm-without-hold inserts HELD and promotes in step 7. No idempotency on
// this insert.
func (s *Store) InsertHeld(ctx context.Context, r AppointmentRow) (AppointmentRow, error) {
	if r.ID == "" {
		r.ID = NewUUID()
	}
	r.Status = StatusHeld
	_, err := s.pool.Exec(ctx, `
		INSERT INTO appointment
			(id, dealership_id, customer_id, vehicle_id, service_type_id,
			 technician_id, service_bay_id, tech_occupation_id, bay_occupation_id,
			 start_at, end_at, status, hold_expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,
		        now() + interval '120 seconds')`,
		r.ID, r.DealershipID, r.CustomerID, r.VehicleID, r.ServiceTypeID,
		r.TechnicianID, r.ServiceBayID, r.TechOccupationID, r.BayOccupationID,
		r.StartAt, r.EndAt, r.Status)
	if err != nil {
		var pgErr interface{ Error() string }
		_ = errors.As(err, &pgErr)
		return AppointmentRow{}, err
	}
	r.HoldExpiresAt = ptrTime(time.Now().Add(2 * time.Minute))
	return r, nil
}

// Get loads one appointment by id.
func (s *Store) Get(ctx context.Context, id string) (AppointmentRow, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, dealership_id, customer_id, vehicle_id, service_type_id,
		       technician_id, service_bay_id, tech_occupation_id, bay_occupation_id,
		       start_at, end_at, status, hold_expires_at, idempotency_key, request_fingerprint
		FROM appointment WHERE id = $1`, id)
	return scanAppointment(row)
}

// GetConfirmedByIdempotencyKey: confirm-only replay lookup (partial unique
// index backs this; holds do not honour the key).
func (s *Store) GetConfirmedByIdempotencyKey(ctx context.Context, key string) (AppointmentRow, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, dealership_id, customer_id, vehicle_id, service_type_id,
		       technician_id, service_bay_id, tech_occupation_id, bay_occupation_id,
		       start_at, end_at, status, hold_expires_at, idempotency_key, request_fingerprint
		FROM appointment
		WHERE idempotency_key = $1 AND status = 'CONFIRMED'`, key)
	r, err := scanAppointment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppointmentRow{}, false, nil
	}
	if err != nil {
		return AppointmentRow{}, false, err
	}
	return r, true, nil
}

// Promote sets the appointment CONFIRMED and binds idempotency on confirm
// (step 7, after both gRPC Confirms). CAS on status HELD. Empty idempotency
// key → SQL NULL: the partial unique index must never see '' (two empty-key
// confirms of different holds would then spuriously conflict, and '' as a key
// means "no key").
func (s *Store) Promote(ctx context.Context, id, idempotencyKey, fingerprint string) (AppointmentRow, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE appointment
		SET status = 'CONFIRMED', hold_expires_at = NULL,
		    idempotency_key = NULLIF($2, ''), request_fingerprint = NULLIF($3, '')
		WHERE id = $1 AND status = 'HELD'
		RETURNING id, dealership_id, customer_id, vehicle_id, service_type_id,
		          technician_id, service_bay_id, tech_occupation_id, bay_occupation_id,
		          start_at, end_at, status, hold_expires_at, idempotency_key, request_fingerprint`,
		id, idempotencyKey, fingerprint)
	r, err := scanAppointment(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return AppointmentRow{}, ErrNotFound
	}
	return r, err
}

// isUniqueViolation reports a Postgres 23505 (unique_violation), e.g. two
// concurrent Promotes of the same Idempotency-Key.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Cancel sets CANCELLED (sweeper CAS or DELETE handler).
func (s *Store) Cancel(ctx context.Context, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE appointment SET status = 'CANCELLED', hold_expires_at = NULL
		 WHERE id = $1 AND status IN ('HELD', 'CONFIRMED')`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SweepBatch CASes expired HELD → CANCELLED and returns the released rows so
// the sweeper can Release both occupations (Recovery; no outbox).
func (s *Store) SweepBatch(ctx context.Context, limit int) ([]AppointmentRow, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE appointment
		SET status = 'CANCELLED', hold_expires_at = NULL
		WHERE id IN (
			SELECT id FROM appointment
			WHERE status = 'HELD' AND hold_expires_at <= now()
			ORDER BY hold_expires_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, dealership_id, customer_id, vehicle_id, service_type_id,
		          technician_id, service_bay_id, tech_occupation_id, bay_occupation_id,
		          start_at, end_at, status, hold_expires_at, idempotency_key, request_fingerprint`,
		limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppointmentRow
	for rows.Next() {
		r, err := scanAppointment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListCancelledNeedingRelease returns CANCELLED rows that still point at at
// least one live occupation. The sweeper Releases them and only then clears
// the ids — the ids are the drop-out marker: set until Release succeeds, NULL
// after, so a sweeper crash can never orphan (or double-release) a slot.
func (s *Store) ListCancelledNeedingRelease(ctx context.Context, limit int) ([]AppointmentRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, dealership_id, customer_id, vehicle_id, service_type_id,
		       technician_id, service_bay_id, tech_occupation_id, bay_occupation_id,
		       start_at, end_at, status, hold_expires_at, idempotency_key, request_fingerprint
		FROM appointment
		WHERE status = 'CANCELLED'
		  AND (tech_occupation_id IS NOT NULL OR bay_occupation_id IS NOT NULL)
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppointmentRow
	for rows.Next() {
		r, err := scanAppointment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClearOccupationIDs nulls both occupation ids, CANCELLED rows only — never a
// HELD/CONFIRMED appointment. Call only after both Release RPCs succeeded.
func (s *Store) ClearOccupationIDs(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE appointment
		SET tech_occupation_id = NULL, bay_occupation_id = NULL
		WHERE id = $1 AND status = 'CANCELLED'`, id)
	return err
}

type rowScanner interface{ Scan(dest ...any) error }

func scanAppointment(row rowScanner) (AppointmentRow, error) {
	var r AppointmentRow
	err := row.Scan(&r.ID, &r.DealershipID, &r.CustomerID, &r.VehicleID, &r.ServiceTypeID,
		&r.TechnicianID, &r.ServiceBayID, &r.TechOccupationID, &r.BayOccupationID,
		&r.StartAt, &r.EndAt, &r.Status, &r.HoldExpiresAt, &r.IDempotencyKey, &r.RequestFingerprint)
	return r, err
}

func ptrTime(t time.Time) *time.Time { return &t }
