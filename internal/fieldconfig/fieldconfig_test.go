package fieldconfig

import "testing"

func TestDefaultsApplyWhenNothingIsConfigured(t *testing.T) {
	cfg := Resolve(EntityOrder, nil)

	// The two switches this package exists for start off.
	if got := cfg.RequirementOf("items"); got != Hidden {
		t.Errorf("items = %q, want hidden by default", got)
	}
	if got := cfg.RequirementOf("originWarehouseId"); got != Required {
		t.Errorf("originWarehouseId = %q, want required", got)
	}
}

func TestOverrideTurnsAFieldOn(t *testing.T) {
	cfg := Resolve(EntityOrder, []Override{
		{Entity: EntityOrder, Key: "items", Requirement: Required},
	})

	if got := cfg.RequirementOf("items"); got != Required {
		t.Fatalf("items = %q, want required", got)
	}
	if !cfg.Fields["items"].Overridden {
		t.Error("an overridden field should say so, for the configurator's benefit")
	}
}

// A Locked field is the one an order cannot do without. An override on it
// should not exist — SetOverride refuses to write one — but a row arriving some
// other way must not be able to make orders unsubmittable.
func TestLockedFieldsIgnoreOverrides(t *testing.T) {
	cfg := Resolve(EntityOrder, []Override{
		{Entity: EntityOrder, Key: "originWarehouseId", Requirement: Hidden},
	})

	if got := cfg.RequirementOf("originWarehouseId"); got != Required {
		t.Errorf("a locked field was reconfigured to %q", got)
	}
	if err := CheckOverride(EntityOrder, "originWarehouseId", Hidden); err == nil {
		t.Error("CheckOverride should refuse a locked field")
	}
}

// An override for a key the code no longer declares must not resurrect it.
func TestUndeclaredOverridesAreDropped(t *testing.T) {
	cfg := Resolve(EntityOrder, []Override{
		{Entity: EntityOrder, Key: "fieldWeRemoved", Requirement: Required},
	})

	if _, present := cfg.Fields["fieldWeRemoved"]; present {
		t.Error("a stored override resurrected a field the code does not declare")
	}
}

func TestValidateReportsEverythingAtOnce(t *testing.T) {
	cfg := Resolve(EntityOrder, []Override{
		{Entity: EntityOrder, Key: "cargoTypeId", Requirement: Required},
	})

	err := cfg.Validate(map[string]bool{})
	if err == nil {
		t.Fatal("an empty submission should not validate")
	}

	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("got %T, want *ValidationError", err)
	}
	// Four required fields on a default order form; the point is that all of
	// them come back together rather than one per round trip.
	if len(ve.Missing) < 2 {
		t.Errorf("only %d missing fields reported: %v", len(ve.Missing), ve.Missing)
	}
}

// The refusal that keeps a hidden field honest. Silently dropping the value
// looks identical to saving it from the caller's side.
func TestHiddenFieldsAreRefusedNotIgnored(t *testing.T) {
	cfg := Resolve(EntityOrder, nil)

	err := cfg.Validate(map[string]bool{
		"originWarehouseId":      true,
		"destinationWarehouseId": true,
		"customerId":             true,
		"agreementId":            true,
		"pickupAt":               true,
		"cargoTypeId":            true,
		"items":                  true, // hidden for this company
	})
	if err == nil {
		t.Fatal("a hidden field carrying a value should be refused")
	}

	ve := err.(*ValidationError)
	if len(ve.NotWanted) != 1 || ve.NotWanted[0] != "items" {
		t.Errorf("notWanted = %v, want [items]", ve.NotWanted)
	}
}

func TestSatisfiedSubmissionPasses(t *testing.T) {
	cfg := Resolve(EntityOrder, nil)

	present := map[string]bool{}
	for key, f := range cfg.Fields {
		if f.Requirement == Required {
			present[key] = true
		}
	}

	if err := cfg.Validate(present); err != nil {
		t.Errorf("a complete submission was refused: %v", err)
	}
}
