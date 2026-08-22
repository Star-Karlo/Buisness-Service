// Package handlers exposes the business service over HTTP.
//
// The route table this serves has one handler per operation. That is the
// deliberate contrast with the monolith, where 46 routes shared one generic
// update handler that wrote whatever the request body contained.
package handlers

import (
	"errors"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/platform/authctx"
	"github.com/karlo/business-service/internal/platform/query"
	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/repository"
	"github.com/karlo/business-service/internal/services"
	"github.com/shopspring/decimal"
)

// OrderHandler serves the order endpoints.
type OrderHandler struct {
	orders *services.OrderService
}

func NewOrderHandler(orders *services.OrderService) *OrderHandler {
	return &OrderHandler{orders: orders}
}

type createOrderRequest struct {
	AgreementID            string           `json:"agreementId"`
	TransporterCompanyID   string           `json:"transporterCompanyId"`
	OrderKind              string           `json:"orderKind"`
	OriginWarehouseID      string           `json:"originWarehouseId"`
	DestinationWarehouseID string           `json:"destinationWarehouseId"`
	CargoTypeID            string           `json:"cargoTypeId"`
	ItemTypeID             string           `json:"itemTypeId"`
	Quantity               *decimal.Decimal `json:"quantity"`
	WeightKg               *decimal.Decimal `json:"weightKg"`
	VolumeM3               *decimal.Decimal `json:"volumeM3"`
	PickupAt               *time.Time       `json:"pickupAt"`
	DeliveryAt             *time.Time       `json:"deliveryAt"`
	CustomerID             string           `json:"customerId"`
	ReferenceNumber        string           `json:"referenceNumber"`
	Detail                 map[string]any   `json:"detail"`
	Submit                 bool             `json:"submit"`
}

// Create places an order.
//
// @Summary  Create order
// @Tags     Orders
// @Security BearerAuth
// @Success  201 {object} models.Order
// @Router   /orders [post]
func (h *OrderHandler) Create(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}

	var req createOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	in := services.CreateOrderInput{
		OrderKind:              req.OrderKind,
		OriginWarehouseID:      req.OriginWarehouseID,
		DestinationWarehouseID: req.DestinationWarehouseID,
		CargoTypeID:            req.CargoTypeID,
		ItemTypeID:             req.ItemTypeID,
		Quantity:               req.Quantity,
		WeightKg:               req.WeightKg,
		VolumeM3:               req.VolumeM3,
		PickupAt:               req.PickupAt,
		DeliveryAt:             req.DeliveryAt,
		CustomerID:             req.CustomerID,
		ReferenceNumber:        req.ReferenceNumber,
		Detail:                 req.Detail,
		SubmitImmediately:      req.Submit,
	}

	if req.AgreementID != "" {
		id, err := uuid.Parse(req.AgreementID)
		if err != nil {
			response.BadRequest(c, "Invalid agreementId")
			return
		}
		in.AgreementID = &id
	}
	if req.TransporterCompanyID != "" {
		id, err := uuid.Parse(req.TransporterCompanyID)
		if err != nil {
			response.BadRequest(c, "Invalid transporterCompanyId")
			return
		}
		in.TransporterCompanyID = &id
	}

	order, err := h.orders.Create(c.Request.Context(), actor, in)
	if err != nil {
		writeError(c, err)
		return
	}

	response.Created(c, order)
}

// List pages orders.
//
// @Summary  List orders
// @Tags     Orders
// @Security BearerAuth
// @Success  200 {object} response.Meta
// @Router   /orders [get]
func (h *OrderHandler) List(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}

	params := parseQuery(c, repository.OrderFields())

	orders, total, err := h.orders.List(c.Request.Context(), actor, params)
	if err != nil {
		writeError(c, err)
		return
	}

	response.Paginated(c, orders, meta(params, total))
}

// Get resolves one order.
//
// @Summary  Get order
// @Tags     Orders
// @Security BearerAuth
// @Param    id path string true "Order ID"
// @Success  200 {object} models.Order
// @Router   /orders/{id} [get]
func (h *OrderHandler) Get(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id")
	if !ok {
		return
	}

	order, err := h.orders.Get(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, order)
}

// UpdateDraft edits an unsubmitted order.
//
// @Summary  Update a draft order
// @Tags     Orders
// @Security BearerAuth
// @Param    id path string true "Order ID"
// @Success  200 {object} object
// @Router   /orders/{id} [put]
func (h *OrderHandler) UpdateDraft(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id")
	if !ok {
		return
	}

	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	if err := h.orders.UpdateDraft(c.Request.Context(), actor, id, body); err != nil {
		writeError(c, err)
		return
	}

	response.OKWithMessage(c, "Order updated", nil)
}

