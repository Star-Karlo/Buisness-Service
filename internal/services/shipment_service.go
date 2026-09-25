package services

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
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
	// handovers answers whether the unloading OTP was confirmed, which gates
	// the driver's "start unloading". Nil skips the gate (tests).
	handovers *repository.HandoverRepository
	// pods fills Shipment.Pods on read, so the driver app sees the review
	// state with the shipment. Nil leaves it empty (tests).
	pods *repository.PodRepository
}

// WithDriverFlow wires the handover and POD registers in: the unloading
// gate, and the read-time decoration of the shipment.
func (s *ShipmentService) WithDriverFlow(h *repository.HandoverRepository, p *repository.PodRepository) *ShipmentService {
	s.handovers, s.pods = h, p
	return s
}

// Decorate fills the read-only driver-flow fields on a shipment (the
// latest POD per stage, whether the OTP was confirmed).
func (s *ShipmentService) Decorate(ctx context.Context, shipment *models.Shipment) *models.Shipment {
	return s.decorate(ctx, shipment)
}

// decorate fills the read-only driver-flow fields on a shipment.
func (s *ShipmentService) decorate(ctx context.Context, shipment *models.Shipment) *models.Shipment {
	if shipment == nil {
		return nil
	}
	if s.pods != nil {
		if latest, err := s.pods.Latest(ctx, shipment.ID); err == nil {
			shipment.Pods = latest
		}
		if all, err := s.pods.ListByShipment(ctx, shipment.ID); err == nil {
			shipment.PodHistory = all
		}
	}
	s.fillTripSites(ctx, shipment)
	if s.handovers != nil {
		if ok, at, err := s.handovers.IsVerified(ctx, shipment.ID, "unloading"); err == nil {
			shipment.HandoverVerified, shipment.HandoverVerifiedAt = ok, at
		}
	}
	return shipment
}

// fillTripSites attaches the two warehouses of the trip.
//
// So the driver's app can read an address, a pin and a fence without holding
// warehouse.read, which would let a phone enumerate every site the company
// has. The service fetches them under its own credentials, for this shipment
// only.
func (s *ShipmentService) fillTripSites(ctx context.Context, shipment *models.Shipment) {
	if s.masterdata == nil {
		return
	}
	order, err := s.orders.FindByIDForService(ctx, shipment.OrderID)
	if err != nil {
		return
	}
	site := func(id *string) *models.TripSite {
		if id == nil || *id == "" {
			return nil
		}
		w, err := s.masterdata.GetWarehouse(ctx, *id)
		if err != nil {
			return nil
		}
		return &models.TripSite{
			ID: w.GetId(), Name: w.GetName(), Address: w.GetAddress(), City: w.GetCityId(),
			Latitude: w.GetLatitude(), Longitude: w.GetLongitude(),
			GeofenceRadiusMeters: int(w.GetGeofenceRadiusMeters()),
			PICName:              w.GetPicName(), PICPhone: w.GetPicPhone(),
		}
	}
	shipment.OriginWarehouse = site(order.OriginWarehouseID)
	shipment.DestinationWarehouse = site(order.DestinationWarehouseID)
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

	// Starting to unload needs the PIC's code confirmed first (the driver
	// flow's OTP page). The testing bypass and admins step past it.
	if in.To == models.ShipmentUnloading && !actor.StatusBypass && actor.Role == models.RoleDriver {
		if err := s.assertHandoverVerified(ctx, shipment.ID); err != nil {
			return nil, err
		}
	}

	return s.advance(ctx, actor, shipment, order, in, machineRole(actor, order))
}

// AdvanceBySystem takes a step the service itself decided on — the move an
// approved POD triggers — on behalf of the reviewer who caused it. The role
// check is RoleSystem's; the reviewer's own entitlement was checked by the
// caller.
func (s *ShipmentService) AdvanceBySystem(ctx context.Context, actor Actor, shipment *models.Shipment, order *models.Order, to string) (*models.Shipment, error) {
	return s.advance(ctx, actor, shipment, order, AdvanceInput{ShipmentID: shipment.ID, To: to}, models.RoleSystem)
}

func (s *ShipmentService) assertHandoverVerified(ctx context.Context, shipmentID uuid.UUID) error {
	if s.handovers == nil {
		return nil
	}
	ok, _, err := s.handovers.IsVerified(ctx, shipmentID, "unloading")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: the receiving PIC's code has not been confirmed yet", ErrValidation)
	}
	return nil
}

