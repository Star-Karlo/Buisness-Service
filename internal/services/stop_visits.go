package services

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/karlo/business-service/internal/models"
)

// The driver's visit to one stop.
//
// A two-ended journey records arrival on the shipment — arrived_loading_at,
// arrived_unloading_at — which has nowhere to put the second unloading point
// of a three-point trip. These record the visit on the stop itself, so a
// delivery through Semarang and Priok has two arrivals, two cargo checks and
// two PODs rather than one of each and a warehouse nobody recorded reaching.
//
// The shipment's own statuses are untouched: they describe the trip as a
// whole, and the stop rows describe what happened along it.

// StopVisitInput is one report from the gate.
type StopVisitInput struct {
	StopID   uuid.UUID
	Position *Position
	Matches  *bool
	Note     string
}

// ArriveAtStop records that the driver reached one point of the journey.
func (s *ShipmentService) ArriveAtStop(ctx context.Context, actor Actor, shipmentID uuid.UUID, in StopVisitInput) (*models.OrderStop, error) {
	stop, order, err := s.stopForDriver(ctx, actor, shipmentID, in.StopID)
	if err != nil {
		return nil, err
	}
	if stop.ArrivedAt != nil {
		return stop, nil // Arriving twice is not an error; the first one stands.
	}
	if err := s.assertStopIsNext(ctx, order.ID, stop); err != nil {
		return nil, err
	}

	fields := map[string]interface{}{"arrived_at": time.Now()}
	if in.Position != nil {
		fields["arrived_latitude"] = decimal.NewFromFloat(in.Position.Latitude)
		fields["arrived_longitude"] = decimal.NewFromFloat(in.Position.Longitude)
	}

	// The fence of THIS stop's warehouse, not the order's single destination.
	within, distance, radius, known := s.stopGeofence(ctx, stop, in.Position)
	if known {
		fields["within_geofence"] = within
		if !within && s.geofencingEnforced(ctx, order) {
			return nil, fmt.Errorf("%w: Driver terdeteksi belum berada di %s — %.0f m dari gudang, di luar radius %d m",
				ErrValidation, stopLabel(stop), distance, radius)
		}
	}
	if err := s.stops.UpdateFields(ctx, stop.ID, fields); err != nil {
		return nil, err
	}
	return s.reloadStop(ctx, order.ID, stop.ID)
}

// StartStop records that work began at one point — muat or bongkar.
func (s *ShipmentService) StartStop(ctx context.Context, actor Actor, shipmentID uuid.UUID, in StopVisitInput) (*models.OrderStop, error) {
	stop, order, err := s.stopForDriver(ctx, actor, shipmentID, in.StopID)
	if err != nil {
		return nil, err
	}
	if stop.ArrivedAt == nil {
		return nil, fmt.Errorf("%w: laporkan tiba di %s dulu sebelum memulai", ErrValidation, stopLabel(stop))
	}
	if stop.StartedAt != nil {
		return stop, nil
	}
	// Unloading hands the goods to somebody, and that somebody confirms the
	// driver's code first — at THIS point, with the PIC named on it. Without
	// the per-stop check a driver could unload at Priok on the strength of
	// the code Semarang's receiver confirmed. Loading has no handover: the
	// goods are being collected, not signed over. The testing bypass and
	// admins step past it, as they do at the shipment's own start.
	if stop.Kind == models.StopUnload && !actor.StatusBypass && actor.Role == models.RoleDriver {
		if err := s.assertStopHandover(ctx, shipmentID, order.ID, stop); err != nil {
			return nil, err
		}
	}
	// Same rule as the shipment's own start: being at the gate is what
	// permits the work, and an enforced order says so with a position.
	if s.geofencingEnforced(ctx, order) {
		within, distance, radius, known := s.stopGeofence(ctx, stop, in.Position)
		if in.Position == nil {
			return nil, fmt.Errorf("%w: Driver terdeteksi belum berada di %s — aktifkan GPS lalu coba lagi", ErrValidation, stopLabel(stop))
		}
		if known && !within {
			return nil, fmt.Errorf("%w: Driver terdeteksi belum berada di %s — %.0f m dari gudang, di luar radius %d m",
				ErrValidation, stopLabel(stop), distance, radius)
		}
	}
	if err := s.stops.UpdateFields(ctx, stop.ID, map[string]interface{}{"started_at": time.Now()}); err != nil {
		return nil, err
	}

	// Work has begun at this point, so the shipment is made to say so. On an
	// interleaved journey that means walking it to `unloading` while a
	// loading point is still outstanding, which is the honest reading: the
	// truck IS unloading. Without it the shipment would sit on the loading
	// side for the whole trip, the unloading POD would be refused for
	// disagreeing with it, and the steps that end the journey would never
	// become reachable.
	if current, err := s.shipments.FindByID(ctx, shipmentID); err == nil {
		// As the driver: they are the one who has begun work here.
		if _, err := s.walkShipmentTo(ctx, actor, current, order,
			shipmentStatusForStop(stop), machineRole(actor, order), in.Position); err != nil {
			// The stop's own record is what this visit is judged by, so a
			// status that will not move does not stop the driver working.
			slog.WarnContext(ctx, "shipment status not walked with the stops",
				"shipmentId", shipmentID, "stopId", stop.ID, "error", err)
		}
	}
	return s.reloadStop(ctx, order.ID, stop.ID)
}

