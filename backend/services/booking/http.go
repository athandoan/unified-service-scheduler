// HTTP transport for Booking (openapi/booking.yaml). Field names match the
// schema: camelCase ids, snake_case start_at / end_at / hold_expires_at.
package booking

import (
	"encoding/json"
	"errors"
	"net/http"
)

// NewHandler returns the Booking public API handler.
func NewHandler(svc *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /holds", createHold(svc))
	mux.HandleFunc("POST /appointments", confirm(svc))
	mux.HandleFunc("GET /appointments/{id}", get(svc))
	mux.HandleFunc("DELETE /appointments/{id}", cancel(svc))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, errName, message string) {
	writeJSON(w, status, map[string]string{"error": errName, "message": message})
}

// mapError: Booking's single 409 for unavailable/contended; 400 for invalid;
// 404 unknown; 409 idempotency conflict.
func mapError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_conflict", "Idempotency-Key reused with a different request")
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "appointment not found")
	case errors.Is(err, ErrUnavailable):
		writeError(w, http.StatusConflict, "unavailable", "no free slot or contended")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// requestID extracts X-Request-Id (Gateway forwards it) or lets the service
// mint one; it travels as gRPC metadata and the pick-hash salt.
func requestID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" {
		return v
	}
	return NewUUID()
}

// createHold: POST /holds — steps 1–6 only, returns 201 HELD. Not idempotent:
// an Idempotency-Key on /holds is rejected (openapi: must not send it).
func createHold(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "" {
			writeError(w, http.StatusBadRequest, "invalid", "Idempotency-Key is confirm-only; holds are not idempotent")
			return
		}
		var req HoldRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "bad JSON body")
			return
		}
		appt, err := svc.CreateHold(r.Context(), requestID(r), req)
		if err != nil {
			mapError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, appt)
	}
}

// confirm: POST /appointments — confirm-without-hold or promote-in-place.
// 200 = idempotent replay, 201 = newly confirmed.
func confirm(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ConfirmRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "bad JSON body")
			return
		}
		appt, status, err := svc.Confirm(r.Context(), requestID(r), r.Header.Get("Idempotency-Key"), req)
		if err != nil {
			mapError(w, err)
			return
		}
		writeJSON(w, status, appt)
	}
}

func get(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		appt, err := svc.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			mapError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, appt)
	}
}

func cancel(svc *Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := svc.Cancel(r.Context(), requestID(r), r.PathValue("id"))
		if err != nil {
			mapError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