func (s *ShipmentService) advance(ctx context.Context, actor Actor, shipment *models.Shipment, order *models.Order, in AdvanceInput, role string) (*models.Shipment, error) {
	if err := models.CanTransitionShipment(shipment.StatusCode, in.To, role); err != nil {
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
	// The testing bypass steps past the driver-only rule, never past tenancy.
	if actor.StatusBypass {
		if !order.InvolvesCompany(actor.CompanyID) {
			return fmt.Errorf("%w: your company is not a party to this order", ErrForbidden)
		}
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

	// The driver flow's arrival rule is "within 1 km of the warehouse"
	// unless the warehouse sets its own radius.
	radius := int(warehouse.GetGeofenceRadiusMeters())
	if radius <= 0 {
		radius = 1000
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

	if s.geofencingEnforced(ctx, order) {
		return fmt.Errorf(
			"%w: you are %.0f m from the warehouse, outside the %d m geofence",
			ErrValidation, distance, radius,
		)
	}
	return nil
}

// geofencingEnforced answers whether THIS order refuses an arrival reported
// outside the fence.
//
// The order's own answer wins, and the company setting is the default it falls
// back to. That ordering is the point of the per-order switch: a planner
// handling one awkward delivery must be able to let it through without
// unfencing every other truck on the road, and a company that wants the rule
// everywhere still gets it on every order nobody has touched.
//
// A master-data or auth outage answers "not enforced": a driver at a gate must
// not be stranded because a setting could not be read.
func (s *ShipmentService) geofencingEnforced(ctx context.Context, order *models.Order) bool {
	if order.GeofencingEnabled != nil {
		return *order.GeofencingEnabled
	}
	settings, err := s.auth.CompanySettings(ctx, order.ShipperCompanyID.String())
	if err != nil {
		return false
	}
	return settings.GetFinishWithGeofencing()
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
		target = models.OrderCompleted
	default:
		return nil
	}

	if order.StatusCode == target {
		return nil
	}

	// The order machine drives these moves with no role attached, because it is
	// the shipment that caused them rather than a person.
	if err := models.CanTransitionOrder(order.StatusCode, target, machineRole(actor, order)); err != nil {
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
	shipment, err := s.shipments.FindByOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	return s.decorate(ctx, shipment), nil
}

// Accept records the driver's acceptance of the job — the swipe in the
// driver app — and where they were when they took it. The shipment stays
// "assigned"; the app moves it to toLoading once the truck is more than a
// kilometre from this point.
func (s *ShipmentService) Accept(ctx context.Context, actor Actor, shipmentID uuid.UUID, pos *Position) (*models.Shipment, error) {
	shipment, err := s.shipments.FindByID(ctx, shipmentID)
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
	if shipment.StatusCode != models.ShipmentAssigned {
		return nil, fmt.Errorf("%w: the shipment is already under way", ErrTransition)
	}
	if shipment.AcceptedAt != nil {
		return shipment, nil
	}
	now := time.Now()
	fields := map[string]interface{}{"accepted_at": now}
	if pos != nil {
		fields["accepted_latitude"] = pos.Latitude
		fields["accepted_longitude"] = pos.Longitude
	}
	if err := s.shipments.UpdateFields(ctx, shipmentID, fields); err != nil {
		return nil, err
	}
	shipment.AcceptedAt = &now
	if pos != nil {
		shipment.AcceptedLatitude, shipment.AcceptedLongitude = &pos.Latitude, &pos.Longitude
	}
	return shipment, nil
}

// CargoCheckInput is the "sesuai / tidak sesuai" answer for one stage.
type CargoCheckInput struct {
	Stage   string
	Matches bool
	Note    string
	// Via names the surface the answer came from: "app", "console", "field".
	Via string
	// By is who answered; nil for the PIC on the field page (no account).
	By *uuid.UUID
}

// RecordCargoCheck stores the cargo check for a stage. At loading it is the
// driver's own answer, taken while loading; at unloading it is the receiving
// PIC's, taken while unloading. Either may be re-answered until the stage's
// POD is approved.
func (s *ShipmentService) RecordCargoCheck(ctx context.Context, shipment *models.Shipment, in CargoCheckInput) error {
	var prefix, wantStatus string
	switch in.Stage {
	case "loading":
		prefix, wantStatus = "loading_cargo", models.ShipmentLoading
	case "unloading":
		prefix, wantStatus = "unloading_cargo", models.ShipmentUnloading
	default:
		return fmt.Errorf("%w: stage must be loading or unloading", ErrValidation)
	}
	if shipment.StatusCode != wantStatus {
		return fmt.Errorf("%w: the cargo check for %s is taken while the shipment is %s, not %s", ErrTransition, in.Stage, wantStatus, shipment.StatusCode)
	}
	fields := map[string]interface{}{
		prefix + "_matches":    in.Matches,
		prefix + "_note":       strings.TrimSpace(in.Note),
		prefix + "_checked_at": time.Now(),
		prefix + "_checked_by": in.By,
	}
	if in.Stage == "unloading" {
		fields["unloading_cargo_checked_via"] = in.Via
	}
	return s.shipments.UpdateFields(ctx, shipment.ID, fields)
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
	var firstArrival notificationv1.EventType
	switch {
	case order.OriginWarehouseID != nil && *order.OriginWarehouseID == warehouseID:
		fields["loading_within_geofence"] = entering
		if entering && shipment.ArrivedLoadingAt == nil {
			fields["arrived_loading_at"] = at
			firstArrival = notificationv1.EventType_EVENT_TYPE_SHIPMENT_ARRIVED_LOADING
		}
	case order.DestinationWarehouseID != nil && *order.DestinationWarehouseID == warehouseID:
		fields["unloading_within_geofence"] = entering
		if entering && shipment.ArrivedUnloadingAt == nil {
			fields["arrived_unloading_at"] = at
			firstArrival = notificationv1.EventType_EVENT_TYPE_SHIPMENT_ARRIVED_UNLOADING
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

	// The first time the truck reaches a warehouse, the people waiting for it
	// hear so — the same message the driver's own "arrived" tap would send,
	// only earlier and without depending on the tap.
	if firstArrival != notificationv1.EventType_EVENT_TYPE_UNSPECIFIED {
		s.notifier.Notify(ctx, clients.Event{
			Type:           firstArrival,
			Subject:        clients.Subject{ID: order.ID.String(), Type: "order"},
			Audience:       clients.ToCompanyRoles(order.ShipperCompanyID.String(), models.RoleShipper, models.RoleWarehousePic),
			IdempotencyKey: fmt.Sprintf("shipment:%s:geofence:%s", order.ID, warehouseID),
			Params: map[string]interface{}{
				"orderNumber": order.OrderNumber,
				"status":      models.StatusLabel(models.DomainShipment, shipment.StatusCode),
			},
		})
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
