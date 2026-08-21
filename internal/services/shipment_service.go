package services

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/karlo/business-service/internal/clients"
	"github.com/karlo/business-service/internal/models"
	notificationv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/notification/v1"
	"github.com/karlo/business-service/internal/repository"
)

// ShipmentService drives the physical execution of an order.
type ShipmentService struct {
	shipments  *repository.ShipmentRepository
	orders     *repository.OrderRepository
	auth       *clients.Auth
	masterdata *clients.MasterData
	notifier   clients.Notifier
}

func NewShipmentService(
	shipments *repository.ShipmentRepository,
	orders *repository.OrderRepository,
	auth *clients.Auth,
	masterdata *clients.MasterData,
	notifier clients.Notifier,
) *ShipmentService {
	return &ShipmentService{
		shipments:  shipments,
		orders:     orders,
		auth:       auth,
		masterdata: masterdata,
		notifier:   notifier,
	}
}

// Position is a reported location, supplied by the driver app on the steps that
// record one.
type Position struct {
	Latitude  float64
	Longitude float64
}

// AdvanceInput is one step of the shipment lifecycle.
type AdvanceInput struct {
	ShipmentID uuid.UUID
	To         string
	Position   *Position
	Note       string
}

// timestampColumn maps each shipment state to the column that records when it
// was reached. Holding this as data rather than a switch is what lets one
// Advance method serve the whole lifecycle, where the monolith had a dozen
// near-identical handlers that had drifted apart.
var timestampColumn = map[string]string{
	models.ShipmentToLoading:   "started_to_loading_at",
	models.ShipmentAtLoading:   "arrived_loading_at",
	models.ShipmentLoading:     "loading_started_at",
	models.ShipmentLoaded:      "loading_finished_at",
	models.ShipmentToUnloading: "started_to_unloading_at",
	models.ShipmentAtUnloading: "arrived_unloading_at",
	models.ShipmentUnloading:   "unloading_started_at",
	models.ShipmentUnloaded:    "unloading_finished_at",
	models.ShipmentFinished:    "finished_at",
}

// Advance moves a shipment one step along its lifecycle.
func (s *ShipmentService) Advance(ctx context.Context, actor Actor, in AdvanceInput) (*models.Shipment, error) {
	shipment, err := s.shipments.FindByID(ctx, in.ShipmentID)
	if err != nil {
		return nil, err
	}

	order, err := s.orders.FindByID(ctx, actor.CompanyID, shipment.OrderID)
	if err != nil {
		return nil, fmt.Errorf("%w: this shipment does not belong to your company", ErrForbidden)
	}

	if err := s.assertActorMayAdvance(actor, shipment, order); err != nil {
		return nil, err
	}

	if err := models.CanTransitionShipment(shipment.StatusCode, in.To, actor.Role); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransition, err)
	}

	fields := map[string]interface{}{}
	if col, ok := timestampColumn[in.To]; ok {
		fields[col] = time.Now()
	}

	// Arrival steps record where the driver actually was, and whether that was
	// inside the warehouse's geofence.
	if err := s.recordArrival(ctx, in, order, fields); err != nil {
		return nil, err
	}

	if err := s.shipments.ApplyStatus(ctx, in.ShipmentID, shipment.StatusCode, in.To, fields); err != nil {
		return nil, err
	}

	if err := s.syncOrderStatus(ctx, actor, order, in.To); err != nil {
		return nil, err
	}

	s.notifyAdvance(ctx, actor, order, in.To)

	shipment.StatusCode = in.To
	shipment.Status = models.StatusLabel(models.DomainShipment, in.To)
	shipment.StatusAlias = models.StatusAlias(models.DomainShipment, in.To)
	return shipment, nil
}

// assertActorMayAdvance confirms the caller is entitled to move THIS shipment,
// which the state machine cannot know: it checks roles, not identities.
func (s *ShipmentService) assertActorMayAdvance(actor Actor, shipment *models.Shipment, order *models.Order) error {
	if actor.Role == models.RoleAdmin || actor.Role == models.RoleSuperadmin {
		return nil
	}

	// A driver may only move their own shipment. Without this, any driver in
	// the company could report progress on anyone's job.
	if actor.Role == models.RoleDriver {
		if shipment.DriverUserID == nil || *shipment.DriverUserID != actor.UserID {
			return fmt.Errorf("%w: this shipment is assigned to another driver", ErrForbidden)
		}
		return nil
	}

	if !order.InvolvesCompany(actor.CompanyID) {
		return fmt.Errorf("%w: your company is not a party to this order", ErrForbidden)
	}
	return nil
}

