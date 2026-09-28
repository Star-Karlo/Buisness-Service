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

// stopsForOrder derives the visit list. Loading points first in the order the
// planner listed them, then unloading points, numbered across the whole
// journey so "which is next" is one comparison.
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
	seq := int16(1)
	add := func(kind string, ids []string, pics []stopPIC) {
		for i, id := range ids {
			if id == "" {
				continue
			}
			stop := models.OrderStop{Seq: seq, Kind: kind, WarehouseID: id}
			// The PIC lists are positional, and a planner may have filled
			// some and not others.
			if i < len(pics) {
				if n := pics[i].Name; n != "" {
					stop.PICName = &n
				}
				if p := pics[i].Phone; p != "" {
					stop.PICPhone = &p
				}
			}
			stops = append(stops, stop)
			seq++
		}
	}
	add(models.StopLoad, loads, loadPICs)
	add(models.StopUnload, unloads, unloadPICs)
	return stops
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
func (s *OrderService) SyncStops(ctx context.Context, order *models.Order) error {
	if s.stops == nil {
		return nil
	}
	return s.stops.Replace(ctx, order.ID, stopsForOrder(order))
}
