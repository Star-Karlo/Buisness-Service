// Package routing turns two points into a road route, using MAPID.
//
// MAPID wraps GraphHopper, which is worth knowing because the request and
// response shapes are GraphHopper's and its documentation is the one that
// applies. What MAPID adds is Indonesian coverage and the key.
//
// The client lives in a backend service rather than in the browser for one
// reason that is not negotiable: the request carries an API key. A map widget
// calling MAPID directly would ship that key to every visitor, and a key in a
// public bundle is a key that belongs to whoever finds it.
package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

// Profile is the vehicle a route is planned for.
//
// MAPID exposes exactly four; asking for anything else is refused by name, so
// they are declared here rather than passed through as free text.
type Profile string

const (
	// ProfileTruck respects lorry restrictions — weight and height limits, and
	// roads closed to goods vehicles. It is the right default for this system:
	// planning a truck's journey on a car profile produces a route the driver
	// cannot legally take, and the difference is not obvious on a map.
	ProfileTruck Profile = "truck"

	ProfileCar        Profile = "car"
	ProfileMotorcycle Profile = "motorcycle"
	ProfileFoot       Profile = "foot"
)

// Valid reports whether MAPID knows this profile.
func (p Profile) Valid() bool {
	switch p {
	case ProfileTruck, ProfileCar, ProfileMotorcycle, ProfileFoot:
		return true
	}
	return false
}

// Point is a coordinate in the order MAPID expects: longitude first.
//
// This is GeoJSON order and the reverse of how people say it, which is the
// single easiest thing to get wrong here — transposed coordinates for Indonesia
// land in the Indian Ocean or out of bounds entirely, so the mistake usually
// surfaces as a routing error rather than a wrong route.
type Point struct {
	Lon float64
	Lat float64
}

// Request asks for one route.
type Request struct {
	// From, To and any intermediate stops, in order.
	Points []Point

	Profile Profile

	// AvoidTolls plans around toll roads. Worth being explicit about the cost:
	// on Semarang to Surabaya it saves the toll but adds about twenty minutes,
	// so it is a commercial decision rather than a better route.
	AvoidTolls bool

	// WithTollSegments asks which parts of the route are tolled, so a caller
	// can price the journey or show the driver where the gates are.
	WithTollSegments bool
}

// TollSegment marks a stretch of the route as tolled or not.
//
// From and To index into the geometry's coordinate list, which is how
// GraphHopper expresses "this part of the line". Status is "all" for a tolled
// stretch and "missing" where the data says nothing — NOT "free". The
// distinction matters: absent toll data is not evidence of a free road.
type TollSegment struct {
	From   int    `json:"from"`
	To     int    `json:"to"`
	Status string `json:"status"`
}

// Tolled reports whether this stretch is known to be tolled.
func (s TollSegment) Tolled() bool { return s.Status == "all" }

// Route is one planned journey.
type Route struct {
	DistanceMeters float64 `json:"distanceMeters"`
	Duration       time.Duration

	// Geometry is the road line as [lon, lat] pairs.
	Geometry [][]float64 `json:"geometry"`

	// BBox is [minLon, minLat, maxLon, maxLat], for fitting a map to the route
	// without walking every coordinate.
	BBox []float64 `json:"bbox"`

	// TollSegments is populated only when it was asked for.
	TollSegments []TollSegment `json:"tollSegments,omitempty"`
}

// DurationSeconds is what a JSON client wants; a Go Duration marshals as
// nanoseconds, which reads as a nonsense integer on the other side.
func (r Route) DurationSeconds() float64 { return r.Duration.Seconds() }

// Client talks to MAPID.
type Client struct {
	baseURL string
	key     string
	http    *http.Client
}

// ErrNotConfigured is returned when no key is set. Routing then fails cleanly
// rather than sending unauthenticated requests that MAPID rejects one at a
// time.
var ErrNotConfigured = errors.New("routing: MAPID is not configured")

