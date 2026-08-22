// Package services holds the business rules for orders, shipments, agreements
// and invoices.
package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/karlo/business-service/internal/clients"
	"github.com/karlo/business-service/internal/models"
	notificationv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/notification/v1"
	"github.com/karlo/business-service/internal/platform/query"
	"github.com/karlo/business-service/internal/repository"
	"github.com/shopspring/decimal"
)

var (
	// ErrValidation is a rejected input.
	ErrValidation = errors.New("validation failed")
	// ErrForbidden is a caller acting outside their authority.
	ErrForbidden = errors.New("forbidden")
	// ErrTransition is a refused state change.
	ErrTransition = errors.New("invalid state change")
)

// Actor is the caller performing an operation, resolved from their token.
type Actor struct {
	UserID    uuid.UUID
	CompanyID uuid.UUID
	Role      string
}

// OrderService owns the order lifecycle.
type OrderService struct {
	orders     *repository.OrderRepository
	shipments  *repository.ShipmentRepository
	agreements *repository.AgreementRepository
	auth       *clients.Auth
	masterdata *clients.MasterData
	notifier   clients.Notifier
}

func NewOrderService(
	orders *repository.OrderRepository,
	shipments *repository.ShipmentRepository,
	agreements *repository.AgreementRepository,
	auth *clients.Auth,
	masterdata *clients.MasterData,
	notifier clients.Notifier,
) *OrderService {
	return &OrderService{
		orders:     orders,
		shipments:  shipments,
		agreements: agreements,
		auth:       auth,
		masterdata: masterdata,
		notifier:   notifier,
	}
}

// CreateOrderInput describes a new order.
type CreateOrderInput struct {
	AgreementID            *uuid.UUID
	TransporterCompanyID   *uuid.UUID
	OrderKind              string
	OriginWarehouseID      string
	DestinationWarehouseID string
	CargoTypeID            string
	ItemTypeID             string
	Quantity               *decimal.Decimal
	WeightKg               *decimal.Decimal
	VolumeM3               *decimal.Decimal
	PickupAt               *time.Time
	DeliveryAt             *time.Time
	CustomerID             string
	ReferenceNumber        string
	Detail                 map[string]interface{}
	// SubmitImmediately skips the draft state for clients that have no draft UI.
	SubmitImmediately bool
}

// Create places a new order.
func (s *OrderService) Create(ctx context.Context, actor Actor, in CreateOrderInput) (*models.Order, error) {
	if err := s.validateCreate(ctx, actor, in); err != nil {
		return nil, err
	}

	number, err := s.nextOrderNumber(ctx)
	if err != nil {
		return nil, err
	}

	status := models.OrderDraft
	if in.SubmitImmediately {
		status = models.OrderSubmitted
	}

	kind := in.OrderKind
	if kind == "" {
		kind = models.OrderKindStandard
	}

	order := &models.Order{
		OrderNumber:          number,
		AgreementID:          in.AgreementID,
		ShipperCompanyID:     actor.CompanyID,
		TransporterCompanyID: in.TransporterCompanyID,
		CreatedByUserID:      actor.UserID,
		OrderKind:            kind,
		StatusCode:           status,
		PickupAt:             in.PickupAt,
		DeliveryAt:           in.DeliveryAt,
		Quantity:             in.Quantity,
		WeightKg:             in.WeightKg,
		VolumeM3:             in.VolumeM3,
		Detail:               models.JSONB(in.Detail),
	}
	setIfNotEmpty(&order.OriginWarehouseID, in.OriginWarehouseID)
	setIfNotEmpty(&order.DestinationWarehouseID, in.DestinationWarehouseID)
	setIfNotEmpty(&order.CargoTypeID, in.CargoTypeID)
	setIfNotEmpty(&order.ItemTypeID, in.ItemTypeID)
	setIfNotEmpty(&order.CustomerID, in.CustomerID)
	setIfNotEmpty(&order.ReferenceNumber, in.ReferenceNumber)

	if order.Detail == nil {
		order.Detail = models.JSONB{}
	}

	// Price the order from the agreement, if one applies.
	if in.AgreementID != nil {
		if err := s.applyAgreementPrice(ctx, actor, order); err != nil {
			return nil, err
		}
	}

	if err := s.orders.Create(ctx, order); err != nil {
		return nil, err
	}

	if status == models.OrderSubmitted && order.TransporterCompanyID != nil {
		s.notifier.Notify(ctx, clients.Event{
			Type:    notificationv1.EventType_EVENT_TYPE_ORDER_CREATED,
			Subject: clients.Subject{ID: order.ID.String(), Type: "order"},
			Audience: clients.ToCompanyRoles(order.TransporterCompanyID.String(),
				models.RoleTransporter, models.RoleManager),
			ActorID:        actor.UserID.String(),
			IdempotencyKey: "order-created:" + order.ID.String(),
			Params: map[string]interface{}{
				"orderNumber": order.OrderNumber,
			},
		})
	}

	return order, nil
}

