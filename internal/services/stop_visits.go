package services

import (
	"context"
	"fmt"
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
