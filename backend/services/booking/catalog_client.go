// Package bookingclient is Booking's fail-closed Catalog snapshot client
// (write-path step 1): plain HTTP over openapi/catalog.yaml endpoints. No
// cache — the write path never trusts a cache, and Catalog down → reject.
package booking

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Snapshot is everything the write path needs from Catalog. Occupancy duration
// is NOT here — end_at comes from Technician Reserve (skill duration).
type Snapshot struct {
	DealershipID   string
	Timezone       string // IANA
	OpeningHours   map[int]OpeningHours
	ServiceTypeOK  bool
	VehicleBelongs bool
}

type OpeningHours struct {
	Weekday      int `json:"weekday"`
	OpenMinutes  int `json:"openMinutes"`
	CloseMinutes int `json:"closeMinutes"`
}

type dealershipDTO struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Timezone     string         `json:"timezone"`
	OpeningHours []OpeningHours `json:"openingHours"`
}

type serviceTypeDTO struct {
	ID string `json:"id"`
}

type vehicleDTO struct {
	ID         string `json:"id"`
	CustomerID string `json:"customerId"`
}

// errNotFound: Catalog answered but the resource does not exist (404).
var errCatalogNotFound = errCatalog("catalog resource not found")

// errCatalogDown: Catalog unreachable or non-2xx (fail closed).
var errCatalogDown = errCatalog("catalog unavailable")

type errCatalog string

func (e errCatalog) Error() string { return string(e) }

// Client is a read-only Catalog HTTP client.
type CatalogClient struct {
	BaseURL string
	HTTP    *http.Client
}

func NewCatalogClient(baseURL string) *CatalogClient {
	return &CatalogClient{
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 3 * time.Second},
	}
}

// Snapshot fetches dealership (TZ + hours), service-type existence, and
// vehicle→customer ownership in three GETs. Any failure is fail-closed.
func (c *CatalogClient) Snapshot(ctx context.Context, dealershipID, serviceTypeID, customerID, vehicleID string) (Snapshot, error) {
	var snap Snapshot

	// Dealership + hours.
	resp, err := c.get(ctx, "/dealerships")
	if err != nil {
		return snap, errCatalogDown
	}
	var dealers []dealershipDTO
	if err := json.Unmarshal(resp, &dealers); err != nil {
		return snap, errCatalogDown
	}
	found := false
	for _, d := range dealers {
		if d.ID == dealershipID {
			found = true
			snap.DealershipID = d.ID
			snap.Timezone = d.Timezone
			snap.OpeningHours = map[int]OpeningHours{}
			for _, h := range d.OpeningHours {
				snap.OpeningHours[h.Weekday] = h
			}
			break
		}
	}
	if !found {
		return snap, errCatalogNotFound
	}

	// Service type exists (durationMinutes ignored for occupancy).
	resp, err = c.get(ctx, "/service-types")
	if err != nil {
		return snap, errCatalogDown
	}
	var types []serviceTypeDTO
	if err := json.Unmarshal(resp, &types); err != nil {
		return snap, errCatalogDown
	}
	for _, st := range types {
		if st.ID == serviceTypeID {
			snap.ServiceTypeOK = true
			break
		}
	}
	if !snap.ServiceTypeOK {
		return snap, errCatalogNotFound
	}

	// Vehicle belongs to customer.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/customers/"+customerID+"/vehicles", nil)
	if err != nil {
		return snap, errCatalogDown
	}
	hres, err := c.HTTP.Do(req)
	if err != nil {
		return snap, errCatalogDown
	}
	defer hres.Body.Close()
	if hres.StatusCode == http.StatusNotFound {
		return snap, errCatalogNotFound
	}
	if hres.StatusCode != http.StatusOK {
		return snap, errCatalogDown
	}
	var vehicles []vehicleDTO
	if err := json.NewDecoder(hres.Body).Decode(&vehicles); err != nil {
		return snap, errCatalogDown
	}
	for _, v := range vehicles {
		if v.ID == vehicleID {
			snap.VehicleBelongs = true
			break
		}
	}
	if !snap.VehicleBelongs {
		return snap, errCatalogNotFound
	}
	return snap, nil
}

func (c *CatalogClient) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog %s: %s", path, resp.Status)
	}
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 2048)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if n == 0 || err != nil {
			break
		}
	}
	return buf, nil
}
