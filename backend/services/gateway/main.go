// Command gateway is the thin REST edge (SYSTEM_DESIGN, Gateway role): a
// reverse-proxy only. Routes Catalog reads to :8081 and Booking writes to
// :8082, forwarding Idempotency-Key and X-Request-Id. No DB, no pgx, no proto,
// no allocator clients, no idempotency logic — liveness only.
package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

type proxy struct {
	catalog *httputil.ReverseProxy
	booking *httputil.ReverseProxy
}

// forwardHeaders copies the idempotency/tracing headers onto the outbound
// request (it does not apply idempotency — that stays on Booking confirm).
func forwardHeaders(src *http.Request, dst *http.Request) {
	if v := src.Header.Get("Idempotency-Key"); v != "" {
		dst.Header.Set("Idempotency-Key", v)
	}
	reqID := src.Header.Get("X-Request-Id")
	if reqID == "" {
		reqID = newRequestID()
	}
	dst.Header.Set("X-Request-Id", reqID)
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func main() {
	catalogTarget := envOr("GATEWAY_CATALOG_URL", "http://localhost:8081")
	bookingTarget := envOr("GATEWAY_BOOKING_URL", "http://localhost:8082")
	addr := envOr("GATEWAY_ADDR", ":8080")

	catalogURL, err := url.Parse(catalogTarget)
	if err != nil {
		log.Fatalf("gateway: catalog url: %v", err)
	}
	bookingURL, err := url.Parse(bookingTarget)
	if err != nil {
		log.Fatalf("gateway: booking url: %v", err)
	}

	p := &proxy{
		catalog: httputil.NewSingleHostReverseProxy(catalogURL),
		booking: httputil.NewSingleHostReverseProxy(bookingURL),
	}

	log.Printf("gateway: listening on %s (catalog=%s booking=%s)", addr, catalogTarget, bookingTarget)
	if err := http.ListenAndServe(addr, corsWrap(newMux(p))); err != nil {
		log.Fatalf("gateway: serve: %v", err)
	}
}

// newMux wires the gateway routes onto the given backends. Extracted from main
// so tests can hit the routes via httptest without listening. Routes are
// method-specific (Go 1.22 ServeMux patterns), so browser preflights
// (OPTIONS) never match them — corsWrap handles that outside the mux.
func newMux(p *proxy) *http.ServeMux {
	mux := http.NewServeMux()
	// Catalog reads (openapi/catalog.yaml).
	mux.HandleFunc("GET /dealerships", p.catalogRoute)
	mux.HandleFunc("GET /service-types", p.catalogRoute)
	mux.HandleFunc("GET /customers/{id}/vehicles", p.catalogRoute)
	// Booking writes + reads (openapi/booking.yaml).
	mux.HandleFunc("POST /holds", p.bookingRoute)
	mux.HandleFunc("POST /appointments", p.bookingRoute)
	mux.HandleFunc("GET /appointments/{id}", p.bookingRoute)
	mux.HandleFunc("DELETE /appointments/{id}", p.bookingRoute)
	// Liveness only — no DB.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	return mux
}

// corsWrap adds the CORS response headers the browser harness needs (it is
// served from :8000 and calls this Gateway on :8080) and answers preflights.
// The method-specific mux patterns would 405 an OPTIONS request, so OPTIONS is
// short-circuited here: 204 with CORS headers, no backend proxy. Everything
// else still goes through the mux — CORS is headers only, the saga stays on
// Booking.
func corsWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Idempotency-Key, X-Request-Id")
		h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (p *proxy) catalogRoute(w http.ResponseWriter, r *http.Request) {
	forwardHeaders(r, r)
	p.catalog.ServeHTTP(w, r)
}

func (p *proxy) bookingRoute(w http.ResponseWriter, r *http.Request) {
	forwardHeaders(r, r)
	p.booking.ServeHTTP(w, r)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func cryptoRead(b []byte) (int, error) { return rand.Read(b) }