func (s *OrderService) validateCreate(ctx context.Context, actor Actor, in CreateOrderInput) error {
	if in.OrderKind != "" &&
		in.OrderKind != models.OrderKindStandard &&
		in.OrderKind != models.OrderKindEmpty &&
		in.OrderKind != models.OrderKindThreePL {
		return fmt.Errorf("%w: unknown order kind %q", ErrValidation, in.OrderKind)
	}

	// An empty repositioning trip has no cargo and no destination requirement;
	// everything else needs both ends of a journey.
	if in.OrderKind != models.OrderKindEmpty {
		if in.OriginWarehouseID == "" || in.DestinationWarehouseID == "" {
			return fmt.Errorf("%w: origin and destination are required", ErrValidation)
		}
		if in.OriginWarehouseID == in.DestinationWarehouseID {
			return fmt.Errorf("%w: origin and destination must differ", ErrValidation)
		}
	}

	if in.PickupAt != nil && in.DeliveryAt != nil && in.DeliveryAt.Before(*in.PickupAt) {
		return fmt.Errorf("%w: delivery cannot be before pickup", ErrValidation)
	}

	// Confirm the master data references exist before writing an order that
	// points at them.
	if err := s.masterdata.ValidateCatalogRefs(ctx, actor.CompanyID.String(), catalogRefs(in.CargoTypeID, in.ItemTypeID)); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}

	return nil
}

// applyAgreementPrice resolves the contracted rate for the order's lane.
func (s *OrderService) applyAgreementPrice(ctx context.Context, actor Actor, order *models.Order) error {
	agreement, err := s.agreements.FindByID(ctx, actor.CompanyID, *order.AgreementID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return fmt.Errorf("%w: agreement not found", ErrValidation)
		}
		return err
	}

	settings, err := s.auth.CompanySettings(ctx, actor.CompanyID.String())
	if err != nil {
		return fmt.Errorf("resolve company settings: %w", err)
	}

	if !agreement.IsUsable(time.Now(), settings.GetActiveAgreementVerifiedOnly()) {
		return fmt.Errorf("%w: agreement %s is not usable for new orders", ErrValidation, agreement.AgreementNumber)
	}

	order.TransporterCompanyID = &agreement.TransporterCompanyID
	order.CurrencyID = agreement.CurrencyID
	return nil
}

// Get resolves an order the caller's company is party to.
func (s *OrderService) Get(ctx context.Context, actor Actor, id uuid.UUID) (*models.Order, error) {
	return s.orders.FindByID(ctx, actor.CompanyID, id)
}

// GetForService resolves an order for a cross-service read.
func (s *OrderService) GetForService(ctx context.Context, id uuid.UUID) (*models.Order, error) {
	return s.orders.FindByIDForService(ctx, id)
}

// List pages the caller's orders.
func (s *OrderService) List(ctx context.Context, actor Actor, p query.Params) ([]models.Order, int64, error) {
	// A driver sees their own assignments, not the company's whole book.
	if actor.Role == models.RoleDriver {
		return s.orders.ListForDriver(ctx, actor.UserID, p)
	}
	return s.orders.List(ctx, actor.CompanyID, p)
}

// History returns an order's status timeline.
func (s *OrderService) History(ctx context.Context, actor Actor, id uuid.UUID) ([]models.OrderStatusEntry, error) {
	if _, err := s.orders.FindByID(ctx, actor.CompanyID, id); err != nil {
		return nil, err
	}
	return s.orders.History(ctx, id)
}

// Transition moves an order to a new state.
//
// This one method replaces the ~46 legacy routes that all pointed at a generic
// update handler. The state machine decides what is legal; the caller only says
// where they want to go.
func (s *OrderService) Transition(ctx context.Context, actor Actor, id uuid.UUID, to string, note string) (*models.Order, error) {
	order, err := s.orders.FindByID(ctx, actor.CompanyID, id)
	if err != nil {
		return nil, err
	}

	if err := models.CanTransitionOrder(order.StatusCode, to, actor.Role); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransition, err)
	}

	if err := s.assertTransitionAuthority(order, actor, to); err != nil {
		return nil, err
	}

	fields := map[string]interface{}{}
	if to == models.OrderCancelled {
		fields["cancelled_at"] = time.Now()
		fields["cancelled_by_user_id"] = actor.UserID
		if note != "" {
			fields["cancel_reason"] = note
		}
	}

	change := repository.StatusChange{
		OrderID: id,
		From:    order.StatusCode,
		To:      to,
		Actor:   &actor.UserID,
		Source:  models.SourceUser,
	}
	if note != "" {
		change.Note = &note
	}
	change.Fields = fields

	if err := s.orders.ApplyStatus(ctx, change); err != nil {
		return nil, err
	}

	s.notifyTransition(ctx, actor, order, to)

	order.StatusCode = to
	order.Status = models.StatusLabel(models.DomainOrder, to)
	order.StatusAlias = models.StatusAlias(models.DomainOrder, to)
	return order, nil
}

