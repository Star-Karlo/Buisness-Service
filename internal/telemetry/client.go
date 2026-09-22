// Package telemetry reads vehicle positions from the Karlo FMS tracking
// service.
//
// That service is the authority on where a truck is. Everything else in this
// system infers position from something that already happened — the last
// completed shipment's unloading coordinate, most usefully — and an inference
// is exactly as stale as the event it came from.
//
// Devices are addressed by IMEI, never by vehicle. The tracking service knows
// nothing about trucks; master data owns the device-to-vehicle link and its
// history, which is what makes "where was THIS truck last March" answerable
// without crediting one truck's journey to whichever vehicle holds the device
// today.
//
// One property is worth knowing before using this: positions come back
// reverse-geocoded, carrying kelurahan, kecamatan, kota/kabupaten and provinsi
// as NAMES. That matches how master data's Site records geography — names, no
// region table — and is the reason a position can be compared to a site's
// district without a lookup that does not exist.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotConfigured is returned when no base URL is set, so a deployment
// without telemetry starts and runs on the fallback rather than failing.
var ErrNotConfigured = errors.New("telemetry: not configured")

// Position is one device's location at one moment.
//
// The administrative names are what the tracking service reverse-geocoded, not
// something derived here. They are advisory: a position on a boundary may name
// either side, and a position at sea names nothing.
type Position struct {
	IMEI    string    `json:"imei"`
	Time    time.Time `json:"time"`
	Lat     float64   `json:"lat"`
	Lon     float64   `json:"lon"`
	Speed   float64   `json:"speed"`
	Bearing float64   `json:"bearing"`

	// Ignition is nil when the device does not report it. Distinguished from
	// false, which means the engine is genuinely off — a truck that cannot say
	// is not a truck that is stopped.
	Ignition *bool `json:"ignition"`

	ReceivedAt time.Time `json:"received_at"`

	Jalan     string `json:"jalan"`
	Kelurahan string `json:"kelurahan"`
	Kecamatan string `json:"kecamatan"`
	Kota      string `json:"kota"`
	Kabupaten string `json:"kabupaten"`
	Provinsi  string `json:"provinsi"`

	Metadata map[string]any `json:"metadata,omitempty"`
}

// City returns the kota or, failing that, the kabupaten.
//
// Indonesian administrative geography uses one or the other and never both, so
// a caller wanting "the city-level name" has to check two fields. Doing it here
// keeps that knowledge in one place instead of at every call site.
func (p Position) City() string {
	if p.Kota != "" {
		return p.Kota
	}
	return p.Kabupaten
}

// Age is how stale this fix is. Worth surfacing wherever a position is shown:
// a fix from three days ago should not be weighed like one from this morning,
// and nothing about a coordinate says which it is.
func (p Position) Age(now time.Time) time.Duration { return now.Sub(p.Time) }

// VehicleDay is one device's aggregate for one day.
type VehicleDay struct {
	IMEI       string  `json:"imei"`
	Day        string  `json:"day"`
	MovingS    int64   `json:"moving_s"`
	IdleS      int64   `json:"idle_s"`
	ParkingS   int64   `json:"parking_s"`
	DistanceKm float64 `json:"distance_km"`
	MaxSpeed   float64 `json:"max_speed"`
	Samples    int64   `json:"samples"`
}

// Client talks to the tracking service.
//
// Two credentials, either of which is enough. The platform service token is
// the normal one: the same internal credential every platform service
// presents to the others, over the private VPC hop, with nothing to hand
// out or rotate separately. The ingest key is the device credential and
// stays only as a fallback for a tracking deployment that has not yet
// learned to accept service tokens.
type Client struct {
	baseURL      string
	serviceToken string
	key          string
	http         *http.Client
}

// New builds a client. An empty base URL yields one whose every call returns
// ErrNotConfigured, so telemetry is optional rather than a startup dependency.
func New(baseURL, serviceToken, ingestKey string) *Client {
	return &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		serviceToken: serviceToken,
		key:          ingestKey,
		// Short. A planner's screen waits on this, and a slow answer is worse
		// than the fallback position it would have used anyway.
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) Configured() bool { return c != nil && c.baseURL != "" }

// Authenticated reports whether reads would carry a credential. The base URL
// has a default, so Configured alone is true on every deployment; a
// background job that would only ever be answered 401 should check this.
func (c *Client) Authenticated() bool {
	return c.Configured() && (c.serviceToken != "" || c.key != "")
}

// maxIMEIsPerRequest is the tracking service's documented ceiling on /v1/live.
// Larger sets are split rather than truncated: silently dropping the tail would
// make a fleet's last trucks permanently invisible.
const maxIMEIsPerRequest = 5000

