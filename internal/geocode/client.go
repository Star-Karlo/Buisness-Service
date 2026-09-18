// Package geocode talks to the platform's own geocoding service — the
// telemetry side's fms-geocode, which holds Indonesia's kelurahan polygons
// and OSM road names in PostGIS. Forward search ("Jl. Raya Bekasi,
// Cikarang") returns candidates a planner picks from to place a warehouse
// pin; nothing external is called and nothing leaves the VPC.
package geocode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotConfigured is returned when no base URL is set.
var ErrNotConfigured = errors.New("geocode: not configured")

// Candidate is one match for a search.
type Candidate struct {
	// Kind is what matched: road, kelurahan, kecamatan or kabupaten.
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Label is the display string, composed by the service so every
	// caller shows the same thing.
	Label   string `json:"label"`
	Address struct {
		Kelurahan string `json:"kelurahan"`
		Kecamatan string `json:"kecamatan"`
		Kabupaten string `json:"kabupaten"`
		Provinsi  string `json:"provinsi"`
	} `json:"address"`
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type Client struct {
	baseURL      string
	serviceToken string
	http         *http.Client
}

// New builds a client; an empty base URL disables search.
func New(baseURL, serviceToken string) *Client {
	return &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		serviceToken: serviceToken,
		http:         &http.Client{Timeout: 8 * time.Second},
	}
}

func (c *Client) Configured() bool { return c != nil && c.baseURL != "" }

// Search returns up to limit candidates for a free-text query.
func (c *Client) Search(ctx context.Context, q string, limit int) ([]Candidate, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	params := url.Values{}
	params.Set("q", q)
	params.Set("limit", strconv.Itoa(limit))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/search?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("geocode: build request: %w", err)
	}
	if c.serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.serviceToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("geocode: unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("geocode: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("geocode: search returned %d", resp.StatusCode)
	}
	var out []Candidate
	if err := json.Unmarshal(raw, &out); err != nil {
		// Tolerate a wrapped shape, should the service grow one.
		var wrapped struct {
			Items []Candidate `json:"items"`
		}
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil {
			return nil, fmt.Errorf("geocode: decode response: %w", err)
		}
		out = wrapped.Items
	}
	if out == nil {
		out = []Candidate{}
	}
	return out, nil
}