// Transition moves an order to a new state.
//
// One endpoint replaces the ~46 legacy status routes. The state machine decides
// what is legal, so a client asks for a destination rather than being trusted
// to write a status string.
//
// @Summary  Change order status
// @Tags     Orders
// @Security BearerAuth
// @Param    id path string true "Order ID"
// @Success  200 {object} models.Order
// @Router   /orders/{id}/status [put]
func (h *OrderHandler) Transition(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id")
	if !ok {
		return
	}

	var body struct {
		Status string `json:"status" binding:"required"`
		Note   string `json:"note"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	order, err := h.orders.Transition(c.Request.Context(), actor, id, body.Status, body.Note)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, order)
}

// AssignDriver puts a driver and truck on an order.
//
// @Summary  Assign a driver
// @Tags     Orders
// @Security BearerAuth
// @Param    id path string true "Order ID"
// @Success  200 {object} models.Order
// @Router   /orders/{id}/assign [put]
func (h *OrderHandler) AssignDriver(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id")
	if !ok {
		return
	}

	var body struct {
		DriverID string `json:"driverId" binding:"required"`
		TruckID  string `json:"truckId" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	driverID, err := uuid.Parse(body.DriverID)
	if err != nil {
		response.BadRequest(c, "Invalid driverId")
		return
	}

	order, err := h.orders.AssignDriver(c.Request.Context(), actor, id, driverID, body.TruckID)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, order)
}

// History returns an order's status timeline.
//
// @Summary  Order status history
// @Tags     Orders
// @Security BearerAuth
// @Param    id path string true "Order ID"
// @Success  200 {object} object
// @Router   /orders/{id}/history [get]
func (h *OrderHandler) History(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id")
	if !ok {
		return
	}

	entries, err := h.orders.History(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, entries)
}

// NextStates tells a client which transitions are available, so it can render
// only the buttons that will work.
//
// @Summary  Available order transitions
// @Tags     Orders
// @Security BearerAuth
// @Param    id path string true "Order ID"
// @Success  200 {object} object
// @Router   /orders/{id}/transitions [get]
func (h *OrderHandler) NextStates(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id")
	if !ok {
		return
	}

	order, err := h.orders.Get(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}

	next := models.NextOrderStates(order.StatusCode, actor.Role)
	out := make([]gin.H, 0, len(next))
	for _, s := range next {
		out = append(out, gin.H{
			"status": s,
			"label":  models.StatusLabel(models.DomainOrder, s),
			"alias":  models.StatusAlias(models.DomainOrder, s),
		})
	}

	response.OK(c, gin.H{"current": order.StatusCode, "transitions": out})
}

// Summary returns the dashboard counts.
//
// @Summary  Order dashboard summary
// @Tags     Orders
// @Security BearerAuth
// @Success  200 {object} object
// @Router   /orders/summary [get]
func (h *OrderHandler) Summary(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}

	counts, err := h.orders.Summary(c.Request.Context(), actor)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, counts)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// callerActor builds the Actor from the verified token. Company and user
// identity come from the token and never from the request.
func callerActor(c *gin.Context) (services.Actor, bool) {
	principal, ok := authctx.Gin(c)
	if !ok {
		response.Unauthorized(c, "No token provided.")
		return services.Actor{}, false
	}

	userID, err := uuid.Parse(principal.UserID)
	if err != nil {
		response.Unauthorized(c, "Invalid principal")
		return services.Actor{}, false
	}

	companyID, err := uuid.Parse(principal.CompanyID)
	if err != nil {
		response.Forbidden(c, "This account is not attached to a company")
		return services.Actor{}, false
	}

	return services.Actor{UserID: userID, CompanyID: companyID, Role: principal.Role()}, true
}

func pathUUID(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		response.BadRequest(c, "Invalid "+name)
		return uuid.Nil, false
	}
	return id, true
}

func parseQuery(c *gin.Context, fields query.FieldSet) query.Params {
	return query.Parse(
		c.DefaultQuery("page", "0"),
		c.DefaultQuery("pageSize", "20"),
		c.Query("filtered"),
		c.Query("sorted"),
		c.Query("search"),
		fields,
	)
}

func meta(p query.Params, total int64) *response.Meta {
	return &response.Meta{
		Page:       p.Page,
		Limit:      p.PageSize,
		TotalRows:  total,
		TotalPages: p.TotalPages(total),
	}
}

// writeError maps service errors onto status codes.
//
// A refused state change is 409, not 400: the request was well formed, but the
// resource is not in a state that permits it, and a client should retry after
// refreshing rather than reformatting.
func writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, repository.ErrNotFound):
		response.NotFound(c, "Not found")
	case errors.Is(err, services.ErrForbidden):
		response.Forbidden(c, err.Error())
	case errors.Is(err, services.ErrTransition), errors.Is(err, repository.ErrConflict):
		response.Conflict(c, err.Error())
	case errors.Is(err, services.ErrValidation):
		response.BadRequest(c, err.Error())
	default:
		response.InternalError(c, "Request failed")
	}
}
