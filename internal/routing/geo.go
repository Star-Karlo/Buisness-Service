package routing

import "math"

// Haversine is the great-circle distance between two points, in metres.
//
// Used for two things, and it is worth being clear that it is right for one and
// only an approximation for the other:
//
//   - measuring a stretch of an already-computed route, where consecutive
//     geometry points are metres apart and the error is negligible;
//   - ranking candidate trucks by how near they are to a loading point, where
//     it is a straight line and the road is longer.
//
// The ranking use is deliberate. Routing every truck in a fleet to find the
// nearest would be one MAPID call per candidate, which is slow and billable,
// and straight-line distance orders candidates correctly almost always. The
// road route is then computed once, for the truck the planner actually picks.
func Haversine(a, b Point) float64 {
	const earthRadiusMeters = 6371000.0

	lat1 := a.Lat * math.Pi / 180
	lat2 := b.Lat * math.Pi / 180
	dLat := (b.Lat - a.Lat) * math.Pi / 180
	dLon := (b.Lon - a.Lon) * math.Pi / 180

	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1)*math.Cos(lat2)*math.Sin(dLon/2)*math.Sin(dLon/2)

	return 2 * earthRadiusMeters * math.Asin(math.Sqrt(h))
}

// WithinGeofence reports whether a position is inside a circular fence.
//
// radiusMeters of 0 means the warehouse has no fence configured, which returns
// false — "no fence" is not "everywhere". Callers record the result rather than
// enforcing it, so an unconfigured warehouse produces an honest "not verified"
// instead of a blocked driver.
func WithinGeofence(position, centre Point, radiusMeters int) bool {
	if radiusMeters <= 0 {
		return false
	}
	return Haversine(position, centre) <= float64(radiusMeters)
}
