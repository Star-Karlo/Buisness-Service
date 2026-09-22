// Package services holds the business rules for orders, shipments, agreements
// and invoices.
package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/karlo/business-service/internal/clients"
	"github.com/karlo/business-service/internal/fieldconfig"
	"github.com/karlo/business-service/internal/models"
	masterdatav1 "github.com/karlo/business-service/internal/platform/genproto/karlo/masterdata/v1"
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
//
// CompanyID is the company being ACTED FOR, which is normally the caller's own.
// Karlo staff may act for a client — recording an order on their behalf, or
// configuring their forms — and then CompanyID is the client's while UserID
// stays the staff member's. Keeping them apart is what makes the audit trail
// honest: the order belongs to the client, and the person who typed it is still
// recorded.
type Actor struct {
	UserID    uuid.UUID
	CompanyID uuid.UUID
	Role      string

	// PlatformStaff marks a Karlo employee, who may act across tenants.
	PlatformStaff bool

	// ActingFor is set when CompanyID is NOT the caller's own company. Notified
	// parties and history entries can then say so rather than presenting the
	// action as the client's own.
	ActingFor bool

	// StatusBypass is the TEMPORARY end-to-end testing switch Nathan asked
	// for (Sept 2026): a company Administrator (or Karlo staff) who sends
	// X-Status-Bypass: 1 may take state-machine steps that belong to another
	// role — walking an order through the driver's and warehouse's steps
	// without the driver app. It widens the machine's ROLE check only;
	// tenancy and ownership checks are untouched. Disabled with
	// STATUS_BYPASS_ENABLED=false; remove together with the console's Mode Uji.
	StatusBypass bool
}

// OrderService owns the order lifecycle.
type OrderService struct {
	orders     *repository.OrderRepository
	shipments  *repository.ShipmentRepository
	agreements *repository.AgreementRepository
	auth       *clients.Auth
	masterdata *clients.MasterData
	notifier   clients.Notifier

	// fields enforces the company's form configuration; items stores itemised
	// cargo. Both attached after construction, for the same reason as dispatch.
	fields *FieldConfigService
	items  *repository.OrderItemRepository

	// dispatch plans the haul route when an order is created. Set after
	// construction rather than injected, because DispatchService needs the
	// order repository this service also owns; passing each into the other's
	// constructor is a cycle. Nil-checked at every use, so a deployment that
	// has not wired it still creates orders — without their routes.
	dispatch *DispatchService
}

// WithDispatch attaches route planning. See the field comment for why this is
// not a constructor argument.
func (s *OrderService) WithDispatch(d *DispatchService) { s.dispatch = d }

// WithFieldConfig attaches the per-company field check and item storage.
func (s *OrderService) WithFieldConfig(f *FieldConfigService, items *repository.OrderItemRepository) {
	s.fields = f
	s.items = items
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
	ExpiresAt              *time.Time
	CustomerID             string
	ReferenceNumber        string
	Detail                 map[string]interface{}
	// SubmitImmediately skips the draft state for clients that have no draft UI.
	SubmitImmediately bool

	// CustomerCompanyID is set when the TRANSPORTER enters the order for one
	// of its customers — the revamp's Input Order wizard. The actor is then
	// the transporter and this is the shipper; the order is born approved,
	// ready for a truck, because the party that would approve it is the one
	// typing it in.
	CustomerCompanyID *uuid.UUID

	// Items is the itemised cargo, for the companies whose configuration
	// enables it. Empty for those that name a category and stop.
	Items []OrderItemInput

	// Present names the payload keys the caller actually supplied, for the
	// per-company field check. See CreateAgreementInput.Present.
	Present map[string]bool
}

// OrderItemInput is one line of itemised cargo.
type OrderItemInput struct {
	CatalogItemID string
	Name          string
	Quantity      *decimal.Decimal
	Unit          string
	Packaging     string
	WeightKg      *decimal.Decimal
	LengthCm      *decimal.Decimal
	WidthCm       *decimal.Decimal
	HeightCm      *decimal.Decimal
	HandlingNotes string
}

