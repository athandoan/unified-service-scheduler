// Mocked unit tests (Phase 1, CI without Postgres). All fakes live here:
//
//   - fakeStore: in-memory unexported `store` implementation (InsertHeld / Get /
//     Promote / GetConfirmedByIdempotencyKey / Cancel / SweepBatch).
//   - fakeTech / fakeBay: the generated gRPC client interfaces — Allocators
//     already holds TechnicianServiceClient / BayServiceClient, so no extra
//     wrapper and no bufconn.
//   - catalog 404: real CatalogClient against an httptest server (no second
//     Snapshot interface).
//
// These never touch Postgres and never assert the GiST occupancy invariant —
// the real-Postgres occupancy gate (overlap_test.go / public_occupancy_test.go,
// the 8 tests) still runs unmocked. "Bay fail → tech released" here asserts the
// saga compensation ORDER on fakes only; it is not the occupancy test.
package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bayv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/bay/v1"
	techv1 "github.com/athandoan/unified-service-scheduler/shared/gen/proto/technician/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- fake store ----------------------------------------------------------

// fakeStore is the in-memory `store` (the unexported booking interface).
type fakeStore struct {
	mu   sync.Mutex
	rows map[string]AppointmentRow
	seq  int
	// failInsert makes InsertHeld fail (sweep/compensation tests).
	failInsert bool
	// promoteErr, when set, is returned by Promote (P0 CAS-miss scripting)
	// without flipping the row. promoteCalls counts Promote invocations.
	promoteErr   error
	promoteCalls int
	// getAfterPromoteFail, when set AND promoteCalls > 0, swaps the row
	// returned by Get: the first Get (before Promote) must still see the real
	// HELD row; only the post-Promote re-Get sees the winner's CONFIRMED row.
	getAfterPromoteFail map[string]AppointmentRow
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]AppointmentRow{}}
}

func (f *fakeStore) InsertHeld(_ context.Context, r AppointmentRow) (AppointmentRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failInsert {
		return AppointmentRow{}, errors.New("db down")
	}
	if r.ID == "" {
		f.seq++
		r.ID = "appt-" + string(rune('0'+f.seq))
	}
	r.Status = StatusHeld
	f.rows[r.ID] = r
	return r, nil
}

func (f *fakeStore) Get(_ context.Context, id string) (AppointmentRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok {
		return AppointmentRow{}, ErrNotFound
	}
	// Override only on a re-Get that follows a Promote call: the first Get
	// (confirmHold's status check) must see the real HELD row.
	if f.promoteCalls > 0 && f.getAfterPromoteFail != nil {
		if sub, ok := f.getAfterPromoteFail[id]; ok {
			return sub, nil
		}
	}
	return r, nil
}

func (f *fakeStore) Promote(_ context.Context, id, key, fp string) (AppointmentRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promoteCalls++
	if f.promoteErr != nil {
		return AppointmentRow{}, f.promoteErr
	}
	r, ok := f.rows[id]
	if !ok || r.Status != StatusHeld {
		return AppointmentRow{}, ErrNotFound
	}
	r.Status = StatusConfirmed
	if key != "" {
		k := key
		r.IDempotencyKey = &k
	} else {
		r.IDempotencyKey = nil // empty key → SQL NULL, never ''
	}
	if fp != "" {
		fpv := fp
		r.RequestFingerprint = &fpv
	} else {
		r.RequestFingerprint = nil
	}
	f.rows[id] = r
	return r, nil
}

func (f *fakeStore) GetConfirmedByIdempotencyKey(_ context.Context, key string) (AppointmentRow, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.Status == StatusConfirmed && r.IDempotencyKey != nil && *r.IDempotencyKey == key {
			return r, true, nil
		}
	}
	return AppointmentRow{}, false, nil
}

func (f *fakeStore) Cancel(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok || (r.Status != StatusHeld && r.Status != StatusConfirmed) {
		return false, nil
	}
	r.Status = StatusCancelled
	f.rows[id] = r
	return true, nil
}

