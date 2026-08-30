// Package catalog holds the Catalog stub's read-only HTTP handlers
// (openapi/catalog.yaml). Extracted from cmd/catalog so tests can serve the
// real Catalog over httptest against the real catalog database.
package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// NewHandler returns the Catalog read API: GET /dealerships, /service-types,
// /customers/{id}/vehicles, /healthz.
func NewHandler(pool *pgxpool.Pool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dealerships", listDealerships(pool))
	mux.HandleFunc("GET /service-types", listServiceTypes(pool))
	mux.HandleFunc("GET /customers/{id}/vehicles", listCustomerVehicles(pool))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": http.StatusText(status), "message": message})
}

func listDealerships(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		rows, err := pool.Query(ctx, `
			SELECT d.id, d.name, d.timezone,
			       h.weekday, h.open_minutes, h.close_minutes
			FROM dealership d
			LEFT JOIN opening_hours h ON h.dealership_id = d.id
			ORDER BY d.name, h.weekday`)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query dealerships")
			return
		}
		defer rows.Close()

		byID := map[string]*Dealership{}
		var order []string
		for rows.Next() {
			var id, name, tz string
			var weekday, openMin, closeMin *int16
			if err := rows.Scan(&id, &name, &tz, &weekday, &openMin, &closeMin); err != nil {
				writeError(w, http.StatusInternalServerError, "scan dealership")
				return
			}
			d, ok := byID[id]
			if !ok {
				d = &Dealership{ID: id, Name: name, Timezone: tz, OpeningHours: []OpeningHours{}}
				byID[id] = d
				order = append(order, id)
			}
			if weekday != nil {
				d.OpeningHours = append(d.OpeningHours, OpeningHours{
					Weekday: int(*weekday), OpenMinutes: int(*openMin), CloseMinutes: int(*closeMin),
				})
			}
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusInternalServerError, "scan dealerships")
			return
		}

		out := make([]Dealership, 0, len(order))
		for _, id := range order {
			out = append(out, *byID[id])
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func listServiceTypes(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, err := pool.Query(r.Context(), `
			SELECT id, name, duration_minutes FROM service_type ORDER BY name`)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query service types")
			return
		}
		defer rows.Close()

		out := make([]ServiceType, 0)
		for rows.Next() {
			var st ServiceType
			if err := rows.Scan(&st.ID, &st.Name, &st.DurationMinutes); err != nil {
				writeError(w, http.StatusInternalServerError, "scan service type")
				return
			}
			out = append(out, st)
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusInternalServerError, "scan service types")
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func listCustomerVehicles(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		customerID := r.PathValue("id")
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM customer WHERE id = $1)`, customerID,
		).Scan(&exists); err != nil {
			writeError(w, http.StatusInternalServerError, "query customer")
			return
		}
		if !exists {
			writeError(w, http.StatusNotFound, "customer not found")
			return
		}

		rows, err := pool.Query(ctx,
			`SELECT id, customer_id, vin FROM vehicle WHERE customer_id = $1 ORDER BY vin`, customerID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query vehicles")
			return
		}
		defer rows.Close()

		out := make([]Vehicle, 0)
		for rows.Next() {
			var v Vehicle
			if err := rows.Scan(&v.ID, &v.CustomerID, &v.VIN); err != nil {
				writeError(w, http.StatusInternalServerError, "scan vehicle")
				return
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			writeError(w, http.StatusInternalServerError, "scan vehicles")
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// JSON wire types — field names match openapi/catalog.yaml (camelCase).

type Dealership struct {
	ID           string         `json:"id"`
	Name         string         `json:"name,omitempty"`
	Timezone     string         `json:"timezone"`
	OpeningHours []OpeningHours `json:"openingHours,omitempty"`
}

type OpeningHours struct {
	Weekday      int `json:"weekday"`
	OpenMinutes  int `json:"openMinutes"`
	CloseMinutes int `json:"closeMinutes"`
}

type ServiceType struct {
	ID              string `json:"id"`
	Name            string `json:"name,omitempty"`
	DurationMinutes int    `json:"durationMinutes"`
}

type Vehicle struct {
	ID         string `json:"id"`
	CustomerID string `json:"customerId"`
	VIN        string `json:"vin"`
}
