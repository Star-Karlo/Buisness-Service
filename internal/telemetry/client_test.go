package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The payload below is the real shape returned by /v1/live, including the
// placeholder row the service emits with an imei of "..." and a null island
// coordinate. That row is why Live filters on (0,0): nought, nought is a real
// place in the Atlantic, and a caller that received it would rank a truck as
// being there.
const liveFixture = `[
  {"imei":"...","time":"2026-07-21T12:45:53.082608Z","received_at":"2026-07-21T12:45:53.082608Z",
   "lat":0,"lon":0,"speed":0,"bearing":0,"ignition":null,
   "kecamatan":"","kelurahan":"","kabupaten":"","kota":"","provinsi":"","jalan":"",
   "metadata":{"source":"generic"}},
  {"imei":"350317170950319","time":"2026-09-07T07:24:33Z","received_at":"2026-09-07T07:24:36.20363Z",
   "lat":-6.2128516,"lon":106.5636716,"speed":0,"bearing":335,"ignition":false,
   "kecamatan":"Curug","kelurahan":"Kadu Jaya","kabupaten":"Tangerang","kota":"",
   "provinsi":"Banten","jalan":"Jalan Raya Serang","metadata":{"source":"generic"}},
  {"imei":"860000000000001","time":"2026-09-07T07:20:00Z","received_at":"2026-09-07T07:20:02Z",
   "lat":-6.9,"lon":110.42,"speed":48,"bearing":90,"ignition":true,
   "kecamatan":"Semarang Tengah","kelurahan":"","kabupaten":"","kota":"Semarang",
   "provinsi":"Jawa Tengah","jalan":"","metadata":{}}
]`

func fixtureServer(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "", "")
}

func TestLiveDropsTheNullIslandPlaceholder(t *testing.T) {
	c := fixtureServer(t, liveFixture)

	got, err := c.Live(context.Background(), []string{"...", "350317170950319", "860000000000001"})
	if err != nil {
		t.Fatalf("Live: %v", err)
	}

	if _, present := got["..."]; present {
		t.Error("the (0,0) placeholder row was returned as a position")
	}
	if len(got) != 2 {
		t.Fatalf("got %d positions, want 2", len(got))
	}
}

// Indonesian geography uses kota or kabupaten and never both, so a caller
// wanting "the city-level name" would otherwise have to check two fields.
func TestCityPrefersKotaThenFallsBackToKabupaten(t *testing.T) {
	c := fixtureServer(t, liveFixture)
	got, _ := c.Live(context.Background(), []string{"350317170950319", "860000000000001"})

	if city := got["350317170950319"].City(); city != "Tangerang" {
		t.Errorf("kabupaten-only row City() = %q, want Tangerang", city)
	}
	if city := got["860000000000001"].City(); city != "Semarang" {
		t.Errorf("kota row City() = %q, want Semarang", city)
	}
}

// A device that cannot report ignition is not a device reporting the engine
// off, so the field is a pointer and null must survive as nil.
func TestNullIgnitionIsDistinctFromFalse(t *testing.T) {
	c := fixtureServer(t, liveFixture)
	got, _ := c.Live(context.Background(), []string{"350317170950319", "860000000000001"})

	reported := got["350317170950319"]
	if reported.Ignition == nil || *reported.Ignition {
		t.Error("an explicit false ignition should survive as a non-nil false")
	}
	running := got["860000000000001"]
	if running.Ignition == nil || !*running.Ignition {
		t.Error("an explicit true ignition should survive as a non-nil true")
	}
}

func TestPositionCarriesKecamatanForDistrictMatching(t *testing.T) {
	c := fixtureServer(t, liveFixture)
	got, _ := c.Live(context.Background(), []string{"350317170950319"})

	if got["350317170950319"].Kecamatan != "Curug" {
		t.Errorf("kecamatan = %q, want Curug", got["350317170950319"].Kecamatan)
	}
}

func TestAgeReportsStaleness(t *testing.T) {
	c := fixtureServer(t, liveFixture)
	got, _ := c.Live(context.Background(), []string{"860000000000001"})

	now := time.Date(2026, 9, 7, 8, 20, 0, 0, time.UTC)
	if age := got["860000000000001"].Age(now); age != time.Hour {
		t.Errorf("Age = %v, want 1h", age)
	}
}

// An unconfigured client must fail cleanly rather than sending requests to an
// empty host, so telemetry stays optional.
func TestUnconfiguredClientRefusesRatherThanDialling(t *testing.T) {
	c := New("", "", "")
	if c.Configured() {
		t.Fatal("a client with no base URL should not report itself configured")
	}
	if _, err := c.Live(context.Background(), []string{"1"}); err != ErrNotConfigured {
		t.Errorf("Live error = %v, want ErrNotConfigured", err)
	}
}

func TestEmptyRequestSkipsTheCallEntirely(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, "", "").Live(context.Background(), nil)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if called {
		t.Error("asking for no devices should not reach the network")
	}
	if len(got) != 0 {
		t.Errorf("got %d positions, want 0", len(got))
	}
}

func TestIngestKeyIsSentWhenConfigured(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Ingest-Key")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	_, _ = New(srv.URL, "", "secret").Live(context.Background(), []string{"1"})
	if seen != "secret" {
		t.Errorf("X-Ingest-Key = %q, want secret", seen)
	}
}

func TestReadsCarryTheServiceTokenAsBearer(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "svc-token", "")
	if _, err := c.Live(context.Background(), []string{"1"}); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer svc-token" {
		t.Fatalf("Authorization = %q", got.Get("Authorization"))
	}
	if got.Get("X-Ingest-Key") != "" {
		t.Fatalf("no ingest key was configured, but one was sent: %q", got.Get("X-Ingest-Key"))
	}
	if !c.Authenticated() {
		t.Fatal("a service token alone must count as authenticated")
	}
}