// assertTransitionAuthority applies the rules the state machine cannot express,
// because they depend on which side of the order the caller is on.
func (s *OrderService) assertTransitionAuthority(order *models.Order, actor Actor, to string) error {
	isShipper := order.ShipperCompanyID == actor.CompanyID
	isTransporter := order.TransporterCompanyID != nil && *order.TransporterCompanyID == actor.CompanyID

	if !isShipper && !isTransporter && actor.Role != models.RoleAdmin && actor.Role != models.RoleSuperadmin {
		return fmt.Errorf("%w: your company is not a party to this order", ErrForbidden)
	}

	switch to {
	case models.OrderApproved, models.OrderRejected, models.OrderReadyToPlan:
		// Only the carrying side accepts or plans work.
		if !isTransporter && actor.Role != models.RoleAdmin && actor.Role != models.RoleSuperadmin {
			return fmt.Errorf("%w: only the transporter may take this action", ErrForbidden)
		}
	case models.OrderSubmitted:
		if !isShipper && actor.Role != models.RoleAdmin && actor.Role != models.RoleSuperadmin {
			return fmt.Errorf("%w: only the shipper may submit this order", ErrForbidden)
		}
	}

	return nil
}

// notifyTransition tells the other side of the order what changed.
func (s *OrderService) notifyTransition(ctx context.Context, actor Actor, order *models.Order, to string) {
	eventType, ok := orderEventFor(to)
	if !ok {
		return
	}

	// Notify the counterparty, not the side that acted.
	var audience *notificationv1.Audience
	switch {
	case order.ShipperCompanyID == actor.CompanyID && order.TransporterCompanyID != nil:
		audience = clients.ToCompanyRoles(order.TransporterCompanyID.String(),
			models.RoleTransporter, models.RoleManager)
	case order.TransporterCompanyID != nil && *order.TransporterCompanyID == actor.CompanyID:
		audience = clients.ToCompanyRoles(order.ShipperCompanyID.String(), models.RoleShipper)
	default:
		return
	}

	s.notifier.Notify(ctx, clients.Event{
		Type:     eventType,
		Subject:  clients.Subject{ID: order.ID.String(), Type: "order"},
		Audience: audience,
		ActorID:  actor.UserID.String(),
		// Keyed on the transition, so a retry of the same change does not
		// notify twice.
		IdempotencyKey: fmt.Sprintf("order:%s:%s", order.ID, to),
		Params: map[string]interface{}{
			"orderNumber": order.OrderNumber,
			"status":      models.StatusLabel(models.DomainOrder, to),
		},
	})
}

func orderEventFor(status string) (notificationv1.EventType, bool) {
	switch status {
	case models.OrderSubmitted:
		return notificationv1.EventType_EVENT_TYPE_ORDER_CREATED, true
	case models.OrderApproved:
		return notificationv1.EventType_EVENT_TYPE_ORDER_UPDATED, true
	case models.OrderReadyToPlan:
		return notificationv1.EventType_EVENT_TYPE_ORDER_READY_TO_PLAN, true
	case models.OrderCancelled, models.OrderRejected:
		return notificationv1.EventType_EVENT_TYPE_ORDER_CANCELLED, true
	case models.OrderCancelRequested:
		return notificationv1.EventType_EVENT_TYPE_ORDER_APPROVAL_REQUIRED, true
	default:
		return notificationv1.EventType_EVENT_TYPE_UNSPECIFIED, false
	}
}

