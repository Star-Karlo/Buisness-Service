package services

import (
	"context"
	"testing"

	"github.com/karlo/business-service/internal/repository"
)

func TestATwoEndedJourneyIsWatchedAtItsTwoEnds(t *testing.T) {
	// No stop repository: the watcher behaves exactly as it always did.
	w := &GeofenceWatcher{}
	origin, destination := "wh-origin", "wh-destination"
	got := w.fencesFor(context.Background(), repository.ActiveShipment{
		OriginWarehouseID:      &origin,
		DestinationWarehouseID: &destination,
	})
	if len(got) != 2 || got[0] != origin || got[1] != destination {
		t.Fatalf("fencesFor = %v, want the order's two warehouses", got)
	}
}

func TestAnOrderMissingAWarehouseIsNotWatchedAtNowhere(t *testing.T) {
	w := &GeofenceWatcher{}
	origin := "wh-origin"
	empty := ""
	got := w.fencesFor(context.Background(), repository.ActiveShipment{
		OriginWarehouseID:      &origin,
		DestinationWarehouseID: &empty,
	})
	if len(got) != 1 || got[0] != origin {
		t.Fatalf("fencesFor = %v, want only the warehouse it names", got)
	}
}

// The multi-stop branch needs a database to list stops from, so it is covered
// in the integration suite. What is checked here is the fallback — the path
// every two-ended order takes, and the one a failure to read stops lands on.