// recordArrival stores the reported position and evaluates the geofence.
//
// The result is recorded either way. Enforcement is a separate decision, taken
// below, so that turning the company setting on later does not invalidate
// history recorded while it was off.
func (s *ShipmentService) recordArrival(ctx context.Context, in AdvanceInput, order *models.Order, fields map[string]interface{}) error {
	var (
		warehouseID string
		latCol      string
		lngCol      string
		fenceCol    string
	)

	switch in.To {
	case models.ShipmentAtLoading:
		latCol, lngCol, fenceCol = "loading_latitude", "loading_longitude", "loading_within_geofence"
		if order.OriginWarehouseID != nil {
			warehouseID = *order.OriginWarehouseID
		}
	case models.ShipmentAtUnloading:
		latCol, lngCol, fenceCol = "unloading_latitude", "unloading_longitude", "unloading_within_geofence"
		if order.DestinationWarehouseID != nil {
			warehouseID = *order.DestinationWarehouseID
		}
	default:
		return nil
	}

	if in.Position == nil {
		return fmt.Errorf("%w: a position is required when reporting arrival", ErrValidation)
	}

	fields[latCol] = in.Position.Latitude
	fields[lngCol] = in.Position.Longitude

	if warehouseID == "" {
		return nil
	}

	warehouse, err := s.masterdata.GetWarehouse(ctx, warehouseID)
	if err != nil {
		// A master data outage must not strand a driver at a gate. The arrival
		// is recorded with the geofence result left unknown.
		fields[fenceCol] = nil
		return nil
	}

	radius := int(warehouse.GetGeofenceRadiusMeters())
	if radius <= 0 {
		radius = 200
	}

	distance := haversineMeters(
		in.Position.Latitude, in.Position.Longitude,
		warehouse.GetLatitude(), warehouse.GetLongitude(),
	)
	within := distance <= float64(radius)
	fields[fenceCol] = within

	if within {
		return nil
	}

	// Enforce only when the shipper's company asked for it.
	settings, err := s.auth.CompanySettings(ctx, order.ShipperCompanyID.String())
	if err != nil {
		return nil
	}
	if settings.GetFinishWithGeofencing() {
		return fmt.Errorf(
			"%w: you are %.0f m from the warehouse, outside the %d m geofence",
			ErrValidation, distance, radius,
		)
	}
	return nil
}

// syncOrderStatus keeps the order in step with its shipment.
//
// The order and the shipment are separate state machines that must not
// disagree: an order still reading "assigned" while its shipment is halfway to
// the unloading point is exactly the inconsistency the legacy system produced.
func (s *ShipmentService) syncOrderStatus(ctx context.Context, actor Actor, order *models.Order, shipmentStatus string) error {
	var target string
	switch shipmentStatus {
	case models.ShipmentToLoading:
		target = models.OrderInTransit
	case models.ShipmentFinished:
		target = models.OrderDelivered
	default:
		return nil
	}

	if order.StatusCode == target {
		return nil
	}

	// The order machine drives these moves with no role attached, because it is
	// the shipment that caused them rather than a person.
	if err := models.CanTransitionOrder(order.StatusCode, target, actor.Role); err != nil {
		// The shipment advanced but the order cannot follow. Report it rather
		// than leaving the two silently inconsistent.
		return fmt.Errorf("%w: shipment moved to %s but the order cannot move to %s: %v",
			ErrTransition, shipmentStatus, target, err)
	}

	return s.orders.ApplyStatus(ctx, repository.StatusChange{
		OrderID: order.ID,
		From:    order.StatusCode,
		To:      target,
		Actor:   &actor.UserID,
		Source:  models.SourceSystem,
	})
}

func (s *ShipmentService) notifyAdvance(ctx context.Context, actor Actor, order *models.Order, to string) {
	var eventType notificationv1.EventType
	switch to {
	case models.ShipmentToLoading:
		eventType = notificationv1.EventType_EVENT_TYPE_SHIPMENT_STARTED
	case models.ShipmentAtLoading:
		eventType = notificationv1.EventType_EVENT_TYPE_SHIPMENT_ARRIVED_LOADING
	case models.ShipmentLoaded:
		eventType = notificationv1.EventType_EVENT_TYPE_SHIPMENT_LOADED
	case models.ShipmentAtUnloading:
		eventType = notificationv1.EventType_EVENT_TYPE_SHIPMENT_ARRIVED_UNLOADING
	case models.ShipmentFinished:
		eventType = notificationv1.EventType_EVENT_TYPE_SHIPMENT_FINISHED
	default:
		return
	}

	s.notifier.Notify(ctx, clients.Event{
		Type:           eventType,
		Subject:        clients.Subject{ID: order.ID.String(), Type: "order"},
		Audience:       clients.ToCompanyRoles(order.ShipperCompanyID.String(), models.RoleShipper, models.RoleWarehousePic),
		ActorID:        actor.UserID.String(),
		IdempotencyKey: fmt.Sprintf("shipment:%s:%s", order.ID, to),
		Params: map[string]interface{}{
			"orderNumber": order.OrderNumber,
			"status":      models.StatusLabel(models.DomainShipment, to),
		},
	})
}

