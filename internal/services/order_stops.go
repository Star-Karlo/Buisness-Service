package services

import (
	"context"

	"github.com/karlo/business-service/internal/models"
)

// Stops are built from the order's own point lists.
//
// The console has always sent loadingPoints and unloadingPoints — the whole
// journey — in the order's detail, and the server read neither. It took
// originWarehouseId and destinationWarehouseId, which the console fills with
// the FIRST loading point and the LAST unloading point, and everything
// downstream believed that was the trip. This turns the lists into rows so a
// journey with a stop in the middle has one.

// stopsForOrder derives the visit list.
//
// Index k of loadingPoints and index k of unloadingPoints are the two ends of
// shipment k+1, so each stop is stamped with its shipment number: that is what
// joins a drop-off to the pick-up whose goods it is delivering, and what a
// per-shipment cargo list, document set or invoice line is looked up by.
// Without it the rows are a bag of points and Shipment 2 does not exist.
//
// The points are then put in the order they will be visited — the planner's,
// if they chose one, otherwise every load and then every unload — and numbered
// 1..N along the way, so "which stop is next" stays one comparison.
//
// Falls back to the order's two warehouse columns when the detail carries no
// lists — an order placed through the API rather than the wizard, or one from
// before the lists existed. Such a journey has exactly two stops, which is
// what it always had.
func stopsForOrder(order *models.Order) []models.OrderStop {
	loads := warehouseList(order.Detail, "loadingPoints")
	unloads := warehouseList(order.Detail, "unloadingPoints")

	if len(loads) == 0 && order.OriginWarehouseID != nil {
		loads = []string{*order.OriginWarehouseID}
	}
	if len(unloads) == 0 && order.DestinationWarehouseID != nil {
		unloads = []string{*order.DestinationWarehouseID}
	}
	if len(loads) == 0 || len(unloads) == 0 {
		// An empty repositioning trip has no cargo and may have no
		// destination. Nothing to visit in sequence.
		return nil
	}

	loadPICs := picList(order.Detail, "loadingPics")
	unloadPICs := picList(order.Detail, "unloadingPics")

	stops := make([]models.OrderStop, 0, len(loads)+len(unloads))
	for _, ref := range visitOrder(order, len(loads), len(unloads)) {
		ids, pics := loads, loadPICs
		if ref.Kind == models.StopUnload {
			ids, pics = unloads, unloadPICs
		}
		if ref.Index >= len(ids) || ids[ref.Index] == "" {
			continue
		}
		if len(stops) >= maxStopsPerOrder {
			// A journey with hundreds of points is a bad order, not a long
			// trip. Stopping here keeps the numbering inside the column's
			// width instead of wrapping it.
			break
		}
		stop := models.OrderStop{
			Seq: stopNo(len(stops) + 1),
			// The shipment this point belongs to is its position in its own
			// list, whatever order the truck visits the points in.
			ShipmentNo:  stopNo(ref.Index + 1),
			Kind:        ref.Kind,
			WarehouseID: ids[ref.Index],
		}
		// The PIC lists are positional, and a planner may have filled some
		// and not others.
		if ref.Index < len(pics) {
			if n := pics[ref.Index].Name; n != "" {
				stop.PICName = &n
			}
			if p := pics[ref.Index].Phone; p != "" {
				stop.PICPhone = &p
			}
		}
		stops = append(stops, stop)
	}
	return stops
}

// A journey is allowed many points, but not so many that a stop number stops
// fitting the column that holds it.
const maxStopsPerOrder = 200

// stopNo narrows a position to the width the column stores.
func stopNo(n int) int16 {
	if n < 1 {
		return 1
	}
	if n > maxStopsPerOrder {
		return maxStopsPerOrder
	}
	return int16(n)
}

type stopPIC struct{ Name, Phone string }

// warehouseList reads a list of warehouse ids out of the order's detail.
func warehouseList(detail models.JSONB, key string) []string {
	raw, ok := detail[key].([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// picList reads the per-point PICs, which the wizard writes alongside the
// points as [{warehouseId, name, phone}].
func picList(detail models.JSONB, key string) []stopPIC {
	raw, ok := detail[key].([]interface{})
	if !ok {
		return nil
	}
	out := make([]stopPIC, 0, len(raw))
	for _, v := range raw {
		m, ok := v.(map[string]interface{})
		if !ok {
			out = append(out, stopPIC{})
			continue
		}
		name, _ := m["name"].(string)
		phone, _ := m["phone"].(string)
		out = append(out, stopPIC{Name: name, Phone: phone})
	}
	return out
}

// SyncStops rebuilds an order's stop list from its detail.
//
// The visit order is checked here rather than deeper down because this is the
// one seam every write passes through: a planner who drags Bongkar 1 above
// Muat 1 is told so, instead of having the reorder silently ignored and then
// wondering why the truck still goes the old way.
func (s *OrderService) SyncStops(ctx context.Context, order *models.Order) error {
	if s.stops == nil {
		return nil
	}
	if err := ValidateStopSequence(order); err != nil {
		return err
	}
	return s.stops.Replace(ctx, order.ID, stopsForOrder(order))
}
