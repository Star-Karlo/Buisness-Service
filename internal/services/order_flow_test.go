package services

import (
	"errors"
	"testing"

	"github.com/karlo/business-service/internal/models"
)

func orderWith(detail models.JSONB) *models.Order {
	return &models.Order{Detail: detail}
}

func TestOrderFlowReadsTheShapeNotAFlag(t *testing.T) {
	single := orderWith(models.JSONB{
		"loadingPoints":   []interface{}{"a"},
		"unloadingPoints": []interface{}{"b"},
	})
	if got := OrderFlow(single); got != FlowSingle {
		t.Fatalf("one pair of points is the single-shipment flow, got %q", got)
	}

	multi := orderWith(models.JSONB{
		"loadingPoints":   []interface{}{"a", "c"},
		"unloadingPoints": []interface{}{"b", "d"},
	})
	if got := OrderFlow(multi); got != FlowMulti {
		t.Fatalf("two pairs of points is the multi-shipment flow, got %q", got)
	}

	// An order from before the point lists existed carries only the two
	// warehouse columns, and must keep running the flow it always ran.
	if got := OrderFlow(orderWith(models.JSONB{})); got != FlowSingle {
		t.Fatalf("an order with no point lists is single-shipment, got %q", got)
	}
}

func TestStopsArePairedIntoShipments(t *testing.T) {
	order := orderWith(models.JSONB{
		"loadingPoints":   []interface{}{"jakarta", "bandung"},
		"unloadingPoints": []interface{}{"semarang", "priok"},
	})

	stops := stopsForOrder(order)
	if len(stops) != 4 {
		t.Fatalf("want 4 stops, got %d", len(stops))
	}

	// Default order: both loads, then both unloads.
	want := []struct {
		kind       string
		shipmentNo int16
		warehouse  string
	}{
		{models.StopLoad, 1, "jakarta"},
		{models.StopLoad, 2, "bandung"},
		{models.StopUnload, 1, "semarang"},
		{models.StopUnload, 2, "priok"},
	}
	for i, w := range want {
		got := stops[i]
		if got.Seq != int16(i+1) {
			t.Errorf("stop %d: seq = %d, want %d", i, got.Seq, i+1)
		}
		if got.Kind != w.kind || got.ShipmentNo != w.shipmentNo || got.WarehouseID != w.warehouse {
			t.Errorf("stop %d: got %s/%d/%s, want %s/%d/%s",
				i, got.Kind, got.ShipmentNo, got.WarehouseID, w.kind, w.shipmentNo, w.warehouse)
		}
	}
}

func TestPlannerVisitOrderIsFollowedAndPairingSurvivesIt(t *testing.T) {
	// Muat 1 → Bongkar 1 → Muat 2 → Bongkar 2: deliver the first shipment
	// before picking the second one up.
	order := orderWith(models.JSONB{
		"loadingPoints":   []interface{}{"jakarta", "bandung"},
		"unloadingPoints": []interface{}{"semarang", "priok"},
		"stopSequence": []interface{}{
			map[string]interface{}{"type": "muat", "index": float64(0)},
			map[string]interface{}{"type": "bongkar", "index": float64(0)},
			map[string]interface{}{"type": "muat", "index": float64(1)},
			map[string]interface{}{"type": "bongkar", "index": float64(1)},
		},
	})

	stops := stopsForOrder(order)
	if len(stops) != 4 {
		t.Fatalf("want 4 stops, got %d", len(stops))
	}
	wantOrder := []string{"jakarta", "semarang", "bandung", "priok"}
	for i, w := range wantOrder {
		if stops[i].WarehouseID != w {
			t.Fatalf("visit %d: got %s, want %s", i+1, stops[i].WarehouseID, w)
		}
		if stops[i].Seq != int16(i+1) {
			t.Errorf("visit %d: seq = %d", i+1, stops[i].Seq)
		}
	}
	// Reordering the visits must not renumber the shipments: Semarang still
	// belongs to shipment 1 and Priok to shipment 2.
	if stops[1].ShipmentNo != 1 || stops[3].ShipmentNo != 2 {
		t.Errorf("pairing followed the visit order: %d then %d",
			stops[1].ShipmentNo, stops[3].ShipmentNo)
	}
}

func TestBongkarCannotPrecedeItsOwnMuat(t *testing.T) {
	order := orderWith(models.JSONB{
		"loadingPoints":   []interface{}{"jakarta", "bandung"},
		"unloadingPoints": []interface{}{"semarang", "priok"},
		"stopSequence": []interface{}{
			map[string]interface{}{"type": "bongkar", "index": float64(0)},
			map[string]interface{}{"type": "muat", "index": float64(0)},
			map[string]interface{}{"type": "muat", "index": float64(1)},
			map[string]interface{}{"type": "bongkar", "index": float64(1)},
		},
	})
	err := ValidateStopSequence(order)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("want a validation error, got %v", err)
	}

	// The truck must not be routed through a sequence that was refused, so the
	// builder falls back to the default rather than honouring it.
	stops := stopsForOrder(order)
	if stops[0].WarehouseID != "jakarta" {
		t.Errorf("an impossible sequence was followed anyway: first stop %s", stops[0].WarehouseID)
	}
}

func TestSequenceMustNameEveryPointOnce(t *testing.T) {
	cases := map[string][]interface{}{
		"a point left out": {
			map[string]interface{}{"type": "muat", "index": float64(0)},
			map[string]interface{}{"type": "bongkar", "index": float64(0)},
		},
		"a point named twice": {
			map[string]interface{}{"type": "muat", "index": float64(0)},
			map[string]interface{}{"type": "muat", "index": float64(0)},
			map[string]interface{}{"type": "bongkar", "index": float64(0)},
			map[string]interface{}{"type": "bongkar", "index": float64(1)},
		},
		"an index past the end": {
			map[string]interface{}{"type": "muat", "index": float64(0)},
			map[string]interface{}{"type": "muat", "index": float64(5)},
			map[string]interface{}{"type": "bongkar", "index": float64(0)},
			map[string]interface{}{"type": "bongkar", "index": float64(1)},
		},
	}
	for name, seq := range cases {
		t.Run(name, func(t *testing.T) {
			order := orderWith(models.JSONB{
				"loadingPoints":   []interface{}{"jakarta", "bandung"},
				"unloadingPoints": []interface{}{"semarang", "priok"},
				"stopSequence":    seq,
			})
			if err := ValidateStopSequence(order); !errors.Is(err, ErrValidation) {
				t.Fatalf("want a validation error, got %v", err)
			}
		})
	}
}

func TestSingleShipmentOrderIsUntouchedByAnyOfThis(t *testing.T) {
	// The flow that already works must keep producing exactly the two stops
	// it produced before shipments were paired or sequences existed.
	order := orderWith(models.JSONB{
		"loadingPoints":   []interface{}{"jakarta"},
		"unloadingPoints": []interface{}{"semarang"},
	})
	stops := stopsForOrder(order)
	if len(stops) != 2 {
		t.Fatalf("want 2 stops, got %d", len(stops))
	}
	if stops[0].ShipmentNo != 1 || stops[1].ShipmentNo != 1 {
		t.Errorf("both ends of a single shipment are shipment 1, got %d and %d",
			stops[0].ShipmentNo, stops[1].ShipmentNo)
	}
	if err := ValidateStopSequence(order); err != nil {
		t.Errorf("a single-shipment order has no sequence to validate: %v", err)
	}
}
