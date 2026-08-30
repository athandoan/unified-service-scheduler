// Service wires the Booking write path: Store (booking DB), fail-closed
// Catalog client, and the two allocator gRPC clients.
package booking

import (
	"context"
	"fmt"
	"log"
	"time"
)

// Service implements the public Booking API (POST /holds, POST /appointments,
// GET/DELETE /appointments/{id}).
type Service struct {
	store   store
	catalog *CatalogClient
	alloc   Allocators
}

// store is the slice of appointment persistence the write path uses. Unexported
// on purpose: unit tests provide an in-memory fake; production and the
// real-Postgres occupancy tests still use the concrete *Store (NewStore). Only
// the methods the write path calls live here — no GiST, no occupation SQL.
type store interface {
	InsertHeld(ctx context.Context, r AppointmentRow) (AppointmentRow, error)
	Get(ctx context.Context, id string) (AppointmentRow, error)
	Promote(ctx context.Context, id, idempotencyKey, fingerprint string) (AppointmentRow, error)
	GetConfirmedByIdempotencyKey(ctx context.Context, key string) (AppointmentRow, bool, error)
	Cancel(ctx context.Context, id string) (bool, error)
	SweepBatch(ctx context.Context, limit int) ([]AppointmentRow, error)
	ListCancelledNeedingRelease(ctx context.Context, limit int) ([]AppointmentRow, error)
	ClearOccupationIDs(ctx context.Context, id string) error
}

// NewService builds Booking.
func NewService(store *Store, cat *CatalogClient, alloc Allocators) *Service {
	return &Service{store: store, catalog: cat, alloc: alloc}
}

// requestID returns the incoming X-Request-Id or mints one. The id doubles as
// the pick-hash salt in the Reserve messages.
func requestIDFrom(header string) string {
	if header != "" {
		return header
	}
	return NewUUID()
}

// CreateHold runs steps 1–6 and returns the HELD appointment. It must not
// Confirm (step 7 belongs to POST /appointments). Not idempotent.
func (s *Service) CreateHold(ctx context.Context, requestID string, req HoldRequest) (Appointment, error) {
	start, err := parseStart(req.Start)
	if err != nil {
		return Appointment{}, fmt.Errorf("%w: start", ErrInvalid)
	}

	// Step 1: fail-closed Catalog snapshot.
	snap, err := s.catalog.Snapshot(ctx, req.DealershipID, req.ServiceTypeID, req.CustomerID, req.VehicleID)
	if err != nil {
		return Appointment{}, fmt.Errorf("%w: catalog: %v", ErrInvalid, err)
	}

	// Step 2: strictly future + inside snapshotted hours.
	if !start.After(time.Now()) || !withinHours(snap, start) {
		return Appointment{}, fmt.Errorf("%w: hours or not future", ErrInvalid)
	}

	// Steps 4–5: reserve tech → bay.
	res, err := s.reserveSaga(ctx, requestID, req.ServiceTypeID, snap, start)
	if err != nil {
		return Appointment{}, err
	}

	// Step 6: INSERT appointment HELD (end_at = reserved end). Insert fail →
	// release bay then tech.
	row, err := s.store.InsertHeld(ctx, AppointmentRow{
		DealershipID:     req.DealershipID,
		CustomerID:       req.CustomerID,
		VehicleID:        req.VehicleID,
		ServiceTypeID:    req.ServiceTypeID,
		TechnicianID:     strPtr(res.TechnicianID),
		ServiceBayID:     strPtr(res.ServiceBayID),
		TechOccupationID: strPtr(res.TechOccupationID),
		BayOccupationID:  strPtr(res.BayOccupationID),
		StartAt:          start,
		EndAt:            res.End,
	})
	if err != nil {
		s.releaseBoth(ctx, res.BayOccupationID, res.TechOccupationID, requestID)
		return Appointment{}, ErrUnavailable
	}
	return toAppointment(row), nil
}

// Confirm runs step 7. With holdId: promote-in-place (no re-reserve). Without:
// full saga (steps 1–6) then promote. Idempotency-Key applies to confirm only;
// same key + same fingerprint replays, mismatch is a conflict.
func (s *Service) Confirm(ctx context.Context, requestID, idempotencyKey string, req ConfirmRequest) (Appointment, int, error) {
	if req.HoldID != "" {
		return s.confirmHold(ctx, requestID, idempotencyKey, req)
	}
	return s.confirmWithoutHold(ctx, requestID, idempotencyKey, req)
}

