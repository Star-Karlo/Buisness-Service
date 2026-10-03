package handlers

import "testing"

func TestFoldDetailKeysLiftsTheContractOptions(t *testing.T) {
	present := map[string]bool{}
	foldDetailKeys(present, map[string]any{
		"loadingPoints":   []interface{}{"wh-1"},
		"unloadingPoints": []interface{}{"wh-2"},
		"multiCustomers":  []interface{}{map[string]any{"customerName": "PT Dua"}},
		"billingSplit":    "proportional",
		"allowance":       map[string]interface{}{"total": 1000.0, "upfrontPercent": 60.0},
		// Free-form detail declares no field and must not be lifted.
		"kotaAsal": "KOTA BEKASI",
	})
	for _, key := range []string{
		"lanes.loadingPoints", "lanes.unloadingPoints",
		"multiCustomers", "billingSplit", "allowance.upfrontPercent",
	} {
		if !present[key] {
			t.Errorf("%s was supplied but not recorded — a company with it switched off would not be refused", key)
		}
	}
	if present["kotaAsal"] {
		t.Error("kotaAsal is free-form detail and declares no field; lifting it would invent a configuration key")
	}
}

func TestAnEmptyValueIsNotASuppliedField(t *testing.T) {
	// The console sends empty lists for a contract that named no warehouses.
	// Counting those as supplied would refuse an ordinary contract under a
	// company that has the field switched off.
	present := map[string]bool{}
	foldDetailKeys(present, map[string]any{
		"loadingPoints":   []interface{}{},
		"unloadingPoints": nil,
		"billingSplit":    "",
		"allowance":       map[string]interface{}{},
	})
	for key := range present {
		t.Errorf("%s was recorded from an empty value", key)
	}
}
