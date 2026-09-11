package fieldconfig

import (
	"encoding/json"
	"testing"
)

// The JSON a form renderer receives. Every key it binds to is asserted here,
// because a missing tag marshals as Go's field name — Locked, Key, DataType —
// and the form would silently read undefined for all of them.
func TestResolvedMarshalsForTheForm(t *testing.T) {
	cfg := Resolve(EntityOrder, []Override{
		{Entity: EntityOrder, Key: "items", Requirement: Required, LabelOverride: "Rincian barang"},
	})

	raw, err := json.Marshal(cfg.Fields["items"])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{"entity", "key", "dataType", "locked", "default", "group", "label", "sort", "requirement", "overridden"} {
		if _, ok := got[key]; !ok {
			t.Errorf("field %q missing from the JSON: %s", key, raw)
		}
	}

	// The company's wording wins and reaches the client under `label` — the
	// declared label is shadowed rather than sent alongside, so a form has one
	// thing to read.
	if got["label"] != "Rincian barang" {
		t.Errorf("label = %v, want the company's override", got["label"])
	}
	if got["locked"] != false {
		t.Errorf("locked = %v, want false", got["locked"])
	}

	locked, _ := json.Marshal(cfg.Fields["originWarehouseId"])
	var lockedGot map[string]any
	_ = json.Unmarshal(locked, &lockedGot)
	if lockedGot["locked"] != true {
		t.Errorf("originWarehouseId locked = %v, want true", lockedGot["locked"])
	}
}