// confirmHold promotes a known hold. Order matters:
//
//	Get → unknown 404 → CONFIRMED: 200 replay against the row's own
//	fingerprint (a replay body may carry only {holdId} — the stored fp, not the
//	request fields, decides) → CANCELLED: 409 → else promote HELD.
//
// The confirm-without-hold replay (replay()) still fingerprints the request
// fields; only the hold path replays from the row.
func (s *Service) confirmHold(ctx context.Context, requestID, idempotencyKey string, req ConfirmRequest) (Appointment, int, error) {
	row, err := s.store.Get(ctx, req.HoldID)
	if err != nil {
		return Appointment{}, 0, ErrNotFound
	}

	switch row.Status {
	case StatusConfirmed:
		// Replay of an already-confirmed hold. With a key, a different
		// CONFIRMED appointment under the same key is a conflict; the same
		// appointment (or no key at all) replays the row itself — never fall
		// through to Promote from here.
		if idempotencyKey != "" {
			if prior, ok, err := s.store.GetConfirmedByIdempotencyKey(ctx, idempotencyKey); err != nil {
				return Appointment{}, 0, err
			} else if ok && prior.ID != row.ID {
				return Appointment{}, 0, ErrIdempotencyConflict
			}
		}
		return toAppointment(row), 200, nil
	case StatusCancelled:
		return Appointment{}, 0, ErrUnavailable
	}

	// HELD: promote with a fingerprint taken from the row, so a replay body of
	// just {holdId} compares equal to the first full confirm's fingerprint.
	fp := fingerprint(req.HoldID, row.DealershipID, row.CustomerID, row.VehicleID, row.ServiceTypeID, "")
	if err := s.promoteConfirmed(ctx, requestID, row, idempotencyKey, fp); err != nil {
		return Appointment{}, 0, err
	}
	out, err := s.store.Get(ctx, row.ID)
	if err != nil {
		return Appointment{}, 0, ErrNotFound
	}
	return toAppointment(out), 201, nil
}

// confirmWithoutHold: steps 1–6 (insert HELD) then step 7 (promote).
func (s *Service) confirmWithoutHold(ctx context.Context, requestID, idempotencyKey string, req ConfirmRequest) (Appointment, int, error) {
	start, err := parseStart(req.Start)
	if err != nil {
		return Appointment{}, 0, fmt.Errorf("%w: start", ErrInvalid)
	}
	snap, err := s.catalog.Snapshot(ctx, req.DealershipID, req.ServiceTypeID, req.CustomerID, req.VehicleID)
	if err != nil {
		return Appointment{}, 0, fmt.Errorf("%w: catalog: %v", ErrInvalid, err)
	}
	if !start.After(time.Now()) || !withinHours(snap, start) {
		return Appointment{}, 0, fmt.Errorf("%w: hours or not future", ErrInvalid)
	}

	// Idempotency replay only makes sense for a deterministic request; check
	// before claiming (confirm-only; holds never honour the key).
	if appt, ok, err := s.replay(ctx, idempotencyKey, req); ok || err != nil {
		if err != nil {
			return Appointment{}, 0, err
		}
		return appt, 200, nil
	}

	res, err := s.reserveSaga(ctx, requestID, req.ServiceTypeID, snap, start)
	if err != nil {
		return Appointment{}, 0, err
	}

	row, err := s.store.InsertHeld(ctx, AppointmentRow{
		DealershipID:     req.DealershipID,
		CustomerID:       req.CustomerID,
		VehicleID:        req.VehicleID,
		ServiceTypeID:    req.ServiceTypeID,
		TechnicianID:     strPtr(res.TechnicianID),
		ServiceBayID:     strPtr(res.ServiceBayID),
		TechOccupationID: strPtr(res.TechOccupationID),
		BayOccupationID:  strPtr(res.BayOccupationID),
		StartAt:          start,
		EndAt:            res.End,
	})
	if err != nil {
		s.releaseBoth(ctx, res.BayOccupationID, res.TechOccupationID, requestID)
		return Appointment{}, 0, ErrUnavailable
	}

	fp := fingerprint("", req.DealershipID, req.CustomerID, req.VehicleID, req.ServiceTypeID, req.Start)
	if err := s.promoteConfirmed(ctx, requestID, row, idempotencyKey, fp); err != nil {
		return Appointment{}, 0, err
	}
	out, err := s.store.Get(ctx, row.ID)
	if err != nil {
		return Appointment{}, 0, ErrNotFound
	}
	return toAppointment(out), 201, nil
}