// Live returns the latest position of each device, keyed by IMEI.
//
// Devices the tracking service has never heard from are simply absent from the
// map rather than present with a zero coordinate. Nought, nought is a real
// place in the Atlantic, and a caller that received it would rank a truck as
// being there.
func (c *Client) Live(ctx context.Context, imeis []string) (map[string]Position, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	out := make(map[string]Position, len(imeis))
	if len(imeis) == 0 {
		return out, nil
	}

	for start := 0; start < len(imeis); start += maxIMEIsPerRequest {
		end := start + maxIMEIsPerRequest
		if end > len(imeis) {
			end = len(imeis)
		}

		q := url.Values{}
		q.Set("imeis", strings.Join(imeis[start:end], ","))

		var batch []Position
		if err := c.get(ctx, "/v1/live?"+q.Encode(), &batch); err != nil {
			return nil, err
		}
		for _, p := range batch {
			if p.IMEI == "" || (p.Lat == 0 && p.Lon == 0) {
				continue
			}
			out[p.IMEI] = p
		}
	}

	return out, nil
}

// LiveOne returns the latest position of a single device.
func (c *Client) LiveOne(ctx context.Context, imei string) (*Position, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	var p Position
	if err := c.get(ctx, "/v1/live/"+url.PathEscape(imei), &p); err != nil {
		return nil, err
	}
	if p.Lat == 0 && p.Lon == 0 {
		return nil, nil
	}
	return &p, nil
}

// HistoryPage is one page of a device's track.
type HistoryPage struct {
	NextCursor string     `json:"next_cursor"`
	Points     []Position `json:"points"`
}

// History returns a device's track over a window.
//
// maxPoints asks the tracking service to downsample rather than returning every
// fix. A day of a moving truck is thousands of points, and a map cannot draw
// them usefully — downsampling at the source avoids transferring what the
// client would immediately discard.
func (c *Client) History(ctx context.Context, imei string, from, to time.Time, maxPoints int) (*HistoryPage, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	q := url.Values{}
	q.Set("from", from.UTC().Format(time.RFC3339))
	q.Set("to", to.UTC().Format(time.RFC3339))
	if maxPoints > 0 {
		q.Set("max_points", fmt.Sprint(maxPoints))
	}

	var page HistoryPage
	if err := c.get(ctx, "/v1/history/"+url.PathEscape(imei)+"?"+q.Encode(), &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// DailyStats returns per-device time-in-state and distance.
//
// The dates are YYYY-MM-DD, not RFC3339 — the tracking service aggregates by
// calendar day, and sending a timestamp is refused rather than truncated.
func (c *Client) DailyStats(ctx context.Context, imeis []string, from, to time.Time) ([]VehicleDay, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}

	q := url.Values{}
	q.Set("from", from.Format("2006-01-02"))
	q.Set("to", to.Format("2006-01-02"))
	if len(imeis) > 0 {
		q.Set("imeis", strings.Join(imeis, ","))
	}

	var days []VehicleDay
	if err := c.get(ctx, "/v1/stats/daily?"+q.Encode(), &days); err != nil {
		return nil, err
	}
	return days, nil
}

// MobileReading is one phone fix, in the shape FMS's mobile ingest takes.
// The driver is the identity: FMS joins it to the truck through the
// driver's current assignment in master data.
type MobileReading struct {
	Driver       string         `json:"driver"`
	Latitude     float64        `json:"latitude"`
	Longitude    float64        `json:"longitude"`
	Speed        *float64       `json:"speed,omitempty"`
	Bearing      *float64       `json:"bearing,omitempty"`
	Altitude     *float64       `json:"altitude,omitempty"`
	BatteryLevel *float64       `json:"batteryLevel,omitempty"`
	GpsCreatedAt string         `json:"gpsCreatedAt,omitempty"`
	IO           map[string]any `json:"io,omitempty"`
}

// IngestMobile forwards a batch of phone fixes to FMS (POST
// /v1/ingest/mobile) under the service token. Returns how many were stored.
func (c *Client) IngestMobile(ctx context.Context, readings []MobileReading) (int, error) {
	if !c.Configured() {
		return 0, ErrNotConfigured
	}
	body, err := json.Marshal(readings)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/ingest/mobile", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("telemetry: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.serviceToken)
	}
	if c.key != "" {
		req.Header.Set("X-Ingest-Key", c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("telemetry: unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("telemetry: ingest returned %d: %s", resp.StatusCode, firstLine(raw))
	}
	var out struct {
		Stored int `json:"stored"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Stored, nil
}

func (c *Client) get(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("telemetry: build request: %w", err)
	}
	if c.serviceToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.serviceToken)
	}
	if c.key != "" {
		req.Header.Set("X-Ingest-Key", c.key)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telemetry: unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("telemetry: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telemetry: %s returned %d: %s", path, resp.StatusCode, firstLine(raw))
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("telemetry: %s returned something unexpected: %w", path, err)
	}
	return nil
}

func firstLine(b []byte) string {
	const max = 300
	if len(b) > max {
		b = b[:max]
	}
	return strings.TrimSpace(string(b))
}
