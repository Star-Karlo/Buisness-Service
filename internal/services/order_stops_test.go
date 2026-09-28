package services

import (
	"testing"

	"github.com/karlo/business-service/internal/models"
)

// Turning the wizard's point lists into a visit list.
//
// The console has always sent loadingPoints and unloadingPoints; the server
// read neither and used the order's two warehouse columns, which hold the
// FIRST loading point and the LAST unloading point. A journey with a stop in
// the middle therefore had no record of it anywhere.
func TestStopsForOrder(t *testing.T) {
	id := func(s string) *string { return &s }

	t.Run("three points become three stops in visit order", func(t *testing.T) {
		order := &models.Order{
			OriginWarehouseID:      id("WH-JKT"),
			DestinationWarehouseID: id("WH-PRIOK"),
			Detail: models.JSONB{
				"loadingPoints":   []interface{}{"WH-JKT"},
				"unloadingPoints": []interface{}{"WH-SMG", "WH-PRIOK"},
				"unloadingPics": []interface{}{
					map[string]interface{}{"name": "Budi", "phone": "0811"},
					map[string]interface{}{"name": "Sari", "phone": "0812"},
				},
			},
		}
		stops := stopsForOrder(order)
		if len(stops) != 3 {
			t.Fatalf("got %d stops, want 3 — the middle one is the whole point", len(stops))
		}
		want := []struct {
			seq  int16
			kind string
			wh   string
		}{{1, models.StopLoad, "WH-JKT"}, {2, models.StopUnload, "WH-SMG"}, {3, models.StopUnload, "WH-PRIOK"}}
		for i, w := range want {
			if stops[i].Seq != w.seq || stops[i].Kind != w.kind || stops[i].WarehouseID != w.wh {
				t.Errorf("stop %d = {%d %s %s}, want {%d %s %s}",
					i, stops[i].Seq, stops[i].Kind, stops[i].WarehouseID, w.seq, w.kind, w.wh)
			}
		}
		if stops[1].PICName == nil || *stops[1].PICName != "Budi" {
			t.Errorf("Semarang's PIC = %v, want Budi — the PIC lists are positional", stops[1].PICName)
		}
		if stops[2].PICPhone == nil || *stops[2].PICPhone != "0812" {
			t.Errorf("Priok's PIC phone = %v, want 0812", stops[2].PICPhone)
		}
	})

	t.Run("no lists falls back to the order's two ends", func(t *testing.T) {
		stops := stopsForOrder(&models.Order{
			OriginWarehouseID:      id("WH-A"),
			DestinationWarehouseID: id("WH-B"),
			Detail:                 models.JSONB{},
		})
		if len(stops) != 2 || stops[0].WarehouseID != "WH-A" || stops[1].WarehouseID != "WH-B" {
			t.Fatalf("got %+v, want the two warehouse columns as one load and one unload", stops)
		}
	})

	t.Run("an empty trip with no destination has no stops", func(t *testing.T) {
		if stops := stopsForOrder(&models.Order{OriginWarehouseID: id("WH-A"), Detail: models.JSONB{}}); len(stops) != 0 {
			t.Errorf("got %d stops for a repositioning trip, want none", len(stops))
		}
	})

	t.Run("blank ids in the list are skipped, and numbering stays contiguous", func(t *testing.T) {
		stops := stopsForOrder(&models.Order{
			OriginWarehouseID:      id("WH-A"),
			DestinationWarehouseID: id("WH-C"),
			Detail: models.JSONB{
				"loadingPoints":   []interface{}{"WH-A"},
				"unloadingPoints": []interface{}{"", "WH-C"},
			},
		})
		if len(stops) != 2 {
			t.Fatalf("got %d stops, want 2", len(stops))
		}
		if stops[1].Seq != 2 {
			t.Errorf("seq = %d, want 2: a blank point must not leave a hole in the numbering", stops[1].Seq)
		}
	})
}
