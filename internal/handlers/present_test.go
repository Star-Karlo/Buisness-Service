package handlers

import "testing"

// The distinction this whole helper exists for: a zero value that was SENT is
// not the same submission as one that was omitted, and a bound struct cannot
// tell them apart.
func TestZeroIsPresentButOmittedIsNot(t *testing.T) {
	got := presentKeys([]byte(`{"quantity":0,"weightKg":12.5}`))

	if !got["quantity"] {
		t.Error("a quantity of 0 was sent and should count as present")
	}
	if got["volumeM3"] {
		t.Error("volumeM3 was never sent and should not count as present")
	}
}

// A client clearing a field sends null. Treating that as present would let it
// satisfy a required field with nothing in it.
func TestExplicitNullIsAbsent(t *testing.T) {
	got := presentKeys([]byte(`{"customerId":null,"referenceNumber":"AB-1"}`))

	if got["customerId"] {
		t.Error("an explicit null should not count as present")
	}
	if !got["referenceNumber"] {
		t.Error("referenceNumber was sent and should count as present")
	}
}

// Nested objects contribute dotted paths, matching the catalogue's keys.
func TestNestedObjectsBecomeDottedPaths(t *testing.T) {
	got := presentKeys([]byte(`{"route":{"originCityId":"abc","originDistrictId":"def"}}`))

	for _, key := range []string{"route", "route.originCityId", "route.originDistrictId"} {
		if !got[key] {
			t.Errorf("%q should be present", key)
		}
	}
}

// The catalogue declares `items` as one list-typed field, not a field per
// element, so an array is marked present and not descended into.
func TestArraysAreMarkedButNotDescended(t *testing.T) {
	got := presentKeys([]byte(`{"items":[{"name":"Roll Kain","weightKg":18}]}`))

	if !got["items"] {
		t.Error("items should be present")
	}
	if got["items.name"] || got["name"] {
		t.Error("array elements should not contribute their own keys")
	}
}

func TestUnparseableBodyYieldsNothing(t *testing.T) {
	if len(presentKeys([]byte(`not json`))) != 0 {
		t.Error("an unparseable body should yield no keys, leaving binding to report it")
	}
}

// The agreement form's route and pricing inputs live on rate rows, but the
// catalogue declares them as form-level paths. foldRateKeys is the bridge.
func TestRateKeysFoldUpToCataloguePaths(t *testing.T) {
	present := map[string]bool{}
	foldRateKeys(present, []rateRequest{
		{OriginCityID: "jkt", DestinationCityID: "smg"},
		{OriginCityID: "jkt", DestinationCityID: "smg", OriginDistrictID: "kec-1"},
	})

	// Supplied by the second row only — "in use" is any row, not every row.
	if !present["route.originDistrictId"] {
		t.Error("a kecamatan on one rate row should mark the field in use")
	}
	if present["route.destinationDistrictId"] {
		t.Error("no row supplied a destination kecamatan")
	}
	// Price is present because a rate row exists: zero is a real price on a
	// backhaul, so presence cannot depend on the number being non-zero.
	if !present["price"] {
		t.Error("price should be present whenever a rate row exists")
	}
	if present["minQuantity"] {
		t.Error("no row supplied a minimum quantity")
	}
}