// New builds a client. An empty key yields a client whose every call returns
// ErrNotConfigured, so a deployment without routing starts and runs.
func New(baseURL, key string) *Client {
	if baseURL == "" {
		baseURL = "https://routing.mapid.io/"
	}
	return &Client{
		baseURL: baseURL,
		key:     key,
		// Long routes take seconds to compute; the Semarang–Surabaya lane is
		// around 350km and 2500 geometry points.
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// Configured reports whether routing is available.
func (c *Client) Configured() bool { return c != nil && c.key != "" }

// mapidRequest is the wire shape, which is GraphHopper's.
type mapidRequest struct {
	Profile       string      `json:"profile"`
	Points        [][]float64 `json:"points"`
	PointsEncoded bool        `json:"points_encoded"`
	Instructions  bool        `json:"instructions"`
	Details       []string    `json:"details,omitempty"`

	// Toll avoidance needs the contraction hierarchy disabled, because a
	// custom model changes edge weights and the precomputed shortcuts no
	// longer apply. It makes the request slower; there is no way around it.
	CHDisable   bool         `json:"ch.disable,omitempty"`
	CustomModel *customModel `json:"custom_model,omitempty"`
}

type customModel struct {
	Priority []priorityRule `json:"priority"`
}

type priorityRule struct {
	If         string `json:"if"`
	MultiplyBy string `json:"multiply_by"`
}

type mapidResponse struct {
	Paths []struct {
		Distance float64   `json:"distance"`
		Time     int64     `json:"time"`
		BBox     []float64 `json:"bbox"`
		Points   struct {
			Coordinates [][]float64 `json:"coordinates"`
		} `json:"points"`
		Details struct {
			Toll [][]interface{} `json:"toll"`
		} `json:"details"`
	} `json:"paths"`
	Message string `json:"message"`
}

// Route plans a journey.
func (c *Client) Route(ctx context.Context, req Request) (*Route, error) {
	if !c.Configured() {
		return nil, ErrNotConfigured
	}
	if len(req.Points) < 2 {
		return nil, fmt.Errorf("routing: a route needs at least two points, got %d", len(req.Points))
	}
	if req.Profile == "" {
		req.Profile = ProfileTruck
	}
	if !req.Profile.Valid() {
		return nil, fmt.Errorf("routing: MAPID has no %q profile (car, truck, motorcycle, foot)", req.Profile)
	}

	points := make([][]float64, 0, len(req.Points))
	for _, p := range req.Points {
		points = append(points, []float64{p.Lon, p.Lat})
	}

	body := mapidRequest{
		Profile:       string(req.Profile),
		Points:        points,
		PointsEncoded: false,
		Instructions:  false,
	}
	if req.WithTollSegments || req.AvoidTolls {
		body.Details = []string{"toll"}
	}
	if req.AvoidTolls {
		body.CHDisable = true
		body.CustomModel = &customModel{
			Priority: []priorityRule{{If: "toll == ALL", MultiplyBy: "0"}},
		}
	}

	raw, err := c.post(ctx, body)
	if err != nil {
		return nil, err
	}

	var parsed mapidResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("routing: MAPID returned something that is not a route: %w", err)
	}
	if len(parsed.Paths) == 0 {
		if parsed.Message != "" {
			return nil, fmt.Errorf("routing: MAPID found no route: %s", parsed.Message)
		}
		return nil, errors.New("routing: MAPID found no route between those points")
	}

	p := parsed.Paths[0]
	out := &Route{
		DistanceMeters: p.Distance,
		Duration:       time.Duration(p.Time) * time.Millisecond,
		Geometry:       p.Points.Coordinates,
		BBox:           p.BBox,
	}

	for _, seg := range p.Details.Toll {
		if len(seg) != 3 {
			continue
		}
		from, okFrom := toInt(seg[0])
		to, okTo := toInt(seg[1])
		status, okStatus := seg[2].(string)
		if okFrom && okTo && okStatus {
			out.TollSegments = append(out.TollSegments, TollSegment{From: from, To: to, Status: status})
		}
	}

	return out, nil
}

// post sends the request, retrying a gateway failure once.
//
// MAPID returns an occasional 502 that succeeds on an immediate retry — it is
// frequent enough to have shown up while exploring the API, and a route request
// is idempotent, so retrying is safe. One retry only: if it fails twice the
// service is genuinely unwell and hammering it does not help.
func (c *Client) post(ctx context.Context, body mapidRequest) ([]byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("routing: encode request: %w", err)
	}

	endpoint := c.baseURL + "?key=" + url.QueryEscape(c.key)

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("routing: build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("routing: MAPID unreachable: %w", err)
			continue
		}

		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("routing: read response: %w", readErr)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			return raw, nil
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("routing: MAPID returned %d", resp.StatusCode)
			slog.WarnContext(ctx, "MAPID gateway error, retrying",
				"status", resp.StatusCode, "attempt", attempt+1)
			continue
		default:
			// A 4xx is our fault and will not improve on a retry. The message
			// is passed through because MAPID's is specific — out of bounds,
			// unknown profile — and far more use than a generic failure.
			return nil, fmt.Errorf("routing: MAPID refused the request (%d): %s",
				resp.StatusCode, firstLine(raw))
		}
	}

	return nil, lastErr
}

func toInt(v interface{}) (int, bool) {
	f, ok := v.(float64)
	return int(f), ok
}

func firstLine(b []byte) string {
	const max = 300
	if len(b) > max {
		b = b[:max]
	}
	return string(bytes.TrimSpace(b))
}