// GetByOrder returns the shipment for an order.
func (s *ShipmentService) GetByOrder(ctx context.Context, actor Actor, orderID uuid.UUID) (*models.Shipment, error) {
	if _, err := s.orders.FindByID(ctx, actor.CompanyID, orderID); err != nil {
		return nil, err
	}
	return s.shipments.FindByOrder(ctx, orderID)
}

// ActiveForDriver answers the telemetry service's correlation question.
func (s *ShipmentService) ActiveForDriver(ctx context.Context, driverID uuid.UUID) (*models.Shipment, error) {
	return s.shipments.FindActiveByDriver(ctx, driverID)
}

// AttachDocument records a POD or checklist upload.
func (s *ShipmentService) AttachDocument(ctx context.Context, actor Actor, shipmentID uuid.UUID, doc models.ShipmentDocument) error {
	shipment, err := s.shipments.FindByID(ctx, shipmentID)
	if err != nil {
		return err
	}
	order, err := s.orders.FindByID(ctx, actor.CompanyID, shipment.OrderID)
	if err != nil {
		return fmt.Errorf("%w: this shipment does not belong to your company", ErrForbidden)
	}
	if err := s.assertActorMayAdvance(actor, shipment, order); err != nil {
		return err
	}

	doc.ShipmentID = shipmentID
	doc.UploadedByUserID = &actor.UserID
	return s.shipments.AddDocument(ctx, &doc)
}

// Documents lists a shipment's attachments.
func (s *ShipmentService) Documents(ctx context.Context, actor Actor, shipmentID uuid.UUID) ([]models.ShipmentDocument, error) {
	shipment, err := s.shipments.FindByID(ctx, shipmentID)
	if err != nil {
		return nil, err
	}
	if _, err := s.orders.FindByID(ctx, actor.CompanyID, shipment.OrderID); err != nil {
		return nil, fmt.Errorf("%w: this shipment does not belong to your company", ErrForbidden)
	}
	return s.shipments.Documents(ctx, shipmentID)
}

// ReportGeofenceEvent is the inbound edge from the telemetry service.
//
// A geofence crossing is evidence, not a command: it records that the truck
// arrived, but it does not advance the shipment on the driver's behalf. Drivers
// confirm arrival themselves, and conflating the two would let a GPS glitch
// move a shipment forward.
func (s *ShipmentService) ReportGeofenceEvent(ctx context.Context, shipmentID uuid.UUID, warehouseID string, entering bool, at time.Time) (string, error) {
	shipment, err := s.shipments.FindByID(ctx, shipmentID)
	if err != nil {
		return "", err
	}

	order, err := s.orders.FindByIDForService(ctx, shipment.OrderID)
	if err != nil {
		return "", err
	}

	fields := map[string]interface{}{}
	switch {
	case entering && order.OriginWarehouseID != nil && *order.OriginWarehouseID == warehouseID:
		fields["loading_within_geofence"] = true
		if shipment.ArrivedLoadingAt == nil {
			fields["arrived_loading_at"] = at
		}
	case entering && order.DestinationWarehouseID != nil && *order.DestinationWarehouseID == warehouseID:
		fields["unloading_within_geofence"] = true
		if shipment.ArrivedUnloadingAt == nil {
			fields["arrived_unloading_at"] = at
		}
	default:
		// A crossing at some other warehouse is not relevant to this shipment.
		return shipment.StatusCode, nil
	}

	// Status is unchanged: from and to are the same, so this records the
	// evidence without moving the machine.
	if err := s.shipments.ApplyStatus(ctx, shipmentID, shipment.StatusCode, shipment.StatusCode, fields); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			// Something else advanced the shipment first; that is not an error
			// for a passive observation.
			return shipment.StatusCode, nil
		}
		return "", err
	}

	return shipment.StatusCode, nil
}

// earthRadiusMeters is the mean radius used for distance calculations.
const earthRadiusMeters = 6371000.0

// haversineMeters returns the great-circle distance between two positions.
//
// Accurate to a few metres at the scale of a warehouse geofence, which is all
// this needs to decide whether a driver is at the gate.
func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	rad := func(deg float64) float64 { return deg * math.Pi / 180 }

	dLat := rad(lat2 - lat1)
	dLon := rad(lon2 - lon1)

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(rad(lat1))*math.Cos(rad(lat2))*
			math.Sin(dLon/2)*math.Sin(dLon/2)

	return earthRadiusMeters * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

// GetByOrderShipment resolves a shipment by its own id, applying the same
// tenant check as the advance path: the caller's company must be a party to the
// order behind it.
func (s *ShipmentService) GetByOrderShipment(ctx context.Context, actor Actor, shipmentID uuid.UUID) (*models.Shipment, error) {
	shipment, err := s.shipments.FindByID(ctx, shipmentID)
	if err != nil {
		return nil, err
	}
	if _, err := s.orders.FindByID(ctx, actor.CompanyID, shipment.OrderID); err != nil {
		return nil, fmt.Errorf("%w: this shipment does not belong to your company", ErrForbidden)
	}
	return shipment, nil
}