// assertStopHandover checks the right handover for this unloading point.
//
// The FIRST unloading point of a journey is verified through the shipment's
// own OTP — that is where the delivery's stage-level handover was confirmed,
// and on a two-ended journey it is the only one. Later points each have their
// own, because the goods change hands again to somebody else.
//
// Without the first-point fallback the driver app's mirror of the shipment's
// start onto stop 1 is refused on every multi-stop order, and that stop is
// left with no start time even though the driver plainly started there.
func (s *ShipmentService) assertStopHandover(ctx context.Context, shipmentID, orderID uuid.UUID, stop *models.OrderStop) error {
	if err := s.assertHandoverVerified(ctx, shipmentID, &stop.ID); err == nil {
		return nil
	}
	if s.isFirstUnloadStop(ctx, orderID, stop) {
		return s.assertHandoverVerified(ctx, shipmentID, nil)
	}
	return s.assertHandoverVerified(ctx, shipmentID, &stop.ID)
}

// isFirstUnloadStop says whether this is the journey's earliest unloading
// point in visit order.
func (s *ShipmentService) isFirstUnloadStop(ctx context.Context, orderID uuid.UUID, stop *models.OrderStop) bool {
	stops, err := s.stops.ListByOrder(ctx, orderID)
	if err != nil {
		return false
	}
	for i := range stops {
		if stops[i].Kind != models.StopUnload {
			continue
		}
		return stops[i].ID == stop.ID
	}
	return false
}

// CheckStopCargo records sesuai / tidak sesuai at one point.
func (s *ShipmentService) CheckStopCargo(ctx context.Context, actor Actor, shipmentID uuid.UUID, in StopVisitInput) (*models.OrderStop, error) {
	stop, order, err := s.stopForDriver(ctx, actor, shipmentID, in.StopID)
	if err != nil {
		return nil, err
	}
	if in.Matches == nil {
		return nil, fmt.Errorf("%w: jawab sesuai atau tidak sesuai", ErrValidation)
	}
	if stop.StartedAt == nil {
		return nil, fmt.Errorf("%w: mulai proses di %s dulu", ErrValidation, stopLabel(stop))
	}
	if err := s.stops.UpdateFields(ctx, stop.ID, map[string]interface{}{
		"cargo_matches":    *in.Matches,
		"cargo_note":       in.Note,
		"cargo_checked_at": time.Now(),
	}); err != nil {
		return nil, err
	}
	return s.reloadStop(ctx, order.ID, stop.ID)
}

