package models

import "testing"

func TestShipmentStatusAtOrPast(t *testing.T) {
	cases := []struct {
		status, mark string
		want         bool
	}{
		{ShipmentUnloading, ShipmentLoading, true},  // further along
		{ShipmentLoading, ShipmentLoading, true},    // exactly there
		{ShipmentLoading, ShipmentUnloading, false}, // not yet
		{ShipmentFinished, ShipmentToUnloading, true},
		{ShipmentCancelled, ShipmentLoading, false}, // off the chain
		{"nonsense", ShipmentLoading, false},
	}
	for _, c := range cases {
		if got := ShipmentStatusAtOrPast(c.status, c.mark); got != c.want {
			t.Errorf("ShipmentStatusAtOrPast(%q, %q) = %v, want %v", c.status, c.mark, got, c.want)
		}
	}
}

func TestNextShipmentStatusTowards(t *testing.T) {
	// The step from toUnloading to unloading is the one an interleaved
	// journey needs and the table does not allow in one hop.
	if got := NextShipmentStatusTowards(ShipmentToUnloading, ShipmentUnloading); got != ShipmentAtUnloading {
		t.Errorf("toUnloading → unloading should step through atUnloading, got %q", got)
	}
	if got := NextShipmentStatusTowards(ShipmentLoaded, ShipmentUnloading); got != ShipmentToUnloading {
		t.Errorf("loaded → unloading should step to toUnloading, got %q", got)
	}
	// The legacy approval states are skipped rather than walked into.
	if got := NextShipmentStatusTowards(ShipmentAtLoading, ShipmentLoading); got != ShipmentLoading {
		t.Errorf("atLoading → loading should skip loadingApproved, got %q", got)
	}
	// Nowhere to go: already there, already past, or off the chain.
	for _, c := range [][2]string{
		{ShipmentUnloading, ShipmentUnloading},
		{ShipmentUnloaded, ShipmentLoading},
		{ShipmentCancelled, ShipmentFinished},
		{ShipmentLoading, "nonsense"},
	} {
		if got := NextShipmentStatusTowards(c[0], c[1]); got != "" {
			t.Errorf("NextShipmentStatusTowards(%q, %q) = %q, want no step", c[0], c[1], got)
		}
	}
}

func TestEveryChainStepCanBeWalkedBySomebody(t *testing.T) {
	// The walk is given the role of whoever caused it — the driver for a
	// stop event, the system for an approval. Every step must be open to one
	// of them, or a journey strands halfway along the chain.
	for i := range shipmentChain {
		from := shipmentChain[i]
		to := NextShipmentStatusTowards(from, ShipmentFinished)
		if to == "" {
			continue
		}
		driver := CanTransitionShipment(from, to, RoleDriver)
		system := CanTransitionShipment(from, to, RoleSystem)
		if driver != nil && system != nil {
			t.Errorf("neither the driver nor the system can walk %s → %s (driver: %v, system: %v)",
				from, to, driver, system)
		}
	}
}

func TestTheStepsAnInterleavedJourneyNeedsBelongToTheDriver(t *testing.T) {
	// A driver who starts unloading at the first drop-off while a loading
	// point is still outstanding takes these three steps. If the system had
	// to take them the shipment would never reach `unloading`, and the
	// unloading POD would be refused for disagreeing with it.
	for _, step := range [][2]string{
		{ShipmentLoaded, ShipmentToUnloading},
		{ShipmentToUnloading, ShipmentAtUnloading},
		{ShipmentAtUnloading, ShipmentUnloading},
	} {
		if err := CanTransitionShipment(step[0], step[1], RoleDriver); err != nil {
			t.Errorf("the driver cannot walk %s → %s: %v", step[0], step[1], err)
		}
	}
	// And the two that end a stage are the system's, taken on approval.
	for _, step := range [][2]string{
		{ShipmentUnloading, ShipmentUnloaded},
		{ShipmentUnloaded, ShipmentFinished},
	} {
		if err := CanTransitionShipment(step[0], step[1], RoleSystem); err != nil {
			t.Errorf("the system cannot walk %s → %s: %v", step[0], step[1], err)
		}
	}
}
