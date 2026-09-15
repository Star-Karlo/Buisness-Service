package unit

import (
	"testing"

	"github.com/karlo/business-service/internal/platform/query"
	"github.com/karlo/business-service/internal/routing"
)

// TestProfileValidation pins the four profiles MAPID actually has.
//
// Asking for one it does not know is refused by name, so the list is declared
// here rather than passed through as free text. `small_truck` is the trap: it
// exists in GraphHopper's documentation and in other providers, and MAPID does
// not have it.
func TestProfileValidation(t *testing.T) {
	for _, p := range []routing.Profile{
		routing.ProfileTruck, routing.ProfileCar,
		routing.ProfileMotorcycle, routing.ProfileFoot,
	} {
		if !p.Valid() {
			t.Errorf("%q must be a valid MAPID profile", p)
		}
	}
	for _, p := range []routing.Profile{"small_truck", "bike", "scooter", "driving", ""} {
		if p.Valid() {
			t.Errorf("%q is not a MAPID profile and must be refused", p)
		}
	}
}

// TestTollSegmentStatus covers a distinction that is easy to flatten and
// expensive to get wrong.
//
// MAPID marks a stretch "all" when it is tolled and "missing" where the map
// data says nothing. "missing" is NOT "free" — treating it as free would price
// a journey as cheaper than it is, and the error would only surface when
// somebody reconciled an invoice against the actual toll receipts.
func TestTollSegmentStatus(t *testing.T) {
	if !(routing.TollSegment{Status: "all"}).Tolled() {
		t.Error(`"all" marks a tolled stretch`)
	}
	if (routing.TollSegment{Status: "missing"}).Tolled() {
		t.Error(`"missing" means the data is silent, not that the road is free`)
	}
	if (routing.TollSegment{Status: ""}).Tolled() {
		t.Error("an empty status must not read as tolled")
	}
}

// TestUnconfiguredClientFailsCleanly covers a deployment with no key.
//
// Routing then has to fail in a way that says so, rather than sending
// unauthenticated requests that MAPID rejects one at a time — which would look
// like an outage rather than a missing setting.
func TestUnconfiguredClientFailsCleanly(t *testing.T) {
	c := routing.New("", "")
	if c.Configured() {
		t.Fatal("a client with no key must report itself unconfigured")
	}

	_, err := c.Route(t.Context(), routing.Request{
		Points: []routing.Point{{Lon: 110.4, Lat: -6.9}, {Lon: 112.7, Lat: -7.3}},
	})
	if err == nil {
		t.Fatal("routing without a key must fail")
	}
	if err != routing.ErrNotConfigured {
		t.Errorf("expected ErrNotConfigured, got %v", err)
	}
}

// TestRouteNeedsTwoPoints covers the check that happens before any network
// call, since a one-point route is a caller mistake rather than a MAPID one.
func TestRouteNeedsTwoPoints(t *testing.T) {
	c := routing.New("", "a-key")
	for _, points := range [][]routing.Point{
		nil,
		{{Lon: 110.4, Lat: -6.9}},
	} {
		if _, err := c.Route(t.Context(), routing.Request{Points: points}); err == nil {
			t.Errorf("a route with %d point(s) must be refused", len(points))
		}
	}
}

// TestFilterValueAcceptsBothShapes covers the two spellings clients send for a
// multi-value filter, and the failure each used to produce.
//
// The array form `["draft","submitted"]` could not unmarshal into a string
// field, which aborted the WHOLE filter list and returned every row — a broken
// filter that looks like a working one whenever the unfiltered result happens
// to be small. The string form "draft,submitted" parsed into one value, so
// `IN ('draft,submitted')` matched nothing and looked like "no data". Both were
// 200 responses, which is why neither was noticed.
func TestFilterValueAcceptsBothShapes(t *testing.T) {
	fields := query.FieldSet{"statusCode": "status_code"}

	for _, tc := range []struct {
		name, filtered string
		want           []string
	}{
		{"array", `[{"id":"statusCode","value":["draft","submitted"],"type":"in"}]`,
			[]string{"draft", "submitted"}},
		{"comma-separated string", `[{"id":"statusCode","value":"draft,submitted","type":"in"}]`,
			[]string{"draft", "submitted"}},
		{"single scalar", `[{"id":"statusCode","value":"draft","type":"in"}]`,
			[]string{"draft"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := query.Parse("0", "20", tc.filtered, "", "", fields)
			if p.Err != nil {
				t.Fatalf("unexpected error: %v", p.Err)
			}
			if len(p.Filters) != 1 {
				t.Fatalf("expected one filter, got %d", len(p.Filters))
			}
			got := p.Filters[0].Values
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestUnknownFilterFieldIsRefused covers the other silent failure: a filter on
// a field that is not in the allowlist used to be dropped, so the request
// returned every row with a 200. The caller believes their filter applied.
func TestUnknownFilterFieldIsRefused(t *testing.T) {
	fields := query.FieldSet{"statusCode": "status_code"}

	p := query.Parse("0", "20", `[{"id":"status","value":"draft","type":"equal"}]`, "", "", fields)
	if p.Err == nil {
		t.Fatal("filtering on an unknown field must be refused, not ignored: " +
			"ignoring it returns every row and reads as success")
	}
	if len(p.Filters) != 0 {
		t.Error("a rejected request must carry no filters, so a handler that " +
			"forgets to check Err returns nothing rather than everything")
	}

	// Malformed JSON is the same shape of mistake.
	if p := query.Parse("0", "20", `not json`, "", "", fields); p.Err == nil {
		t.Error("a malformed filter list must be refused")
	}
}