// promoteConfirmed is step 7: gRPC Confirm tech then bay in place; then UPDATE
// appointment CONFIRMED + idempotency. Any Confirm failure → release both
// occupation ids (reverse order), 409, never a replacement pair.
//
// On a Promote (SQL) failure the row is re-Get first: a concurrent confirmer
// may have already promoted it (CONFIRMED → success, zero Releases). Only this
// row's occupation ids are ever released — never another appointment's.
func (s *Service) promoteConfirmed(ctx context.Context, requestID string, row AppointmentRow, idempotencyKey, fp string) error {
	if row.TechOccupationID == nil || row.BayOccupationID == nil {
		return ErrUnavailable
	}
	bayOccID, techOccID := deref(row.BayOccupationID), deref(row.TechOccupationID)

	if err := s.confirmBoth(ctx, techOccID, bayOccID, requestID); err != nil {
		// Still HELD as far as we know: our occupations were reserved and are
		// useless unconfirmed — release them.
		s.releaseBoth(ctx, bayOccID, techOccID, requestID)
		return ErrUnavailable
	}

	if _, err := s.store.Promote(ctx, row.ID, idempotencyKey, fp); err != nil {
		// Occupations are CONFIRMED but the row flip failed. Re-read the row
		// before compensating: another confirmer may have won the CAS.
		cur, getErr := s.store.Get(ctx, row.ID)
		if getErr != nil {
			// Unknowable state; bound the pin by releasing OUR ids only.
			s.releaseBoth(ctx, bayOccID, techOccID, requestID)
			return ErrUnavailable
		}
		switch cur.Status {
		case StatusConfirmed:
			// The concurrent confirmer won: success/replay, zero Releases.
			return nil
		case StatusCancelled:
			// Cancelled out from under us; release OUR ids.
			s.releaseBoth(ctx, bayOccID, techOccID, requestID)
			return ErrUnavailable
		default: // still HELD: CAS miss or unique-key collision.
			if isUniqueViolation(err) {
				// Same key promoted a different appointment: conflict.
				s.releaseBoth(ctx, bayOccID, techOccID, requestID)
				return ErrIdempotencyConflict
			}
			// Transient still-HELD: the flip is racing us (a concurrent
			// Promote of this hold is in flight and has not committed) or the
			// UPDATE hit an unrelated store error. Our occupations are
			// confirmed but our flip failed; release our own ids — if the
			// racing confirmer already flipped, its re-Get sees CONFIRMED and
			// it releases nothing.
			s.releaseBoth(ctx, bayOccID, techOccID, requestID)
			return ErrUnavailable
		}
	}
	return nil
}

// replay resolves an Idempotency-Key on CONFIRMED: same fingerprint → 200
// replay; different fingerprint → 409.
func (s *Service) replay(ctx context.Context, idempotencyKey string, req ConfirmRequest) (Appointment, bool, error) {
	if idempotencyKey == "" {
		return Appointment{}, false, nil
	}
	row, ok, err := s.store.GetConfirmedByIdempotencyKey(ctx, idempotencyKey)
	if err != nil || !ok {
		return Appointment{}, false, err
	}
	fp := fingerprint(req.HoldID, req.DealershipID, req.CustomerID, req.VehicleID, req.ServiceTypeID, req.Start)
	if row.RequestFingerprint == nil || *row.RequestFingerprint != fp {
		return Appointment{}, false, ErrIdempotencyConflict
	}
	return toAppointment(row), true, nil
}

// Get returns the confirmation record (IDOR-able by design: no auth).
func (s *Service) Get(ctx context.Context, id string) (Appointment, error) {
	row, err := s.store.Get(ctx, id)
	if err != nil {
		return Appointment{}, ErrNotFound
	}
	return toAppointment(row), nil
}