func (f *fakeStore) SweepBatch(_ context.Context, _ int) ([]AppointmentRow, error) {
	return nil, nil
}

// ListCancelledNeedingRelease mirrors the SQL: CANCELLED rows still holding at
// least one occupation id.
func (f *fakeStore) ListCancelledNeedingRelease(_ context.Context, limit int) ([]AppointmentRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []AppointmentRow
	for _, r := range f.rows {
		if r.Status == StatusCancelled &&
			(r.TechOccupationID != nil || r.BayOccupationID != nil) {
			out = append(out, r)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// ClearOccupationIDs nulls both ids, CANCELLED rows only (matches the SQL
// guard: never a HELD/CONFIRMED appointment).
func (f *fakeStore) ClearOccupationIDs(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok || r.Status != StatusCancelled {
		return nil
	}
	r.TechOccupationID = nil
	r.BayOccupationID = nil
	f.rows[id] = r
	return nil
}

// --- fake gRPC clients ---------------------------------------------------

// fakeTech implements techv1.TechnicianServiceClient directly — no wrapper.
type fakeTech struct {
	mu         sync.Mutex
	reserveErr error
	releaseErr error
	// released records Release calls in order.
	released  []string
	confirmed []string
}

func (f *fakeTech) Reserve(_ context.Context, _ *techv1.ReserveRequest, _ ...grpc.CallOption) (*techv1.ReserveResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	return &techv1.ReserveResponse{
		OccupationId: "tech-occ-1",
		TechnicianId: "tech-1",
		End:          nil, // filled by caller if needed
	}, nil
}

func (f *fakeTech) Confirm(_ context.Context, req *techv1.ConfirmRequest, _ ...grpc.CallOption) (*techv1.ConfirmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmed = append(f.confirmed, req.GetOccupationId())
	return &techv1.ConfirmResponse{}, nil
}

func (f *fakeTech) Release(_ context.Context, req *techv1.ReleaseRequest, _ ...grpc.CallOption) (*techv1.ReleaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releaseErr != nil {
		return nil, f.releaseErr
	}
	f.released = append(f.released, req.GetOccupationId())
	return &techv1.ReleaseResponse{}, nil
}

// fakeBay implements bayv1.BayServiceClient directly — no wrapper.
type fakeBay struct {
	mu         sync.Mutex
	reserveErr error
	releaseErr error
	released   []string
	confirmed  []string
}

func (f *fakeBay) Reserve(_ context.Context, _ *bayv1.ReserveRequest, _ ...grpc.CallOption) (*bayv1.ReserveResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveErr != nil {
		return nil, f.reserveErr
	}
	return &bayv1.ReserveResponse{OccupationId: "bay-occ-1", ServiceBayId: "bay-1"}, nil
}

func (f *fakeBay) Confirm(_ context.Context, req *bayv1.ConfirmRequest, _ ...grpc.CallOption) (*bayv1.ConfirmResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirmed = append(f.confirmed, req.GetOccupationId())
	return &bayv1.ConfirmResponse{}, nil
}

func (f *fakeBay) Release(_ context.Context, req *bayv1.ReleaseRequest, _ ...grpc.CallOption) (*bayv1.ReleaseResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releaseErr != nil {
		return nil, f.releaseErr
	}
	f.released = append(f.released, req.GetOccupationId())
	return &bayv1.ReleaseResponse{}, nil
}

// --- helpers ---------------------------------------------------------------

// holdSnapshotBody builds an httptest Catalog whose dealership is open 08:00–
// 17:00 Mon–Fri in Europe/London.
func catalogServer(t *testing.T, missingServiceType, missingVehicle bool) (*httptest.Server, string, string, string, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /dealerships", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]dealershipDTO{{
			ID:       "dealer-1",
			Name:     "Test Dealer",
			Timezone: "UTC",
			OpeningHours: []OpeningHours{
				{Weekday: 1, OpenMinutes: 480, CloseMinutes: 1020},
				{Weekday: 2, OpenMinutes: 480, CloseMinutes: 1020},
				{Weekday: 3, OpenMinutes: 480, CloseMinutes: 1020},
				{Weekday: 4, OpenMinutes: 480, CloseMinutes: 1020},
				{Weekday: 5, OpenMinutes: 480, CloseMinutes: 1020},
			},
		}})
	})
	mux.HandleFunc("GET /service-types", func(w http.ResponseWriter, _ *http.Request) {
		if missingServiceType {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_ = json.NewEncoder(w).Encode([]serviceTypeDTO{{ID: "svc-1"}})
	})
	mux.HandleFunc("GET /customers/{id}/vehicles", func(w http.ResponseWriter, _ *http.Request) {
		if missingVehicle {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode([]vehicleDTO{{ID: "veh-1", CustomerID: "cust-1"}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, "dealer-1", "cust-1", "veh-1", "svc-1"
}

// unitService wires Service with fakes only. Catalog is the real CatalogClient
// against httptest.
func unitService(t *testing.T) (*Service, *fakeStore, *fakeTech, *fakeBay) {
	t.Helper()
	srv, _, _, _, _ := catalogServer(t, false, false)
	fs := newFakeStore()
	ft := &fakeTech{}
	fb := &fakeBay{}
	svc := &Service{
		store:   fs,
		catalog: NewCatalogClient(srv.URL),
		alloc: Allocators{
			Tech: ft,
			Bay:  fb,
		},
	}
	return svc, fs, ft, fb
}

// validHoldRequest: a Monday 10:00 UTC (inside 08:00–17:00 hours).
func validHoldRequest() HoldRequest {
	return HoldRequest{
		DealershipID:  "dealer-1",
		CustomerID:    "cust-1",
		VehicleID:     "veh-1",
		ServiceTypeID: "svc-1",
		Start:         nextMondayAt(10, 0).Format(time.RFC3339Nano),
	}
}

// nextMondayAt: the next Monday at hour:minute UTC, strictly future.
func nextMondayAt(hour, minute int) time.Time {
	now := time.Now().UTC()
	cand := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, time.UTC)
	for {
		if cand.Weekday() == time.Monday && cand.After(now.Add(time.Minute)) {
			return cand
		}
		cand = cand.AddDate(0, 0, 1)
	}
}

// --- tests ----------------------------------------------------------------

// Past start → ErrInvalid (step 2: strictly future).
func TestCreateHold_PastStart_Invalid(t *testing.T) {
	svc, _, _, _ := unitService(t)
	req := validHoldRequest()
	req.Start = time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)

	_, err := svc.CreateHold(context.Background(), "req-1", req)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("past start: want ErrInvalid, got %v", err)
	}
}

// Start outside opening hours → ErrInvalid (withinHours, step 2).
func TestCreateHold_OutsideHours_Invalid(t *testing.T) {
	svc, _, _, _ := unitService(t)
	req := validHoldRequest()
	req.Start = nextMondayAt(7, 30).Format(time.RFC3339Nano) // opens 08:00

	_, err := svc.CreateHold(context.Background(), "req-1", req)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("before open: want ErrInvalid, got %v", err)
	}
}

// Catalog 404 (unknown service type) → ErrInvalid, fail-closed (step 1).
func TestCreateHold_Catalog404_Invalid(t *testing.T) {
	srv, _, _, _, _ := catalogServer(t, true /* missingServiceType */, false)
	fs := newFakeStore()
	svc := &Service{
		store:   fs,
		catalog: NewCatalogClient(srv.URL),
		alloc:   Allocators{Tech: &fakeTech{}, Bay: &fakeBay{}},
	}

	_, err := svc.CreateHold(context.Background(), "req-1", validHoldRequest())
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("catalog 404: want ErrInvalid, got %v", err)
	}
}

