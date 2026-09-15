package routing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"time"
)

// CacheKey identifies a route request by its content.
//
// Keying by content rather than by the order that asked is the whole point. The
// same warehouse pair is routed over and over — every order on a lane, every
// re-open of the planner — and the roads between two fixed points do not change
// between Tuesday and Wednesday. An order-keyed cache would never hit, because
// every order is new.
//
// Coordinates are rounded to five decimal places before hashing. That is about
// a metre at the equator, which is far below any warehouse geofence, and it is
// what makes two requests for the same warehouse agree: the coordinate arrives
// as a float through JSON, through Postgres NUMERIC and back, and the last bits
// do not survive that intact. Without rounding the cache would appear to work
// and quietly miss most of the time — the worst kind of failure, because it
// looks like a cache.
func CacheKey(points []Point, profile Profile, avoidTolls bool) string {
	h := sha256.New()

	_, _ = fmt.Fprintf(h, "v1|%s|%t|", profile, avoidTolls)
	for _, p := range points {
		h.Write([]byte(strconv.FormatFloat(round5(p.Lon), 'f', 5, 64)))
		h.Write([]byte{','})
		h.Write([]byte(strconv.FormatFloat(round5(p.Lat), 'f', 5, 64)))
		h.Write([]byte{';'})
	}

	return hex.EncodeToString(h.Sum(nil))
}

func round5(v float64) float64 {
	const factor = 1e5
	// The +0.5 rounding is done on the absolute value so that negative
	// longitudes — which Indonesia does not have, but a mis-signed coordinate
	// does — round the same way as positive ones rather than towards zero.
	if v < 0 {
		return -float64(int64(-v*factor+0.5)) / factor
	}
	return float64(int64(v*factor+0.5)) / factor
}

// Entry is a cached route, as stored.
type Entry struct {
	CacheKey   string
	Profile    string
	AvoidTolls bool
	Points     []Point

	Route *Route

	// HasToll and TollDistanceMeters are derived at write time so that "does
	// this lane cross a toll road" is an indexed boolean rather than a scan
	// through the segment list on every read.
	HasToll            bool
	TollDistanceMeters int
}

// Store is the persistence the cache needs. It is an interface so this package
// does not depend on the repository layer, which depends on it.
type Store interface {
	Lookup(ctx context.Context, key string) (*Entry, error)
	Save(ctx context.Context, e *Entry) error
}

// Cache wraps a Client so callers get a route without knowing whether it was
// computed or remembered.
type Cache struct {
	client *Client
	store  Store
}

func NewCache(client *Client, store Store) *Cache {
	return &Cache{client: client, store: store}
}

func (c *Cache) Configured() bool { return c.client.Configured() }

// Result is a route plus where it came from.
type Result struct {
	Key    string
	Route  *Route
	Cached bool

	HasToll            bool
	TollDistanceMeters int
}

// Route returns a route, from the cache when possible.
//
// A cache failure is never fatal. If the store cannot be read the route is
// computed; if it cannot be written the route is still returned. Routing that
// stops working because a cache table is unhappy would take dispatch down with
// it, and the cache exists to save money, not to be a dependency.
func (c *Cache) Route(ctx context.Context, req Request) (*Result, error) {
	if req.Profile == "" {
		req.Profile = ProfileTruck
	}

	key := CacheKey(req.Points, req.Profile, req.AvoidTolls)

	if entry, err := c.store.Lookup(ctx, key); err != nil {
		slog.WarnContext(ctx, "route cache lookup failed, computing instead", "error", err)
	} else if entry != nil {
		return &Result{
			Key: key, Route: entry.Route, Cached: true,
			HasToll: entry.HasToll, TollDistanceMeters: entry.TollDistanceMeters,
		}, nil
	}

	// Toll detail is always requested, even when the caller did not ask. It is
	// what the allowance screen needs to estimate a toll cost, and asking for
	// it later would mean a second MAPID call for a route already computed —
	// paying twice to save nothing, since the cache stores whatever it fetched.
	req.WithTollSegments = true

	route, err := c.client.Route(ctx, req)
	if err != nil {
		return nil, err
	}

	hasToll, tollMeters := tollSummary(route)

	entry := &Entry{
		CacheKey: key, Profile: string(req.Profile), AvoidTolls: req.AvoidTolls,
		Points: req.Points, Route: route,
		HasToll: hasToll, TollDistanceMeters: tollMeters,
	}
	if err := c.store.Save(ctx, entry); err != nil {
		slog.WarnContext(ctx, "route cache save failed", "error", err, "key", key)
	}

	return &Result{
		Key: key, Route: route, Cached: false,
		HasToll: hasToll, TollDistanceMeters: tollMeters,
	}, nil
}

// tollSummary works out how much of a route is tolled.
//
// GraphHopper reports toll status over index ranges into the geometry, not over
// distances, so the tolled distance has to be measured along the line. A
// segment whose status is "missing" is NOT counted as free — absent toll data
// is not evidence of a free road — but it is not counted as tolled either,
// which is the honest reading and means an estimate can be low but never
// invented.
func tollSummary(r *Route) (bool, int) {
	var tolled float64
	var has bool

	for _, seg := range r.TollSegments {
		if !seg.Tolled() {
			continue
		}
		has = true
		tolled += lineLength(r.Geometry, seg.From, seg.To)
	}

	return has, int(tolled)
}

// lineLength measures a stretch of the geometry in metres.
func lineLength(geometry [][]float64, from, to int) float64 {
	if from < 0 {
		from = 0
	}
	if to > len(geometry)-1 {
		to = len(geometry) - 1
	}

	var total float64
	for i := from; i < to; i++ {
		a, b := geometry[i], geometry[i+1]
		if len(a) < 2 || len(b) < 2 {
			continue
		}
		total += Haversine(Point{Lon: a[0], Lat: a[1]}, Point{Lon: b[0], Lat: b[1]})
	}
	return total
}

// CacheTTL is how long an entry stays usable.
//
// Long, because the failure mode of a slightly stale route is a distance a few
// percent out rather than a wrong answer, and roads change on the timescale of
// years. Short enough that a genuinely new toll road is picked up within a
// quarter.
const CacheTTL = 90 * 24 * time.Hour