// stopForDriver resolves a stop and checks who is asking.
//
// The same rule the rest of the driver flow uses: the assigned driver acts on
// their own shipment, and staff or the testing bypass may act for them.
func (s *ShipmentService) stopForDriver(ctx context.Context, actor Actor, shipmentID, stopID uuid.UUID) (*models.OrderStop, *models.Order, error) {
	if s.stops == nil {
		return nil, nil, fmt.Errorf("%w: this journey has no stops recorded", ErrValidation)
	}
	shipment, err := s.shipments.FindByID(ctx, shipmentID)
	if err != nil {
		return nil, nil, err
	}
	order, err := s.orders.FindByIDForService(ctx, shipment.OrderID)
	if err != nil {
		return nil, nil, err
	}
	isDriver := shipment.DriverUserID != nil && *shipment.DriverUserID == actor.UserID
	if !isDriver && !actor.StatusBypass && actor.Role != models.RoleAdmin {
		return nil, nil, fmt.Errorf("%w: only the assigned driver may report from a stop", ErrForbidden)
	}
	stops, err := s.stops.ListByOrder(ctx, order.ID)
	if err != nil {
		return nil, nil, err
	}
	for i := range stops {
		if stops[i].ID == stopID {
			return &stops[i], order, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: that stop does not belong to this shipment", ErrValidation)
}

// assertStopIsNext keeps the journey in order.
//
// Arriving at the third point while the second is unvisited is either a
// mistake or a skipped delivery, and both are worth refusing: the sequence is
// what the planner agreed with the customer.
func (s *ShipmentService) assertStopIsNext(ctx context.Context, orderID uuid.UUID, stop *models.OrderStop) error {
	stops, err := s.stops.ListByOrder(ctx, orderID)
	if err != nil {
		return nil
	}
	for i := range stops {
		if stops[i].ID == stop.ID {
			break
		}
		if stops[i].FinishedAt == nil && stops[i].ArrivedAt == nil {
			return fmt.Errorf("%w: kunjungi %s dulu — urutan perjalanan sudah disepakati",
				ErrValidation, stopLabel(&stops[i]))
		}
	}
	return nil
}

// stopGeofence measures the driver against this stop's own warehouse.
func (s *ShipmentService) stopGeofence(ctx context.Context, stop *models.OrderStop, pos *Position) (within bool, distance float64, radius int, known bool) {
	if pos == nil || s.masterdata == nil {
		return false, 0, 0, false
	}
	w, err := s.masterdata.GetWarehouse(ctx, stop.WarehouseID)
	if err != nil {
		return false, 0, 0, false
	}
	radius = int(w.GetGeofenceRadiusMeters())
	if radius <= 0 {
		radius = 1000
	}
	distance = haversineMeters(pos.Latitude, pos.Longitude, w.GetLatitude(), w.GetLongitude())
	return distance <= float64(radius), distance, radius, true
}

func (s *ShipmentService) reloadStop(ctx context.Context, orderID, stopID uuid.UUID) (*models.OrderStop, error) {
	stops, err := s.stops.ListByOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	for i := range stops {
		if stops[i].ID == stopID {
			return &stops[i], nil
		}
	}
	return nil, nil
}

// stopLabel names a stop in the driver's words.
func stopLabel(stop *models.OrderStop) string {
	if stop.Kind == models.StopLoad {
		return "titik muat"
	}
	return "titik bongkar"
}

// The shipment's status follows the stops, on a journey that has them.
//
// A two-ended trip's statuses and its two visits are the same events, so the
// driver app drives the statuses directly. A journey through several points
// cannot work that way: with the visit order Muat 1 → Bongkar 1 → Muat 2 →
// Bongkar 2 the truck is unloading at Semarang while a loading point is still
// outstanding, and no single status describes that. The stop rows do.
//
// So the stop events lead and the status is walked along behind them, far
// enough that what the shipment says is true of the point being worked and
// that the steps which end a stage — loaded, unloaded, finished — remain
// reachable from wherever the journey has got to.

// shipmentStatusForStop is where the shipment should stand while this visit
// is being worked.
func shipmentStatusForStop(stop *models.OrderStop) string {
	if stop.Kind == models.StopLoad {
		return models.ShipmentLoading
	}
	return models.ShipmentUnloading
}

// walkShipmentTo advances a shipment step by step until it reaches `to`,
// taking each transition as the system.
//
// Step by step because the table allows one hop at a time: from toUnloading
// the shipment cannot jump to unloading, it must pass through atUnloading.
// Advancing one step per event instead would leave the journey stranded —
// nothing else was going to take those steps, because the driver app stops
// firing shipment statuses once a journey is stop-driven.
//
// A shipment already at or past `to` is left alone: the status only ever
// moves forward, and a later visit must not drag it back.
// The role matters: the steps through the middle of the journey — loaded →
// toUnloading → atUnloading → unloading — belong to the driver, and the
// system may not take them. That is correct, and it is why the walk is given
// the role of whoever caused it: a driver starting work at a stop moves the
// shipment as the driver, while the approval that ends a stage moves it as
// the system, which is what those last two steps allow.
func (s *ShipmentService) walkShipmentTo(ctx context.Context, actor Actor, shipment *models.Shipment, order *models.Order, to, role string, pos *Position) (*models.Shipment, error) {
	if shipment.StatusCode == to || models.ShipmentStatusAtOrPast(shipment.StatusCode, to) {
		return shipment, nil
	}
	// Bounded: the table is a chain, so the walk cannot be longer than it.
	for i := 0; i < 12 && shipment.StatusCode != to; i++ {
		next := models.NextShipmentStatusTowards(shipment.StatusCode, to)
		if next == "" {
			return shipment, nil // no route from here; leave it where it is
		}
		// The position travels with the walk: a driver-role step runs the
		// geofence check, and a walk with no position is refused for having
		// no GPS — at a gate the driver is standing at.
		moved, err := s.advance(ctx, actor, shipment, order,
			AdvanceInput{ShipmentID: shipment.ID, To: next, Position: pos}, role)
		if err != nil {
			// Returned, not swallowed: the caller decides. A stop event
			// treats the status as a description and carries on; approving
			// the last POD of a stage is what ends the journey, and a
			// failure there must be heard.
			return shipment, err
		}
		shipment = moved
	}
	return shipment, nil
}
