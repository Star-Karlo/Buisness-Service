package handlers

import (
	"testing"

	"github.com/karlo/business-service/internal/fieldconfig"
)

// A company that has switched warehouse lanes OFF for its ordinary contracts
// must still be able to write multi-customer ones.
//
// Those name warehouses because of what they ARE — each customer covered has
// its own two — not because the company chose to price lanes that way. Marking
// lanes.* present for them tied the contract to an option about ordinary
// contracts, and a hidden field is REFUSED rather than ignored, so switching
// that option off would have refused every multi-customer contract.
func TestWarehouseLanesAreIntrinsicToAMultiCustomerContract(t *testing.T) {
	warehouses := []interface{}{"wh-a"}

	cases := []struct {
		name          string
		agreementType string
		wantPresent   bool
	}{
		{"ordinary contract still answers for the option", "single-shipment", true},
		{"multi-shipment likewise", "multi-shipment", true},
		{"multi-customer does not", "multi-customer", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			present := map[string]bool{}
			foldDetailKeys(present, map[string]any{
				"agreementType":   tc.agreementType,
				"loadingPoints":   warehouses,
				"unloadingPoints": warehouses,
			})
			for _, key := range []string{"lanes.loadingPoints", "lanes.unloadingPoints"} {
				if present[key] != tc.wantPresent {
					t.Errorf("%s present = %v, want %v", key, present[key], tc.wantPresent)
				}
			}
		})
	}
}

// The whole point of the above: with the option hidden, a multi-customer
// contract carrying warehouses is accepted and an ordinary one is refused.
func TestHiddenWarehouseLanesRefuseOnlyOrdinaryContracts(t *testing.T) {
	cfg := fieldconfig.Resolve(fieldconfig.EntityAgreement, []fieldconfig.Override{
		{Key: "lanes.loadingPoints", Requirement: fieldconfig.Hidden},
		{Key: "lanes.unloadingPoints", Requirement: fieldconfig.Hidden},
	})

	// Everything the catalogue demands, so the only thing under test is what
	// the two lane fields do. Without this the required-field check fires
	// first and the Hidden rule is never reached.
	satisfyRequired := func() map[string]bool {
		present := map[string]bool{}
		for key, f := range cfg.Fields {
			if f.Requirement == fieldconfig.Required {
				present[key] = true
			}
		}
		return present
	}

	multi := satisfyRequired()
	foldDetailKeys(multi, map[string]any{
		"agreementType":   "multi-customer",
		"loadingPoints":   []interface{}{"wh-a"},
		"unloadingPoints": []interface{}{"wh-b"},
	})
	if err := cfg.Validate(multi); err != nil {
		t.Errorf("a multi-customer contract was refused: %v", err)
	}

	ordinary := satisfyRequired()
	foldDetailKeys(ordinary, map[string]any{
		"agreementType":   "single-shipment",
		"loadingPoints":   []interface{}{"wh-a"},
		"unloadingPoints": []interface{}{"wh-b"},
	})
	if err := cfg.Validate(ordinary); err == nil {
		t.Error("an ordinary contract supplied warehouses while the option is hidden, and was accepted")
	}
}