// AssignDriver puts a driver and truck on an order and opens its shipment.
//
// The legacy endpoint wrote both ids with no validation whatsoever. Here the
// pairing is confirmed against the master data service first, because a driver
// who is not assigned to a truck cannot lawfully drive it.
func (s *OrderService) AssignDriver(ctx context.Context, actor Actor, orderID, driverID uuid.UUID, truckID string) (*models.Order, error) {
	order, err := s.orders.FindByID(ctx, actor.CompanyID, orderID)
	if err != nil {
		return nil, err
	}

	if order.TransporterCompanyID == nil || *order.TransporterCompanyID != actor.CompanyID {
		if actor.Role != models.RoleAdmin && actor.Role != models.RoleSuperadmin {
			return nil, fmt.Errorf("%w: only the transporter may assign a driver", ErrForbidden)
		}
	}

	if err := models.CanTransitionOrder(order.StatusCode, models.OrderAssigned, actor.Role); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransition, err)
	}

	paired, err := s.masterdata.DriverIsPairedWithTruck(ctx, driverID.String(), truckID)
	if err != nil {
		return nil, fmt.Errorf("verify driver pairing: %w", err)
	}
	if !paired {
		return nil, fmt.Errorf("%w: driver %s is not assigned to truck %s", ErrValidation, driverID, truckID)
	}

	change := repository.StatusChange{
		OrderID: orderID,
		From:    order.StatusCode,
		To:      models.OrderAssigned,
		Actor:   &actor.UserID,
		Source:  models.SourceUser,
		Fields: map[string]interface{}{
			"driver_user_id": driverID,
			"truck_id":       truckID,
		},
	}
	if err := s.orders.ApplyStatus(ctx, change); err != nil {
		return nil, err
	}

	// The shipment is the driver's view of the work, so it is created at the
	// moment the work is assigned.
	shipment := &models.Shipment{
		OrderID:      orderID,
		DriverUserID: &driverID,
		TruckID:      &truckID,
		StatusCode:   models.ShipmentAssigned,
	}
	if err := s.shipments.Create(ctx, shipment); err != nil {
		return nil, fmt.Errorf("create shipment: %w", err)
	}

	s.notifier.Notify(ctx, clients.Event{
		Type:           notificationv1.EventType_EVENT_TYPE_ORDER_ASSIGNED_DRIVER,
		Subject:        clients.Subject{ID: order.ID.String(), Type: "order"},
		Audience:       clients.ToUsers(driverID.String()),
		ActorID:        actor.UserID.String(),
		IdempotencyKey: fmt.Sprintf("order:%s:assigned:%s", orderID, driverID),
		Params: map[string]interface{}{
			"orderNumber": order.OrderNumber,
		},
	})

	order.StatusCode = models.OrderAssigned
	order.DriverUserID = &driverID
	order.TruckID = &truckID
	return order, nil
}

// draftUpdatable is the allowlist for editing an order.
//
// The legacy update handler wrote whatever map the request body decoded to,
// which meant any caller could change price, status or company on any order.
var draftUpdatable = map[string]string{
	"pickupAt":        "pickup_at",
	"deliveryAt":      "delivery_at",
	"quantity":        "quantity",
	"weightKg":        "weight_kg",
	"volumeM3":        "volume_m3",
	"cargoTypeId":     "cargo_type_id",
	"itemTypeId":      "item_type_id",
	"customerId":      "customer_id",
	"referenceNumber": "reference_number",
	"detail":          "detail",
}

// UpdateDraft edits an order that has not yet been submitted.
//
// Editing is confined to the draft state on purpose: once a transporter has
// accepted an order, changing its weight or dates behind their back is a
// commercial problem, not a convenience.
func (s *OrderService) UpdateDraft(ctx context.Context, actor Actor, id uuid.UUID, input map[string]interface{}) error {
	order, err := s.orders.FindByID(ctx, actor.CompanyID, id)
	if err != nil {
		return err
	}
	if order.StatusCode != models.OrderDraft {
		return fmt.Errorf("%w: only a draft order can be edited (this one is %s)", ErrTransition, order.StatusCode)
	}
	if order.ShipperCompanyID != actor.CompanyID {
		return fmt.Errorf("%w: only the shipper may edit this order", ErrForbidden)
	}

	fields := map[string]interface{}{}
	for k, v := range input {
		if col, ok := draftUpdatable[k]; ok {
			fields[col] = v
		}
	}
	if len(fields) == 0 {
		return fmt.Errorf("%w: no editable fields supplied", ErrValidation)
	}

	return s.orders.UpdateFields(ctx, id, fields)
}

// Summary returns the dashboard counts.
func (s *OrderService) Summary(ctx context.Context, actor Actor) (map[string]int64, error) {
	return s.orders.CountByStatus(ctx, actor.CompanyID)
}

// nextOrderNumber builds a per-year sequential number.
func (s *OrderService) nextOrderNumber(ctx context.Context) (string, error) {
	now := time.Now()
	period := now.Format("2006")

	seq, err := s.orders.NextNumber(ctx, "order", period)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("ORD-%s-%06d", period, seq), nil
}

func setIfNotEmpty(target **string, value string) {
	if value != "" {
		v := value
		*target = &v
	}
}