// Create places a new order.
func (s *OrderService) Create(ctx context.Context, actor Actor, in CreateOrderInput) (*models.Order, error) {
	if err := s.validateCreate(ctx, actor, in); err != nil {
		return nil, err
	}

	shipperID, transporterID := actor.CompanyID, in.TransporterCompanyID
	status := models.OrderDraft
	if in.SubmitImmediately {
		status = models.OrderSubmitted
	}
	var number string
	if in.CustomerCompanyID != nil {
		shipperID = *in.CustomerCompanyID
		transporterID = &actor.CompanyID
		status = models.OrderApproved
		// The wizard's own id: ORM + yymmddHHMMSS, "-n" per shipment of a
		// batch. Time-based, so two planners in the same second could
		// collide; the sequence suffix below keeps it unique without making
		// the number unreadable.
		number = fmt.Sprintf("ORM%s", time.Now().Format("060102150405"))
		if seq, err := s.orders.NextNumber(ctx, "orm", time.Now().Format("060102150405")); err == nil && seq > 1 {
			number = fmt.Sprintf("%s-%d", number, seq)
		}
	} else {
		n, err := s.nextOrderNumber(ctx)
		if err != nil {
			return nil, err
		}
		number = n
	}

	kind := in.OrderKind
	if kind == "" {
		kind = models.OrderKindStandard
	}

	order := &models.Order{
		OrderNumber:          number,
		AgreementID:          in.AgreementID,
		ShipperCompanyID:     shipperID,
		TransporterCompanyID: transporterID,
		CreatedByUserID:      actor.UserID,
		OrderKind:            kind,
		StatusCode:           status,
		PickupAt:             in.PickupAt,
		DeliveryAt:           in.DeliveryAt,
		ExpiresAt:            in.ExpiresAt,
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

	// Itemised cargo, for the companies whose configuration enables it.
	//
	// Written after the order rather than inside its transaction: the lines are
	// supporting detail, and an order that exists without them is a form
	// somebody can complete, while an order refused because one line was
	// malformed loses everything typed. A failure is logged and the volume
	// roll-up simply reads short.
	if s.items != nil && len(in.Items) > 0 {
		if err := s.items.ReplaceForOrder(ctx, order.ID, itemsFrom(in.Items)); err != nil {
			slog.WarnContext(ctx, "order items not stored", "orderId", order.ID, "error", err)
		}
	}

	// Plan the loading-to-unloading route. Known as soon as the order exists
	// and independent of who ends up driving it, so it is done here rather than
	// at assignment — which also means the planner's screen already has a
	// distance when they first open it.
	//
	// A failure is logged, not returned. An order without a planned route is
	// recoverable at any time; an order refused because MAPID was briefly
	// unwell is a sale lost to somebody else's outage.
	if s.dispatch != nil {
		if err := s.dispatch.PlanHaul(ctx, order); err != nil {
			slog.WarnContext(ctx, "haul route not planned",
				"orderId", order.ID, "error", err)
		}
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

	// An expiry after the loading time describes an order that may be actioned
	// after the truck was due, which is not a schedule. The PRD states the rule
	// in both directions in consecutive bullets; this is the reading that
	// leaves the order actionable.
	if in.ExpiresAt != nil && in.PickupAt != nil && in.ExpiresAt.After(*in.PickupAt) {
		return fmt.Errorf("%w: the order expires after its loading time", ErrValidation)
	}

	// The company's own form configuration. Enforced here and not only in the
	// browser: the API is reachable by anyone holding a token, and a rule the
	// page enforces alone is not a rule.
	if s.fields != nil {
		if err := s.fields.Validate(ctx, actor.CompanyID, fieldconfig.EntityOrder, in.Present); err != nil {
			return err
		}
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

	// Judged on the order's load date, not the booking date: an order for
	// next week may be priced by next week's contract today.
	on := time.Now()
	if order.PickupAt != nil {
		on = *order.PickupAt
	}
	if reason := agreement.UnusableReason(on, settings.GetActiveAgreementVerifiedOnly()); reason != "" {
		return fmt.Errorf("%w: agreement %s tidak bisa dipakai untuk order baru: %s", ErrValidation, agreement.AgreementNumber, reason)
	}

	order.TransporterCompanyID = &agreement.TransporterCompanyID
	order.CurrencyID = agreement.CurrencyID

	// Record the lineage alongside the version. AgreementID names the version
	// the price came from; without the root, an amendment leaves this order
	// pointing at a superseded row and "every order under this contract"
	// becomes a recursive walk instead of one indexed read.
	root := agreement.RootAgreementID
	if root == uuid.Nil {
		// An agreement written before versioning existed is its own lineage.
		root = agreement.ID
	}
	order.AgreementRootID = &root

	// Resolve the contracted rate for this lane.
	//
	// The rate lives per origin-destination-trucktype line, and the order names
	// warehouses rather than cities, so both ends are resolved through master
	// data first. Until this existed the agreement set the transporter and the
	// currency and left price NULL — so every agreement-backed order was
	// unpriced, and the omission was invisible because nothing downstream
	// insisted on a price until invoicing.
	lane, ok := s.laneOf(ctx, order)
	if !ok {
		// No lane means no rate to look up. Left unpriced rather than refused:
		// an empty repositioning order legitimately has no destination, and a
		// warehouse missing its city is a master-data problem that should not
		// block the order.
		return nil
	}

	if order.TruckID != nil {
		if truck, err := s.masterdata.GetTruck(ctx, *order.TruckID); err == nil {
			lane.TruckTypeID = truck.GetTruckTypeId()
		}
	}

	rate, err := s.agreements.FindRate(ctx, agreement.ID, lane)
	if err != nil && errors.Is(err, repository.ErrNotFound) {
		// Before refusing, two matches the city lookup cannot make:
		//   - a rate written against the same warehouses the order names,
		//     which is more specific than any city and needs no master-data
		//     city id on the warehouse;
		//   - an agreement with exactly one rate — the revamp's MyAgreement,
		//     which prices one tarif per contract and names its route as
		//     text. That rate IS the contract's price.
		if r := rateByWarehouseOrOnlyRate(agreement, order); r != nil {
			rate, err = r, nil
		}
	}
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// The agreement exists but does not cover this lane. Refused,
			// because the alternative is an order that looks contracted and
			// carries no agreed price — which surfaces at invoicing, after the
			// work is done and the argument is expensive.
			return fmt.Errorf("%w: agreement %s has no rate for this lane",
				ErrValidation, agreement.AgreementNumber)
		}
		return err
	}

	order.Price = &rate.Price
	if rate.CurrencyID != nil {
		order.CurrencyID = rate.CurrencyID
	}
	if rate.PricingTypeID != nil {
		order.Detail["pricingTypeId"] = *rate.PricingTypeID
	}
	order.Detail["agreementRateId"] = rate.ID.String()

	return nil
}

// laneOf resolves an order's warehouses to the lane its rates are keyed on.
//
// Reports false when either end cannot be resolved to a city, which leaves the
// order unpriced rather than refused: a warehouse master data cannot return, or
// one with no city recorded, is somebody's data to fix and should not stop an
// order that is otherwise complete.
//
// The district pair is left EMPTY, which FindRate reads as "do not narrow by
// district" — so a company pricing by kecamatan still resolves a rate here, it
// simply gets the containing city's line rather than the kecamatan one.
//
// It is empty because master data cannot yet supply a district this can match
// on, and the reason is a real inconsistency rather than a missing field. The
// Site model records city, province and district as NAMES — there is no region
// table, deliberately — while agreement_rates keys its lanes on catalogue ids
// (`origin_city_id`, populated from the `kota` catalogue). Matching a name
// against an id resolves nothing.
//
// So kecamatan lanes can be DEFINED on an agreement today and are matched when
// the order names them, but they are not derived from the warehouse. Closing
// that needs the city-id-versus-city-name split settled across the two
// services, which is a decision larger than this function. Fabricating an id
// here would produce a lane that silently matches nothing.
func (s *OrderService) laneOf(ctx context.Context, order *models.Order) (repository.Lane, bool) {
	resolve := func(id *string) string {
		if id == nil {
			return ""
		}
		wh, err := s.masterdata.GetWarehouse(ctx, *id)
		if err != nil {
			slog.WarnContext(ctx, "warehouse not resolved for pricing",
				"warehouseId", *id, "error", err)
			return ""
		}
		return wh.GetCityId()
	}

	lane := repository.Lane{
		OriginCityID:      resolve(order.OriginWarehouseID),
		DestinationCityID: resolve(order.DestinationWarehouseID),
	}

	// An order may name its own kecamatan even though the warehouse cannot
	// supply one, which is how a company that prices below city level gets the
	// specific rate: the form asks, and the value rides in detail.
	lane.OriginDistrictID = stringFromDetail(order.Detail, "originDistrictId")
	lane.DestinationDistrictID = stringFromDetail(order.Detail, "destinationDistrictId")

	return lane, lane.OriginCityID != "" && lane.DestinationCityID != ""
}

// Get resolves an order the caller's company is party to.
func (s *OrderService) Get(ctx context.Context, actor Actor, id uuid.UUID) (*models.Order, error) {
	order, err := s.orders.FindByID(ctx, actor.CompanyID, id)
	if err != nil {
		return nil, err
	}
	// Same names as the list, so a detail page and the row it was opened from
	// do not disagree about where an order is going.
	single := []models.Order{*order}
	s.resolveNames(ctx, single)
	return &single[0], nil
}

// GetForService resolves an order for a cross-service read.
func (s *OrderService) GetForService(ctx context.Context, id uuid.UUID) (*models.Order, error) {
	return s.orders.FindByIDForService(ctx, id)
}

// List pages the caller's orders.
func (s *OrderService) List(ctx context.Context, actor Actor, p query.Params) ([]models.Order, int64, error) {
	// A driver sees their own assignments, not the company's whole book.
	var (
		orders []models.Order
		total  int64
		err    error
	)
	if actor.Role == models.RoleDriver {
		orders, total, err = s.orders.ListForDriver(ctx, actor.UserID, p)
	} else {
		orders, total, err = s.orders.List(ctx, actor.CompanyID, p)
	}
	if err != nil {
		return nil, 0, err
	}

	s.resolveNames(ctx, orders)
	return orders, total, nil
}

// resolveNames fills in the display names behind the master data ids an order
// carries: warehouses and the truck.
//
// The ids alone are Mongo ObjectIds owned by another service, so a list
// rendered from them shows a column of hex. Resolving here costs one lookup per
// DISTINCT id per page — two orders between the same pair of warehouses cost
// two lookups, not four.
//
// Failure is deliberately not returned. Master data being unreachable should
// leave the route column blank, not take the order list down with it; the order
// itself is authoritative and complete without the names.
func (s *OrderService) resolveNames(ctx context.Context, orders []models.Order) {
	if s.masterdata == nil || len(orders) == 0 {
		return
	}

	warehouses := map[string]string{}
	trucks := map[string]string{}

	for i := range orders {
		for _, id := range []*string{orders[i].OriginWarehouseID, orders[i].DestinationWarehouseID} {
			if id != nil && *id != "" {
				warehouses[*id] = ""
			}
		}
		if orders[i].TruckID != nil && *orders[i].TruckID != "" {
			trucks[*orders[i].TruckID] = ""
		}
	}

	for id := range warehouses {
		w, err := s.masterdata.GetWarehouse(ctx, id)
		if err != nil {
			slog.WarnContext(ctx, "could not resolve a warehouse name",
				"warehouse_id", id, "error", err)
			continue
		}
		warehouses[id] = w.GetName()
	}
	for id := range trucks {
		t, err := s.masterdata.GetTruck(ctx, id)
		if err != nil {
			slog.WarnContext(ctx, "could not resolve a truck",
				"truck_id", id, "error", err)
			continue
		}
		trucks[id] = t.GetPoliceNumber()
	}

	// Companies, drivers and shipments: one lookup per distinct id, not per
	// row. The list page shows shipper, driver and a status derived from the
	// shipment; without these it would show UUIDs or make a call per row.
	companyIDs := make([]string, 0, len(orders)*2)
	drivers := map[string]string{}
	for i := range orders {
		companyIDs = append(companyIDs, orders[i].ShipperCompanyID.String())
		if orders[i].TransporterCompanyID != nil {
			companyIDs = append(companyIDs, orders[i].TransporterCompanyID.String())
		}
		if orders[i].DriverID != nil && *orders[i].DriverID != "" {
			drivers[*orders[i].DriverID] = ""
		}
	}
	profiles := map[string]clients.CompanyProfile{}
	if s.auth != nil {
		profiles = s.auth.CompanyProfiles(ctx, companyIDs)
	}
	for id := range drivers {
		d, err := s.masterdata.GetDriver(ctx, id)
		if err != nil {
			continue
		}
		drivers[id] = d.GetFullName()
	}
	truckTypes := map[string]string{}
	for id := range trucks {
		t, err := s.masterdata.GetTruck(ctx, id)
		if err == nil {
			truckTypes[id] = t.GetTruckTypeId()
		}
	}

	for i := range orders {
		if p, ok := profiles[orders[i].ShipperCompanyID.String()]; ok {
			orders[i].ShipperCompanyName = p.Name
		}
		if t := orders[i].TransporterCompanyID; t != nil {
			if p, ok := profiles[t.String()]; ok {
				orders[i].TransporterCompanyName = p.Name
			}
		}
		if id := orders[i].DriverID; id != nil {
			orders[i].DriverName = drivers[*id]
		}
		if id := orders[i].TruckID; id != nil {
			orders[i].TruckTypeName = truckTypes[*id]
		}
		if s.shipments != nil {
			if sh, err := s.shipments.FindByOrder(ctx, orders[i].ID); err == nil && sh != nil {
				orders[i].ShipmentStatusCode = sh.StatusCode
				orders[i].ShipmentAcceptedAt = sh.AcceptedAt
			}
		}
		if id := orders[i].OriginWarehouseID; id != nil {
			orders[i].OriginWarehouseName = warehouses[*id]
		}
		if id := orders[i].DestinationWarehouseID; id != nil {
			orders[i].DestinationWarehouseName = warehouses[*id]
		}
		if id := orders[i].TruckID; id != nil {
			orders[i].TruckPoliceNumber = trucks[*id]
		}
	}
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

	if err := models.CanTransitionOrder(order.StatusCode, to, machineRole(actor, order)); err != nil {
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

// machineRole is the role the state machines are asked about. Personas the
// token can carry on its own (admin, driver, warehouse PIC, manager) stand;
// for everyone else the persona is which side of the order their company is
// on — a fact about the order, not the token, which only knows the tenant's
// role name.
func machineRole(actor Actor, order *models.Order) string {
	if actor.StatusBypass {
		return models.RoleAdmin
	}
	switch actor.Role {
	case models.RoleAdmin, models.RoleSuperadmin, models.RoleDriver, models.RoleWarehousePic, models.RoleManager:
		return actor.Role
	}
	if order != nil {
		if order.TransporterCompanyID != nil && *order.TransporterCompanyID == actor.CompanyID {
			return models.RoleTransporter
		}
		if order.ShipperCompanyID == actor.CompanyID {
			return models.RoleShipper
		}
	}
	return actor.Role
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
// The driver is a master-data record — a person with a phone and a licence,
// who usually has no login. The legacy endpoint wrote both ids with no
// validation whatsoever. Here the pairing is confirmed against the master
// data service first, because a driver who is not assigned to a truck cannot
// lawfully drive it; and the driver is told, on WhatsApp to the phone master
// data holds and in-app when they do have a login.
func (s *OrderService) AssignDriver(ctx context.Context, actor Actor, orderID uuid.UUID, driverID, truckID string) (*models.Order, error) {
	order, err := s.orders.FindByID(ctx, actor.CompanyID, orderID)
	if err != nil {
		return nil, err
	}

	if order.TransporterCompanyID == nil || *order.TransporterCompanyID != actor.CompanyID {
		if actor.Role != models.RoleAdmin && actor.Role != models.RoleSuperadmin {
			return nil, fmt.Errorf("%w: only the transporter may assign a driver", ErrForbidden)
		}
	}

	if err := models.CanTransitionOrder(order.StatusCode, models.OrderAssigned, machineRole(actor, order)); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransition, err)
	}

	driver, err := s.masterdata.GetDriver(ctx, driverID)
	if err != nil {
		return nil, fmt.Errorf("%w: driver %s not found", ErrValidation, driverID)
	}
	if driver.GetStatus() != "" && driver.GetStatus() != "active" {
		return nil, fmt.Errorf("%w: driver %s is %s", ErrValidation, driver.GetFullName(), driver.GetStatus())
	}

	paired, err := s.masterdata.DriverIsPairedWithTruck(ctx, driverID, truckID)
	if err != nil {
		return nil, fmt.Errorf("verify driver pairing: %w", err)
	}
	if !paired {
		return nil, fmt.Errorf("%w: driver %s is not assigned to truck %s", ErrValidation, driver.GetFullName(), truckID)
	}

	// The login, when there is one, is what the driver app authenticates as
	// and what the handover checks against.
	var driverUserID *uuid.UUID
	if u, err := uuid.Parse(driver.GetUserId()); err == nil && u != uuid.Nil {
		driverUserID = &u
	}

	fields := map[string]interface{}{
		"driver_id":      driverID,
		"driver_user_id": driverUserID,
		"truck_id":       truckID,
	}
	change := repository.StatusChange{
		OrderID: orderID,
		From:    order.StatusCode,
		To:      models.OrderAssigned,
		Actor:   &actor.UserID,
		Source:  models.SourceUser,
		Fields:  fields,
	}
	if err := s.orders.ApplyStatus(ctx, change); err != nil {
		return nil, err
	}

	// The shipment is the driver's view of the work, so it is created at the
	// moment the work is assigned.
	shipment := &models.Shipment{
		OrderID:      orderID,
		DriverID:     &driverID,
		DriverUserID: driverUserID,
		TruckID:      &truckID,
		StatusCode:   models.ShipmentAssigned,
	}
	if err := s.shipments.Create(ctx, shipment); err != nil {
		return nil, fmt.Errorf("create shipment: %w", err)
	}

	s.notifyDriverAssigned(ctx, actor, order, driver)

	order.StatusCode = models.OrderAssigned
	order.DriverID = &driverID
	order.DriverUserID = driverUserID
	order.TruckID = &truckID

	// Plan the truck's run to the loading point. Only possible now: the
	// approach starts wherever the assigned truck is, so it does not exist
	// until there is an assigned truck, and reassigning replaces it.
	//
	// Logged rather than returned for the same reason as the haul: the driver
	// has the job either way, and refusing an assignment because a routing call
	// failed would leave the order unallocated for no operational gain.
	if s.dispatch != nil {
		if err := s.dispatch.PlanApproach(ctx, order); err != nil {
			slog.WarnContext(ctx, "approach route not planned",
				"orderId", order.ID, "truckId", truckID, "error", err)
		}
	}

	return order, nil
}

// notifyDriverAssigned tells the driver about the job on every channel that
// can reach them: WhatsApp to the phone master data holds, which works for a
// driver with no account, and in-app/push when they do have a login. Both
// carry the same idempotency key per channel so a retried assignment does
// not send twice.
func (s *OrderService) notifyDriverAssigned(ctx context.Context, actor Actor, order *models.Order, driver *masterdatav1.Driver) {
	params := map[string]interface{}{
		"orderNumber": order.OrderNumber,
		"origin":      s.warehouseName(ctx, order.OriginWarehouseID),
		"destination": s.warehouseName(ctx, order.DestinationWarehouseID),
	}
	base := clients.Event{
		Type:    notificationv1.EventType_EVENT_TYPE_ORDER_ASSIGNED_DRIVER,
		Subject: clients.Subject{ID: order.ID.String(), Type: "order"},
		ActorID: actor.UserID.String(),
		Params:  params,
	}

	if phone := driver.GetPhone(); phone != "" {
		ev := base
		ev.Audience = clients.ToPhone(phone)
		ev.IdempotencyKey = fmt.Sprintf("order:%s:assigned:%s:phone", order.ID, driver.GetId())
		s.notifier.Notify(ctx, ev)
	}
	if uid := driver.GetUserId(); uid != "" {
		ev := base
		ev.Audience = clients.ToUsers(uid)
		ev.IdempotencyKey = fmt.Sprintf("order:%s:assigned:%s:user", order.ID, driver.GetId())
		s.notifier.Notify(ctx, ev)
	}
	if driver.GetPhone() == "" && driver.GetUserId() == "" {
		slog.WarnContext(ctx, "driver assigned but unreachable: no phone and no login",
			"orderId", order.ID, "driverId", driver.GetId())
	}
}

// warehouseName labels a warehouse for a message, or "-" when it cannot be
// resolved: a notification with a blank is still worth sending.
func (s *OrderService) warehouseName(ctx context.Context, id *string) string {
	if id == nil || *id == "" {
		return "-"
	}
	wh, err := s.masterdata.GetWarehouse(ctx, *id)
	if err != nil || wh.GetName() == "" {
		return "-"
	}
	return wh.GetName()
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

// itemsFrom converts submitted lines into rows.
//
// Volume is computed HERE and stored, not derived on read. It comes from the
// dimensions as they were at order time; recomputing it later from a catalogue
// item whose dimensions have since been corrected would silently restate a
// shipped order.
func itemsFrom(in []OrderItemInput) []models.OrderItem {
	out := make([]models.OrderItem, 0, len(in))
	for _, item := range in {
		row := models.OrderItem{
			Name:     item.Name,
			Quantity: item.Quantity,
			WeightKg: item.WeightKg,
			LengthCm: item.LengthCm,
			WidthCm:  item.WidthCm,
			HeightCm: item.HeightCm,
		}
		setIfNotEmpty(&row.CatalogItemID, item.CatalogItemID)
		setIfNotEmpty(&row.Unit, item.Unit)
		setIfNotEmpty(&row.Packaging, item.Packaging)
		setIfNotEmpty(&row.HandlingNotes, item.HandlingNotes)

		// All three dimensions or none. A partial entry leaves volume blank
		// rather than treating a missing height as zero, which would make a
		// crate of any size occupy nothing.
		if item.LengthCm != nil && item.WidthCm != nil && item.HeightCm != nil {
			// centimetres cubed to cubic metres.
			volume := item.LengthCm.Mul(*item.WidthCm).Mul(*item.HeightCm).
				Div(decimal.NewFromInt(1_000_000))
			row.VolumeM3 = &volume
		}

		out = append(out, row)
	}
	return out
}

// stringFromDetail reads one string out of an order's free-form detail.
//
// Returns empty for a missing key or a non-string value rather than erroring:
// detail is client-supplied and unvalidated, and a malformed entry should widen
// the rate lookup, not fail the order.
func stringFromDetail(detail models.JSONB, key string) string {
	if detail == nil {
		return ""
	}
	if v, ok := detail[key].(string); ok {
		return v
	}
	return ""
}

func setIfNotEmpty(target **string, value string) {
	if value != "" {
		v := value
		*target = &v
	}
}

// PatchDetail merges keys into an order's detail, for the working data the
// console keeps on an order that has no column of its own — POD review
// figures, invoice adjustments, post-trip reconciliation. Keys are replaced
// wholesale (a nested object is a unit), and a null removes a key. Either
// party to the order may write; the detail is shared working state.
func (s *OrderService) PatchDetail(ctx context.Context, actor Actor, id uuid.UUID, patch map[string]interface{}) (*models.Order, error) {
	order, err := s.orders.FindByID(ctx, actor.CompanyID, id)
	if err != nil {
		return nil, err
	}
	detail := map[string]interface{}(order.Detail)
	if detail == nil {
		detail = map[string]interface{}{}
	}
	for k, v := range patch {
		if v == nil {
			delete(detail, k)
			continue
		}
		detail[k] = v
	}
	if err := s.orders.UpdateFields(ctx, id, map[string]interface{}{"detail": models.JSONB(detail)}); err != nil {
		return nil, err
	}
	order.Detail = models.JSONB(detail)
	return order, nil
}

func rateByWarehouseOrOnlyRate(agreement *models.Agreement, order *models.Order) *models.AgreementRate {
	for i := range agreement.Rates {
		r := &agreement.Rates[i]
		if r.OriginWarehouseID != nil && r.DestinationWarehouseID != nil &&
			order.OriginWarehouseID != nil && order.DestinationWarehouseID != nil &&
			*r.OriginWarehouseID == *order.OriginWarehouseID && *r.DestinationWarehouseID == *order.DestinationWarehouseID {
			return r
		}
	}
	if len(agreement.Rates) == 1 {
		return &agreement.Rates[0]
	}
	return nil
}