// POST /holds with an Idempotency-Key header is rejected (confirm-only).
func TestCreateHold_HTTP_IdempotencyKeyRejected(t *testing.T) {
	svc, _, _, _ := unitService(t)
	ts := httptest.NewServer(NewHandler(svc))
	t.Cleanup(ts.Close)

	body := `{"dealershipId":"dealer-1","customerId":"cust-1","vehicleId":"veh-1","serviceTypeId":"svc-1","start":"` +
		nextMondayAt(10, 0).Format(time.RFC3339Nano) + `"}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/holds", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "some-key")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /holds with Idempotency-Key: want 400, got %d", res.StatusCode)
	}
}

// Confirm with the same Idempotency-Key + same fingerprint replays (200),
// never re-promotes.
func TestConfirm_SameKeySameFingerprint_Replay(t *testing.T) {
	svc, _, _, _ := unitService(t)
	ctx := context.Background()
	start := nextMondayAt(10, 0)

	req := ConfirmRequest{
		DealershipID:  "dealer-1",
		CustomerID:    "cust-1",
		VehicleID:     "veh-1",
		ServiceTypeID: "svc-1",
		Start:         start.Format(time.RFC3339Nano),
	}
	first, status1, err := svc.Confirm(ctx, "req-1", "key-1", req)
	if err != nil || status1 != 201 {
		t.Fatalf("first confirm: want 201, got %d err %v", status1, err)
	}
	if first.Status != StatusConfirmed {
		t.Fatalf("first confirm status: want CONFIRMED, got %s", first.Status)
	}

	replay, status2, err := svc.Confirm(ctx, "req-2", "key-1", req)
	if err != nil || status2 != 200 {
		t.Fatalf("replay: want 200, got %d err %v", status2, err)
	}
	if replay.ID != first.ID {
		t.Fatalf("replay must return the same appointment: %s vs %s", replay.ID, first.ID)
	}
}

// Same key but a different fingerprint → 409 conflict (ErrIdempotencyConflict).
func TestConfirm_SameKeyDifferentFingerprint_Conflict(t *testing.T) {
	svc, _, _, _ := unitService(t)
	ctx := context.Background()
	start := nextMondayAt(10, 0)

	req := ConfirmRequest{
		DealershipID:  "dealer-1",
		CustomerID:    "cust-1",
		VehicleID:     "veh-1",
		ServiceTypeID: "svc-1",
		Start:         start.Format(time.RFC3339Nano),
	}
	if _, status, err := svc.Confirm(ctx, "req-1", "key-1", req); err != nil || status != 201 {
		t.Fatalf("first confirm: want 201, got %d err %v", status, err)
	}

	other := req
	// Different start = different fingerprint; still within hours so the
	// request reaches the replay check (catalog runs before it).
	other.Start = nextMondayAt(11, 0).Format(time.RFC3339Nano)
	_, _, err := svc.Confirm(ctx, "req-2", "key-1", other)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("mismatched fingerprint: want ErrIdempotencyConflict, got %v", err)
	}
}

// Saga order on fakes: Bay Reserve fail → Technician Release called (bay first
// in releaseBoth). This is compensation ordering, NOT the occupancy claim —
// the real occupancy test stays in overlap_test.go on real Postgres.
func TestConfirm_BayReserveFail_ReleasesTech(t *testing.T) {
	svc, fs, ft, fb := unitService(t)
	fb.mu.Lock()
	fb.reserveErr = status.Error(codes.FailedPrecondition, "no free bay")
	fb.mu.Unlock()

	req := ConfirmRequest{
		DealershipID:  "dealer-1",
		CustomerID:    "cust-1",
		VehicleID:     "veh-1",
		ServiceTypeID: "svc-1",
		Start:         nextMondayAt(10, 0).Format(time.RFC3339Nano),
	}
	_, _, err := svc.Confirm(context.Background(), "req-1", "", req)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("bay reserve fail: want ErrUnavailable, got %v", err)
	}
	if got := len(ft.released); got != 1 {
		t.Fatalf("tech Release calls: want 1, got %d", got)
	}
	if got := ft.released[0]; got != "tech-occ-1" {
		t.Fatalf("released tech occupation: want tech-occ-1, got %s", got)
	}
	// No appointment row persisted: the HELD insert is compensated before it
	// would be reached — nothing is written after the failed claim.
	if n := len(fs.rows); n != 0 {
		t.Fatalf("no appointment row must be held after a failed claim, got %d", n)
	}
}

// withinHours helper (unexported, same package): inside / before-open /
// missing weekday = closed.
func TestWithinHours(t *testing.T) {
	snap := Snapshot{
		Timezone: "UTC",
		OpeningHours: map[int]OpeningHours{
			1: {Weekday: 1, OpenMinutes: 480, CloseMinutes: 1020},
		},
	}
	monday10 := nextMondayAt(10, 0)
	if !withinHours(snap, monday10) {
		t.Fatal("Monday 10:00 must be within Mon 08:00–17:00")
	}
	if withinHours(snap, nextMondayAt(7, 30)) {
		t.Fatal("Monday 07:30 must be outside hours")
	}
	sunday := monday10.AddDate(0, 0, -1)
	if withinHours(snap, sunday) {
		t.Fatal("Sunday (no hours row) must be closed")
	}
}

// fingerprint helper (unexported, same package): stable and order-sensitive.
func TestFingerprint(t *testing.T) {
	a := fingerprint("", "d1", "c1", "v1", "s1", "2026-09-07T10:00:00Z")
	if a == "" {
		t.Fatal("fingerprint must not be empty")
	}
	b := fingerprint("", "d1", "c1", "v1", "s1", "2026-09-07T10:00:00Z")
	if a != b {
		t.Fatal("same inputs must produce the same fingerprint")
	}
	c := fingerprint("", "d1", "c1", "v1", "s1", "2026-09-07T11:00:00Z")
	if a == c {
		t.Fatal("different start must change the fingerprint")
	}
}

// --- Phase 2 tests ---------------------------------------------------------

// seedHeld inserts a HELD row directly into the fake store (bypasses the
// catalog/saga): dealer-1/cust-1/veh-1/svc-1, Monday 10:00 UTC.
func seedHeld(t *testing.T, fs *fakeStore, id string) AppointmentRow {
	t.Helper()
	start := nextMondayAt(10, 0)
	tech, bay, to, bo := "tech-1", "bay-1", "tech-occ-seed", "bay-occ-seed"
	row := AppointmentRow{
		ID:               id,
		DealershipID:     "dealer-1",
		CustomerID:       "cust-1",
		VehicleID:        "veh-1",
		ServiceTypeID:    "svc-1",
		TechnicianID:     &tech,
		ServiceBayID:     &bay,
		TechOccupationID: &to,
		BayOccupationID:  &bo,
		StartAt:          start,
		EndAt:            start.Add(2 * time.Hour),
		Status:           StatusHeld,
	}
	if _, err := fs.InsertHeld(context.Background(), row); err != nil {
		t.Fatalf("seed HELD row: %v", err)
	}
	return row
}

// Confirm a HELD hold with key K → 201; replay with only {holdId} and the same
// key → 200 (fingerprint from the row, not the body); no Releases.
func TestConfirmHold_ReplayWithHoldIDOnly_200(t *testing.T) {
	svc, fs, ft, fb := unitService(t)
	ctx := context.Background()
	seedHeld(t, fs, "appt-hold-1")

	first, status, err := svc.Confirm(ctx, "req-1", "key-K", ConfirmRequest{HoldID: "appt-hold-1"})
	if err != nil || status != 201 {
		t.Fatalf("first confirm: want 201, got %d err %v", status, err)
	}
	if first.Status != StatusConfirmed {
		t.Fatalf("first confirm status: want CONFIRMED, got %s", first.Status)
	}

	replay, status, err := svc.Confirm(ctx, "req-2", "key-K", ConfirmRequest{HoldID: "appt-hold-1"})
	if err != nil || status != 200 {
		t.Fatalf("replay with {holdId} only: want 200, got %d err %v", status, err)
	}
	if replay.ID != "appt-hold-1" {
		t.Fatalf("replay must return the same appointment, got %s", replay.ID)
	}
	if len(ft.released) != 0 || len(fb.released) != 0 {
		t.Fatalf("replay must not release: tech %v bay %v", ft.released, fb.released)
	}
}

// Unknown holdId → 404 (ErrNotFound); cancelled holdId → 409 (ErrUnavailable,
// not ErrInvalid/400).
func TestConfirmHold_UnknownNotFound_CancelledConflict(t *testing.T) {
	svc, fs, _, _ := unitService(t)
	ctx := context.Background()
	seedHeld(t, fs, "appt-hold-2")
	if _, err := fs.Cancel(ctx, "appt-hold-2"); err != nil {
		t.Fatalf("seed cancel: %v", err)
	}

	_, _, err := svc.Confirm(ctx, "req-1", "", ConfirmRequest{HoldID: "appt-missing"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hold: want ErrNotFound, got %v", err)
	}

	_, _, err = svc.Confirm(ctx, "req-2", "", ConfirmRequest{HoldID: "appt-hold-2"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancelled hold: want ErrUnavailable (409), got %v", err)
	}
	if errors.Is(err, ErrInvalid) {
		t.Fatal("cancelled hold must not map to 400/ErrInvalid")
	}
}

// P0 concurrent-promote guard, scripted: the first Get sees the real HELD row
// (so confirmHold enters promoteConfirmed), Promote fails with ErrNotFound
// without flipping the row, and the post-Promote re-Get shows the winner's
// CONFIRMED row → the loser does zero Release calls (it must never release the
// winner's occupations).
func TestPromoteConfirmed_ConcurrentWinner_NoReleases(t *testing.T) {
	svc, fs, ft, fb := unitService(t)
	ctx := context.Background()
	seedHeld(t, fs, "appt-race-1")

	fs.mu.Lock()
	fs.promoteErr = ErrNotFound // loser's CAS miss: row no longer HELD
	fs.getAfterPromoteFail = map[string]AppointmentRow{
		"appt-race-1": {
			ID:               "appt-race-1",
			DealershipID:     "dealer-1",
			CustomerID:       "cust-1",
			VehicleID:        "veh-1",
			ServiceTypeID:    "svc-1",
			TechOccupationID: strPtr("tech-occ-seed"),
			BayOccupationID:  strPtr("bay-occ-seed"),
			Status:           StatusConfirmed,
		},
	}
	fs.mu.Unlock()

	_, _, err := svc.Confirm(ctx, "req-1", "key-K", ConfirmRequest{HoldID: "appt-race-1"})
	if err != nil {
		t.Fatalf("loser must succeed as replay, got err %v", err)
	}
	fs.mu.Lock()
	calls := fs.promoteCalls
	fs.mu.Unlock()
	if calls < 1 {
		t.Fatal("Promote must have been invoked (non-vacuous script)")
	}
	if len(ft.released) != 0 || len(fb.released) != 0 {
		t.Fatalf("loser must not release the winner's occupations: tech %v bay %v",
			ft.released, fb.released)
	}
}

// Two goroutines confirm the same HELD holdId with the same key: no panic, no
// cross-row releases, and the row ends CONFIRMED.
func TestConfirmHold_ConcurrentSameHold_NoCrossRowRelease(t *testing.T) {
	svc, fs, ft, fb := unitService(t)
	ctx := context.Background()
	seedHeld(t, fs, "appt-race-2")

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			defer wg.Done()
			_, _, err := svc.Confirm(ctx, fmt.Sprintf("req-%d", i), "key-K",
				ConfirmRequest{HoldID: "appt-race-2"})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil && !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("confirm %d: unexpected error %v", i, err)
		}
	}
	row, err := fs.Get(ctx, "appt-race-2")
	if err != nil || row.Status != StatusConfirmed {
		t.Fatalf("row must end CONFIRMED, got %s err %v", row.Status, err)
	}
	// tech-occ-seed / bay-occ-seed ARE the winner's occupations: no confirmer
	// of this hold may Release them, loser or not.
	if len(ft.released) != 0 || len(fb.released) != 0 {
		t.Fatalf("same-hold concurrent confirm must not Release winner occupations: tech %v bay %v",
			ft.released, fb.released)
	}
}

// Empty Idempotency-Key: Promote stores NULL (nil pointer), never '' — two
// empty-key confirms must not unique-conflict because of ''.
func TestPromote_EmptyKey_NilNotEmptyString(t *testing.T) {
	svc, fs, _, _ := unitService(t)
	ctx := context.Background()
	seedHeld(t, fs, "appt-empty-1")
	seedHeld(t, fs, "appt-empty-2")

	for _, id := range []string{"appt-empty-1", "appt-empty-2"} {
		if _, status, err := svc.Confirm(ctx, "req-1", "", ConfirmRequest{HoldID: id}); err != nil || status != 201 {
			t.Fatalf("empty-key confirm of %s: want 201, got %d err %v", id, status, err)
		}
	}
	for _, id := range []string{"appt-empty-1", "appt-empty-2"} {
		row, err := fs.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if row.IDempotencyKey != nil {
			t.Fatalf("%s: empty key must store NULL, got %q", id, *row.IDempotencyKey)
		}
	}
}

// Sweeper drop-out: a CANCELLED row with occupation ids is Released then
// cleared; a second sweep releases nothing; a failed Release RPC keeps the ids
// set for the next tick.
func TestSweepOnce_ReleasesThenClears_FailedReleaseRetries(t *testing.T) {
	svc, fs, ft, fb := unitService(t)
	ctx := context.Background()
	seedHeld(t, fs, "appt-sweep-1")
	if _, err := fs.Cancel(ctx, "appt-sweep-1"); err != nil {
		t.Fatalf("seed cancel: %v", err)
	}

	svc.sweepOnce(ctx)

	// First pass: both Released exactly once, ids cleared.
	if len(ft.released) != 1 || ft.released[0] != "tech-occ-seed" {
		t.Fatalf("tech releases after first sweep: want [tech-occ-seed], got %v", ft.released)
	}
	if len(fb.released) != 1 || fb.released[0] != "bay-occ-seed" {
		t.Fatalf("bay releases after first sweep: want [bay-occ-seed], got %v", fb.released)
	}
	row, err := fs.Get(ctx, "appt-sweep-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.TechOccupationID != nil || row.BayOccupationID != nil {
		t.Fatalf("ids must be cleared after successful releases, got tech=%v bay=%v",
			row.TechOccupationID, row.BayOccupationID)
	}

	// Second pass: nothing left to release (drop-out: ids already nil).
	svc.sweepOnce(ctx)
	if len(ft.released) != 1 || len(fb.released) != 1 {
		t.Fatalf("second sweep must not release again: tech %v bay %v", ft.released, fb.released)
	}

	// Failure path: a row whose Release RPC errors keeps its ids.
	seedHeld(t, fs, "appt-sweep-2")
	if _, err := fs.Cancel(ctx, "appt-sweep-2"); err != nil {
		t.Fatalf("seed cancel 2: %v", err)
	}
	ft.mu.Lock()
	ft.releaseErr = errors.New("grpc down")
	ft.mu.Unlock()
	svc.sweepOnce(ctx)
	row2, err := fs.Get(ctx, "appt-sweep-2")
	if err != nil {
		t.Fatalf("get 2: %v", err)
	}
	if row2.TechOccupationID == nil || row2.BayOccupationID == nil {
		t.Fatal("failed Release must keep both occupation ids set (retry next tick)")
	}
	if row2.Status != StatusCancelled {
		t.Fatalf("failed-release row must stay CANCELLED, got %s", row2.Status)
	}
}