// Cancel: SET CANCELLED then Release both occupations (bay then tech). The
// occupation ids stay set even if a Release fails — the sweeper's
// ListCancelledNeedingRelease drop-out pass retries them.
func (s *Service) Cancel(ctx context.Context, requestID, id string) error {
	row, err := s.store.Get(ctx, id)
	if err != nil {
		return ErrNotFound
	}
	if _, err := s.store.Cancel(ctx, id); err != nil {
		return ErrUnavailable
	}
	if row.BayOccupationID != nil {
		_ = s.releaseBay(ctx, *row.BayOccupationID, requestID)
	}
	if row.TechOccupationID != nil {
		_ = s.releaseTech(ctx, *row.TechOccupationID, requestID)
	}
	return nil
}

// StartSweeper runs the Recovery loop each tick: CAS expired HELD → CANCELLED,
// then release the occupations of CANCELLED rows that still hold ids (drop-out:
// ids are cleared only after both Releases succeed, so a sweeper crash or a
// failed Release retries next tick instead of orphaning the slot). Best effort;
// the allocators' TTL reaper bounds any missed row.
func (s *Service) StartSweeper(ctx context.Context) {
	go func() {
		t := time.NewTicker(SweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepOnce(ctx)
			}
		}
	}()
}

// sweepOnce is one Recovery pass: SweepBatch, then ListCancelledNeedingRelease
// bounded by LIMIT. Extracted so tests can drive a pass without sleeping on
// the ticker.
func (s *Service) sweepOnce(ctx context.Context) {
	rows, err := s.store.SweepBatch(ctx, sweepLimit)
	if err != nil {
		log.Printf("booking: sweep: %v", err)
		return
	}
	// SweepBatch just CASed these HELD → CANCELLED; hand them to the same
	// release-and-clear path so a failed Release is retried next tick.
	s.releaseCancelled(ctx, rows)

	pending, err := s.store.ListCancelledNeedingRelease(ctx, sweepLimit)
	if err != nil {
		log.Printf("booking: sweep: list cancelled: %v", err)
		return
	}
	s.releaseCancelled(ctx, pending)
}

// releaseCancelled releases bay then tech for each row; only when BOTH Release
// RPCs succeed are the occupation ids cleared, so a drop-out before that point
// leaves the ids set for the next tick (and, worst case, the allocators' TTL).
func (s *Service) releaseCancelled(ctx context.Context, rows []AppointmentRow) {
	for _, r := range rows {
		if r.BayOccupationID == nil && r.TechOccupationID == nil {
			continue
		}
		reqID := NewUUID()
		bayErr := error(nil)
		if r.BayOccupationID != nil {
			bayErr = s.releaseBay(ctx, *r.BayOccupationID, reqID)
		}
		techErr := error(nil)
		if r.TechOccupationID != nil {
			techErr = s.releaseTech(ctx, *r.TechOccupationID, reqID)
		}
		if bayErr != nil || techErr != nil {
			// Ids stay set; the next sweep retries this row.
			continue
		}
		if err := s.store.ClearOccupationIDs(ctx, r.ID); err != nil {
			log.Printf("booking: sweep: clear occupation ids for %s: %v (will retry)", r.ID, err)
		}
	}
}

func toAppointment(r AppointmentRow) Appointment {
	appt := Appointment{
		ID:            r.ID,
		DealershipID:  r.DealershipID,
		CustomerID:    r.CustomerID,
		VehicleID:     r.VehicleID,
		ServiceTypeID: r.ServiceTypeID,
		StartAt:       r.StartAt.UTC().Format(time.RFC3339Nano),
		EndAt:         r.EndAt.UTC().Format(time.RFC3339Nano),
		Status:        r.Status,
	}
	if r.HoldExpiresAt != nil {
		s := r.HoldExpiresAt.UTC().Format(time.RFC3339Nano)
		appt.HoldExpiresAt = &s
	}
	if r.TechnicianID != nil {
		appt.TechnicianID = *r.TechnicianID
	}
	if r.ServiceBayID != nil {
		appt.ServiceBayID = *r.ServiceBayID
	}
	if r.TechOccupationID != nil {
		appt.TechOccupation = *r.TechOccupationID
	}
	if r.BayOccupationID != nil {
		appt.BayOccupationID = *r.BayOccupationID
	}
	return appt
}

func strPtr(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
