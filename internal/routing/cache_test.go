package routing

import "testing"

// The property the whole cache rests on: the same lane must produce the same
// key. Coordinates make a round trip through JSON and Postgres NUMERIC between
// one order and the next, and without rounding the last bits do not survive —
// which would make the cache appear to work while missing almost every time.
func TestCacheKeyIsStableUnderFloatNoise(t *testing.T) {
	a := []Point{{Lon: 106.845599, Lat: -6.208763}, {Lon: 110.421768, Lat: -6.966667}}
	b := []Point{{Lon: 106.8455991, Lat: -6.2087631}, {Lon: 110.4217679, Lat: -6.9666671}}

	if CacheKey(a, ProfileTruck, false) != CacheKey(b, ProfileTruck, false) {
		t.Error("float noise below a metre produced a different cache key")
	}
}

// Rounding must not go so far that genuinely different places collide.
func TestCacheKeySeparatesDifferentPlaces(t *testing.T) {
	jakarta := []Point{{Lon: 106.845599, Lat: -6.208763}, {Lon: 110.421768, Lat: -6.966667}}
	shifted := []Point{{Lon: 106.855599, Lat: -6.208763}, {Lon: 110.421768, Lat: -6.966667}}

	if CacheKey(jakarta, ProfileTruck, false) == CacheKey(shifted, ProfileTruck, false) {
		t.Error("a kilometre apart shared a cache key")
	}
}

// A truck route and a car route between the same points are different roads, so
// they must not share an entry.
func TestCacheKeyDistinguishesProfileAndTollPreference(t *testing.T) {
	points := []Point{{Lon: 106.845599, Lat: -6.208763}, {Lon: 110.421768, Lat: -6.966667}}

	truck := CacheKey(points, ProfileTruck, false)
	car := CacheKey(points, ProfileCar, false)
	noTolls := CacheKey(points, ProfileTruck, true)

	if truck == car {
		t.Error("truck and car routes shared a cache key")
	}
	if truck == noTolls {
		t.Error("toll-avoiding and normal routes shared a cache key")
	}
}

// Direction matters: Jakarta to Semarang is not Semarang to Jakarta.
func TestCacheKeyIsDirectional(t *testing.T) {
	a := Point{Lon: 106.845599, Lat: -6.208763}
	b := Point{Lon: 110.421768, Lat: -6.966667}

	if CacheKey([]Point{a, b}, ProfileTruck, false) == CacheKey([]Point{b, a}, ProfileTruck, false) {
		t.Error("a reversed route shared its cache key")
	}
}

func TestHaversineMatchesKnownDistance(t *testing.T) {
	// Monas, Jakarta to Simpang Lima, Semarang: about 400 km great-circle.
	jakarta := Point{Lon: 106.827183, Lat: -6.175392}
	semarang := Point{Lon: 110.421768, Lat: -6.983333}

	got := Haversine(jakarta, semarang)
	if got < 390_000 || got > 420_000 {
		t.Errorf("Jakarta to Semarang = %.0f m, expected roughly 400 km", got)
	}
}

// An unconfigured geofence is "no fence", not "everywhere". Returning true
// would mark every arrival as verified on a warehouse nobody has set up.
func TestGeofenceWithNoRadiusIsNotEverywhere(t *testing.T) {
	p := Point{Lon: 106.827183, Lat: -6.175392}
	if WithinGeofence(p, p, 0) {
		t.Error("a warehouse with no configured radius reported the driver inside it")
	}
	if !WithinGeofence(p, p, 100) {
		t.Error("a driver standing on the pin was reported outside a 100 m fence")
	}
}
