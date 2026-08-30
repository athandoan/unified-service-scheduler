// Package booking implements the Booking write path (SYSTEM_DESIGN, write-path
// steps 1–7, Cancel, Recovery): Catalog snapshot over HTTP (fail-closed), the
// reserve tech → bay → appointment saga over gRPC, idempotent confirm, cancel,
// and the HELD sweeper. Booking owns the appointment row only — it never runs
// occupancy SQL; the allocators' GiST constraints stay the race authority.
package booking

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

const (
	// HoldTTL is fixed at 120 seconds in SQL. No env override.
	HoldTTL = "120 seconds"

	// SweepInterval is the Booking sweeper's CAS tick (Recovery): expired HELD
	// → CANCELLED, then Release both occupations. Best effort, no outbox.
	SweepInterval = 10 * time.Second

	// sweepLimit bounds each sweeper pass: CAS batch and release drop-out list.
	sweepLimit = 50
)

// ErrUnavailable maps to HTTP 409: no capacity or a contended claim.
var ErrUnavailable = errors.New("unavailable")

// ErrInvalid maps to HTTP 400: bad payload, start outside hours, unknown
// Catalog resource, expired/unknown hold, idempotency misuse.
var ErrInvalid = errors.New("invalid request")

// ErrIdempotencyConflict maps to HTTP 409: same key, different fingerprint.
var ErrIdempotencyConflict = errors.New("idempotency key reused with a different request")

// ErrNotFound maps to HTTP 404.
var ErrNotFound = errors.New("not found")

// Status values of an appointment.
const (
	StatusHeld      = "HELD"
	StatusConfirmed = "CONFIRMED"
	StatusCancelled = "CANCELLED"
)

// HoldRequest is POST /holds (not idempotent; Idempotency-Key is rejected).
type HoldRequest struct {
	DealershipID  string `json:"dealershipId"`
	CustomerID    string `json:"customerId"`
	VehicleID     string `json:"vehicleId"`
	ServiceTypeID string `json:"serviceTypeId"`
	Start         string `json:"start"` // ISO 8601 timestamptz; duration is never accepted
}

// ConfirmRequest is POST /appointments: holdId = promote-in-place; otherwise
// confirm-without-hold (full saga).
type ConfirmRequest struct {
	HoldID        string `json:"holdId,omitempty"`
	DealershipID  string `json:"dealershipId,omitempty"`
	CustomerID    string `json:"customerId,omitempty"`
	VehicleID     string `json:"vehicleId,omitempty"`
	ServiceTypeID string `json:"serviceTypeId,omitempty"`
	Start         string `json:"start,omitempty"`
}

// Appointment is the public wire shape (openapi/booking.yaml): camelCase ids,
// snake_case start_at/end_at/hold_expires_at exactly as the schema names them.
type Appointment struct {
	ID              string  `json:"id"`
	DealershipID    string  `json:"dealershipId"`
	CustomerID      string  `json:"customerId"`
	VehicleID       string  `json:"vehicleId"`
	ServiceTypeID   string  `json:"serviceTypeId"`
	StartAt         string  `json:"start_at"`
	EndAt           string  `json:"end_at"`
	Status          string  `json:"status"`
	HoldExpiresAt   *string `json:"hold_expires_at"`
	TechnicianID    string  `json:"technicianId,omitempty"`
	ServiceBayID    string  `json:"serviceBayId,omitempty"`
	TechOccupation  string  `json:"techOccupationId,omitempty"`
	BayOccupationID string  `json:"bayOccupationId,omitempty"`
	// internal: never marshalled
	RowID string `json:"-"`
}

// fingerprint is the confirm-only request fingerprint bound to Idempotency-Key.
func fingerprint(holdID, dealershipID, customerID, vehicleID, serviceTypeID, start string) string {
	h := sha256.New()
	for _, part := range []string{holdID, dealershipID, customerID, vehicleID, serviceTypeID, start} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// parseStart parses an ISO 8601 timestamptz.
func parseStart(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}
