// Package grpcserver implements the BusinessService contract.
//
// The RPCs are mostly read-side. Other services ask this one about orders and
// invoices; they do not drive its state machines, which belong to the people
// using the HTTP API. The one exception is ReportGeofenceEvent, the inbound
// edge from the telemetry service.
package grpcserver

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/karlo/business-service/internal/models"
	businessv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/business/v1"
	"github.com/karlo/business-service/internal/platform/query"
	"github.com/karlo/business-service/internal/repository"
	"github.com/karlo/business-service/internal/services"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Server struct {
	businessv1.UnimplementedBusinessServiceServer

	orders    *services.OrderService
	shipments *services.ShipmentService
	billing   *services.BillingService
	orderRepo *repository.OrderRepository
}

func New(
	orders *services.OrderService,
	shipments *services.ShipmentService,
	billing *services.BillingService,
	orderRepo *repository.OrderRepository,
) *Server {
	return &Server{orders: orders, shipments: shipments, billing: billing, orderRepo: orderRepo}
}

func (s *Server) GetOrder(ctx context.Context, req *businessv1.GetOrderRequest) (*businessv1.GetOrderResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed order id")
	}

	order, err := s.orders.GetForService(ctx, id)
	if err != nil {
		return nil, mapError(err, "order")
	}

	return &businessv1.GetOrderResponse{Order: toProtoOrder(order)}, nil
}

func (s *Server) ListOrders(ctx context.Context, req *businessv1.ListOrdersRequest) (*businessv1.ListOrdersResponse, error) {
	// The company id is mandatory. An unscoped order listing would return every
	// tenant's commercial data in one call.
	companyID, err := uuid.Parse(req.GetCompanyId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "company_id is required and must be a UUID")
	}

	params := query.FromProto(req.GetQuery(), repository.OrderFields())

	orders, total, err := s.orderRepo.List(ctx, companyID, params)
	if err != nil {
		return nil, mapError(err, "orders")
	}

	out := make([]*businessv1.Order, 0, len(orders))
	for i := range orders {
		out = append(out, toProtoOrder(&orders[i]))
	}

	return &businessv1.ListOrdersResponse{
		Orders:   out,
		PageInfo: params.PageInfo(total),
	}, nil
}

// GetOrderSummary returns the small projection other services need to render a
// reference to an order without pulling the whole aggregate.
func (s *Server) GetOrderSummary(ctx context.Context, req *businessv1.GetOrderSummaryRequest) (*businessv1.GetOrderSummaryResponse, error) {
	ids := make([]uuid.UUID, 0, len(req.GetIds()))
	for _, raw := range req.GetIds() {
		if id, err := uuid.Parse(raw); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return &businessv1.GetOrderSummaryResponse{}, nil
	}

	orders, err := s.orderRepo.FindByIDs(ctx, ids)
	if err != nil {
		return nil, mapError(err, "orders")
	}

	out := make([]*businessv1.OrderSummary, 0, len(orders))
	for i := range orders {
		o := &orders[i]
		summary := &businessv1.OrderSummary{
			Id:               o.ID.String(),
			OrderNumber:      o.OrderNumber,
			StatusCode:       o.StatusCode,
			Status:           models.StatusLabel(models.DomainOrder, o.StatusCode),
			ShipperCompanyId: o.ShipperCompanyID.String(),
		}
		if o.TransporterCompanyID != nil {
			summary.TransporterCompanyId = o.TransporterCompanyID.String()
		}
		if o.DriverUserID != nil {
			summary.DriverId = o.DriverUserID.String()
		}
		out = append(out, summary)
	}

	return &businessv1.GetOrderSummaryResponse{Summaries: out}, nil
}

func (s *Server) GetAgreement(ctx context.Context, req *businessv1.GetAgreementRequest) (*businessv1.GetAgreementResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed agreement id")
	}

	agreement, err := s.billing.GetAgreementForService(ctx, id)
	if err != nil {
		return nil, mapError(err, "agreement")
	}

	return &businessv1.GetAgreementResponse{
		Agreement: &businessv1.Agreement{
			Id:                   agreement.ID.String(),
			AgreementNumber:      agreement.AgreementNumber,
			ShipperCompanyId:     agreement.ShipperCompanyID.String(),
			TransporterCompanyId: agreement.TransporterCompanyID.String(),
			StatusCode:           agreement.StatusCode,
			Status:               models.StatusLabel(models.DomainAgreement, agreement.StatusCode),
			ValidFrom:            timestamppb.New(agreement.ValidFrom),
			ValidUntil:           timestamppb.New(agreement.ValidUntil),
			Verified:             agreement.Verified,
			Detail:               toStruct(agreement.Detail),
			CreatedAt:            timestamppb.New(agreement.CreatedAt),
		},
	}, nil
}

// GetInvoice is the accounting service's read path into freight billing.
func (s *Server) GetInvoice(ctx context.Context, req *businessv1.GetInvoiceRequest) (*businessv1.GetInvoiceResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed invoice id")
	}

	inv, err := s.billing.GetInvoiceForService(ctx, id)
	if err != nil {
		return nil, mapError(err, "invoice")
	}

	out := &businessv1.Invoice{
		Id:                   inv.ID.String(),
		InvoiceNumber:        inv.InvoiceNumber,
		ShipperCompanyId:     inv.ShipperCompanyID.String(),
		TransporterCompanyId: inv.TransporterCompanyID.String(),
		StatusCode:           inv.StatusCode,
		// Amounts cross the wire as float64 because that is what the contract
		// declares. They are computed and stored as exact decimals; this is a
		// display projection, and the accounting service reconciles against the
		// stored values, not these.
		Subtotal: mustFloat(inv.Subtotal),
		Ppn:      mustFloat(inv.PPNAmount),
		Pph23:    mustFloat(inv.PPH23Amount),
		Total:    mustFloat(inv.Total),
	}
	if len(inv.Lines) == 1 {
		out.OrderId = inv.Lines[0].OrderID.String()
	}
	if inv.CurrencyID != nil {
		out.CurrencyId = *inv.CurrencyID
	}
	if inv.IssuedAt != nil {
		out.IssuedAt = timestamppb.New(*inv.IssuedAt)
	}
	if inv.PaidAt != nil {
		out.PaidAt = timestamppb.New(*inv.PaidAt)
	}

	return &businessv1.GetInvoiceResponse{Invoice: out}, nil
}

