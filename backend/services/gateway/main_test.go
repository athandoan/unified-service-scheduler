// Gateway route tests (Phase 1, mocked): httptest fake backends, real mux, no
// Listen, no pgx. GET /dealerships proxies to Catalog, POST /appointments
// proxies to Booking, GET /healthz answers 200 locally.
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

func testMux(t *testing.T) (*httptest.Server, *int, *int) {
	t.Helper()
	var catalogHits, bookingHits int
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogHits++
		if r.Header.Get("X-Request-Id") == "" {
			t.Errorf("gateway must forward/mint X-Request-Id")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[{"id":"11111111-1111-4111-8111-111111111111","name":"North Workshop"}]`)
	}))
	booking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bookingHits++
		if r.Header.Get("X-Request-Id") == "" {
			t.Errorf("gateway must forward/mint X-Request-Id")
		}
		if r.Method == http.MethodPost && r.Header.Get("Idempotency-Key") == "" {
			t.Errorf("gateway must forward Idempotency-Key to booking")
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"appt-1","status":"HELD"}`)
	}))
	t.Cleanup(func() { catalog.Close(); booking.Close() })

	catURL := mustURL(t, catalog.URL)
	bookURL := mustURL(t, booking.URL)
	p := &proxy{
		catalog: httputil.NewSingleHostReverseProxy(catURL),
		booking: httputil.NewSingleHostReverseProxy(bookURL),
	}
	mux := corsWrap(newMux(p))
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)
	return gw, &catalogHits, &bookingHits
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u
}

// GET /dealerships proxies to the Catalog backend.
func TestGateway_Dealerships_ProxiesCatalog(t *testing.T) {
	gw, catHits, bookHits := testMux(t)

	res, err := http.Get(gw.URL + "/dealerships")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", res.StatusCode)
	}
	if !strings.Contains(string(body), "North Workshop") {
		t.Fatalf("proxied catalog body, got %s", body)
	}
	if *catHits != 1 || *bookHits != 0 {
		t.Fatalf("catalog=%d booking=%d hits; want catalog=1 booking=0", *catHits, *bookHits)
	}
}

// POST /appointments proxies to Booking, forwarding Idempotency-Key.
func TestGateway_PostAppointments_ProxiesBooking(t *testing.T) {
	gw, catHits, bookHits := testMux(t)

	req, err := http.NewRequest(http.MethodPost, gw.URL+"/appointments",
		strings.NewReader(`{"dealershipId":"d1"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "key-1")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusCreated {
		t.Fatalf("want 201 from booking, got %d", res.StatusCode)
	}
	if *bookHits != 1 || *catHits != 0 {
		t.Fatalf("catalog=%d booking=%d hits; want catalog=0 booking=1", *catHits, *bookHits)
	}
}

// OPTIONS /holds is answered by the gateway's CORS layer — 204 with CORS
// headers, and zero backend hits (the method-specific mux would not match a
// preflight; it must not be proxied either).
func TestGateway_Options_ShortCircuitsWithCORS(t *testing.T) {
	gw, catHits, bookHits := testMux(t)

	req, err := http.NewRequest(http.MethodOptions, gw.URL+"/holds", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://localhost:8000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type, idempotency-key")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204 for OPTIONS preflight, got %d", res.StatusCode)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin: want *, got %q", got)
	}
	if got := res.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Fatalf("Access-Control-Allow-Methods must include POST, got %q", got)
	}
	for _, h := range []string{"Content-Type", "Idempotency-Key"} {
		got := res.Header.Get("Access-Control-Allow-Headers")
		if !strings.Contains(got, h) {
			t.Fatalf("Access-Control-Allow-Headers must include %s, got %q", h, got)
		}
	}
	if *catHits != 0 || *bookHits != 0 {
		t.Fatalf("preflight must not hit backends: catalog=%d booking=%d", *catHits, *bookHits)
	}
}

// GET /healthz is answered by the gateway itself — 200, no backend call.
func TestGateway_Healthz_200(t *testing.T) {
	gw, catHits, bookHits := testMux(t)

	res, err := http.Get(gw.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", res.StatusCode)
	}
	if strings.TrimSpace(string(body)) != "ok" {
		t.Fatalf("healthz body: want ok, got %q", body)
	}
	if *catHits != 0 || *bookHits != 0 {
		t.Fatalf("healthz must not hit backends: catalog=%d booking=%d", *catHits, *bookHits)
	}
}