// GetActiveShipmentByDriver tells the telemetry service which shipment a
// driver's position reports belong to.
func (s *Server) GetActiveShipmentByDriver(ctx context.Context, req *businessv1.GetActiveShipmentByDriverRequest) (*businessv1.GetActiveShipmentByDriverResponse, error) {
	driverID, err := uuid.Parse(req.GetDriverId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed driver id")
	}

	shipment, err := s.shipments.ActiveForDriver(ctx, driverID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			// No active shipment is a normal answer, not an error: a driver is
			// idle most of the time.
			return &businessv1.GetActiveShipmentByDriverResponse{Found: false}, nil
		}
		return nil, mapError(err, "shipment")
	}

	out := &businessv1.Shipment{
		Id:         shipment.ID.String(),
		OrderId:    shipment.OrderID.String(),
		StatusCode: shipment.StatusCode,
	}
	if shipment.DriverUserID != nil {
		out.DriverId = shipment.DriverUserID.String()
	}
	if shipment.TruckID != nil {
		out.TruckId = *shipment.TruckID
	}
	if shipment.StartedToLoadingAt != nil {
		out.StartedAt = timestamppb.New(*shipment.StartedToLoadingAt)
	}

	return &businessv1.GetActiveShipmentByDriverResponse{Shipment: out, Found: true}, nil
}

// ReportGeofenceEvent records that a tracked truck crossed a warehouse
// boundary. It is evidence, not a command: it does not advance the shipment.
func (s *Server) ReportGeofenceEvent(ctx context.Context, req *businessv1.ReportGeofenceEventRequest) (*businessv1.ReportGeofenceEventResponse, error) {
	shipmentID, err := uuid.Parse(req.GetShipmentId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "malformed shipment id")
	}
	if req.GetTransition() == businessv1.GeofenceTransition_GEOFENCE_TRANSITION_UNSPECIFIED {
		return nil, status.Error(codes.InvalidArgument, "transition is required")
	}

	occurredAt := req.GetOccurredAt().AsTime()
	entering := req.GetTransition() == businessv1.GeofenceTransition_GEOFENCE_TRANSITION_ENTER

	newStatus, err := s.shipments.ReportGeofenceEvent(ctx, shipmentID, req.GetWarehouseId(), entering, occurredAt)
	if err != nil {
		return nil, mapError(err, "shipment")
	}

	return &businessv1.ReportGeofenceEventResponse{
		Accepted:      true,
		NewStatusCode: newStatus,
	}, nil
}

// ---------------------------------------------------------------------------
// Mapping
// ---------------------------------------------------------------------------

func toProtoOrder(o *models.Order) *businessv1.Order {
	if o == nil {
		return nil
	}

	out := &businessv1.Order{
		Id:               o.ID.String(),
		OrderNumber:      o.OrderNumber,
		ShipperCompanyId: o.ShipperCompanyID.String(),
		StatusCode:       o.StatusCode,
		Status:           models.StatusLabel(models.DomainOrder, o.StatusCode),
		StatusAlias:      models.StatusAlias(models.DomainOrder, o.StatusCode),
		Detail:           toStruct(o.Detail),
		Deleted:          o.DeletedAt.Valid,
		CreatedAt:        timestamppb.New(o.CreatedAt),
		UpdatedAt:        timestamppb.New(o.UpdatedAt),
	}

	if o.AgreementID != nil {
		out.AgreementId = o.AgreementID.String()
	}
	if o.TransporterCompanyID != nil {
		out.TransporterCompanyId = o.TransporterCompanyID.String()
	}
	if o.DriverUserID != nil {
		out.DriverId = o.DriverUserID.String()
	}
	if o.ParentOrderID != nil {
		out.ParentOrderId = o.ParentOrderID.String()
	}
	if o.TruckID != nil {
		out.TruckId = *o.TruckID
	}
	if o.OriginWarehouseID != nil {
		out.OriginWarehouseId = *o.OriginWarehouseID
	}
	if o.DestinationWarehouseID != nil {
		out.DestinationWarehouseId = *o.DestinationWarehouseID
	}
	if o.CurrencyID != nil {
		out.CurrencyId = *o.CurrencyID
	}
	if o.PickupAt != nil {
		out.PickupAt = timestamppb.New(*o.PickupAt)
	}
	if o.DeliveryAt != nil {
		out.DeliveryAt = timestamppb.New(*o.DeliveryAt)
	}
	if o.Price != nil {
		out.Price = mustFloat(*o.Price)
	}

	return out
}

func toStruct(m models.JSONB) *structpb.Struct {
	if len(m) == 0 {
		return nil
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

// mustFloat converts an exact decimal to the float64 the contract declares.
// Precision is preserved in the database; this is the display projection.
func mustFloat(d models.Money) float64 {
	f, _ := d.Float64()
	return f
}

func mapError(err error, subject string) error {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return status.Errorf(codes.NotFound, "%s not found", subject)
	case errors.Is(err, services.ErrValidation):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, services.ErrForbidden):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, services.ErrTransition), errors.Is(err, repository.ErrConflict):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Errorf(codes.Internal, "failed to load %s", subject)
	}
}
