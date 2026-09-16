// Package handlers exposes the business service over HTTP.
//
// The route table this serves has one handler per operation. That is the
// deliberate contrast with the monolith, where 46 routes shared one generic
// update handler that wrote whatever the request body contained.
package handlers

import (
	"errors"
	"net/http"
	"strings"
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
	AgreementID          string `json:"agreementId"`
	TransporterCompanyID string `json:"transporterCompanyId"`
	// A transporter entering the order for its customer names the customer
	// here and is the transporter itself.
	CustomerCompanyID      string             `json:"customerCompanyId"`
	OrderKind              string             `json:"orderKind"`
	OriginWarehouseID      string             `json:"originWarehouseId"`
	DestinationWarehouseID string             `json:"destinationWarehouseId"`
	CargoTypeID            string             `json:"cargoTypeId"`
	ItemTypeID             string             `json:"itemTypeId"`
	Quantity               *decimal.Decimal   `json:"quantity"`
	WeightKg               *decimal.Decimal   `json:"weightKg"`
	VolumeM3               *decimal.Decimal   `json:"volumeM3"`
	PickupAt               *time.Time         `json:"pickupAt"`
	DeliveryAt             *time.Time         `json:"deliveryAt"`
	ExpiresAt              *time.Time         `json:"expiresAt"`
	CustomerID             string             `json:"customerId"`
	ReferenceNumber        string             `json:"referenceNumber"`
	Detail                 map[string]any     `json:"detail"`
	Items                  []orderItemRequest `json:"items"`
	Submit                 bool               `json:"submit"`
}

// orderItemRequest is one line of itemised cargo, for the companies whose
// configuration enables it.
//
// Volume is deliberately absent: it is derived from the dimensions by the
// service and stored, so a client cannot submit a volume that disagrees with
// its own measurements.
type orderItemRequest struct {
	CatalogItemID string           `json:"catalogItemId"`
	Name          string           `json:"name"`
	Quantity      *decimal.Decimal `json:"quantity"`
	Unit          string           `json:"unit"`
	Packaging     string           `json:"packaging"`
	WeightKg      *decimal.Decimal `json:"weightKg"`
	LengthCm      *decimal.Decimal `json:"lengthCm"`
	WidthCm       *decimal.Decimal `json:"widthCm"`
	HeightCm      *decimal.Decimal `json:"heightCm"`
	HandlingNotes string           `json:"handlingNotes"`
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

	// Read the raw body before binding, to record which keys were actually
	// supplied. Binding alone cannot answer that: an omitted quantity and a
	// quantity of zero both leave the field at its zero value, and the
	// per-company field check needs to tell them apart.
	present := presentKeys(buffered(c.Request))

	var req createOrderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	// The wizard names the customer as a company; the field configuration's
	// key is customerId. Same fold as agreements.
	if req.CustomerCompanyID != "" {
		present["customerId"] = true
	}

	items := make([]services.OrderItemInput, 0, len(req.Items))
	for _, item := range req.Items {
		items = append(items, services.OrderItemInput{
			CatalogItemID: item.CatalogItemID,
			Name:          item.Name,
			Quantity:      item.Quantity,
			Unit:          item.Unit,
			Packaging:     item.Packaging,
			WeightKg:      item.WeightKg,
			LengthCm:      item.LengthCm,
			WidthCm:       item.WidthCm,
			HeightCm:      item.HeightCm,
			HandlingNotes: item.HandlingNotes,
		})
	}

	in := services.CreateOrderInput{
		Present:                present,
		Items:                  items,
		ExpiresAt:              req.ExpiresAt,
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
		CustomerCompanyID:      parseOptionalUUID(req.CustomerCompanyID),
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
	if badQuery(c, params) {
		return
	}

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

	if !allowStatusChange(c, body.Status, models.PermissionForOrderStatus) {
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

	// driverId is the master-data driver id (24 hex characters). A login
	// user id is not accepted here: the driver record is what a truck is
	// paired with and what carries the phone the driver is reached on.
	driverID := strings.TrimSpace(body.DriverID)
	if len(driverID) != 24 {
		response.BadRequest(c, "Invalid driverId: expected a master-data driver id")
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

	// The state machine says which moves are legal from here for this role.
	// That is not the same question the caller is asking.
	next := models.NextOrderStates(order.StatusCode, actor.Role)

	// Clients render action buttons directly from this list, so anything left
	// in it that the caller cannot actually perform becomes a button that
	// fails on click. A member granted order.read and order.create but not
	// order.cancel was being offered Cancel, and got a 403 when they used it —
	// which reads as a broken system rather than as a permission they were
	// never given.
	//
	// Filtered against the SAME map the transition itself enforces, so the two
	// answers cannot drift: if a status is unreachable here it is refused
	// there, and the reverse.
	out := make([]gin.H, 0, len(next))
	for _, s := range next {
		if !callerMayReach(c, s) {
			continue
		}
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

	actor := services.Actor{
		UserID:        userID,
		CompanyID:     companyID,
		Role:          principal.Role(),
		PlatformStaff: principal.IsPlatformStaff,
	}

	// Acting for a client.
	//
	// A header rather than a field on every payload, because "who am I acting
	// as" is the same question on a create, an edit and a read — putting it in
	// the body would mean adding it to a dozen request shapes and forgetting it
	// on the reads.
	if target := strings.TrimSpace(c.GetHeader(actingForHeader)); target != "" {
		if !actor.PlatformStaff {
			// Not a mistake to be tolerated: a company naming another would be
			// reading and writing that company's data.
			response.Forbidden(c, "Only Karlo staff may act on another company's behalf.")
			return services.Actor{}, false
		}
		onBehalfOf, err := uuid.Parse(target)
		if err != nil {
			response.BadRequest(c, "Invalid "+actingForHeader+" header")
			return services.Actor{}, false
		}
		if onBehalfOf != companyID {
			actor.CompanyID = onBehalfOf
			actor.ActingFor = true
		}
	}

	return actor, true
}

// actingForHeader names the company a Karlo staff member is acting for.
//
// Its absence means "myself", which is what every ordinary request sends.
const actingForHeader = "X-Acting-For"

// callerMayReach reports whether this caller holds the permission a status
// change requires, without writing anything.
//
// It shares its lookup with allowStatusChange deliberately. Offering a move and
// then refusing it are the same decision asked at two moments, and answering
// them from two different places is how a UI ends up advertising actions the
// server rejects.
func callerMayReach(c *gin.Context, status string) bool {
	key, known := models.PermissionForOrderStatus(status)
	if !known {
		return false
	}
	principal, ok := authctx.Gin(c)
	if !ok {
		return false
	}
	module, action, found := strings.Cut(key, ".")
	return found && principal.HasModule(module, action)
}

// allowStatusChange refuses a status change the caller has no permission for.
//
// The route guards cannot do this. A status endpoint takes its target from the
// request body, so one URL covers transitions as different as submitting an
// order and cancelling one, and RequireModule is fixed at registration time.
// The check therefore has to happen here, once the body is parsed.
//
// lookup is models.PermissionForOrderStatus or its shipment counterpart. An
// unmapped target status is refused, so adding a status without deciding who
// may reach it fails closed rather than open.
func allowStatusChange(c *gin.Context, to string, lookup func(string) (string, bool)) bool {
	key, known := lookup(to)
	if !known {
		response.BadRequest(c, "Unknown status: "+to)
		return false
	}

	principal, ok := authctx.Gin(c)
	if !ok {
		response.Unauthorized(c, "No token provided.")
		return false
	}

	module, action, found := strings.Cut(key, ".")
	if !found || !principal.HasModule(module, action) {
		response.Forbidden(c, "Access Denied")
		return false
	}
	return true
}

func pathUUID(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		response.BadRequest(c, "Invalid "+name)
		return uuid.Nil, false
	}
	return id, true
}

// parseQuery normalises a listing request.
//
// The caller must check params.Err before using the result — badQuery does
// that and answers 400. A filter the server does not understand must never be
// silently dropped: the response would be 200 with real rows and only the
// count wrong, which is far harder to notice than an error.
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
	// 402 rather than 403: the company has not bought this, which a sales
	// conversation fixes. A 403 would send the user to their administrator,
	// who has nothing to grant.
	case errors.Is(err, services.ErrNotEntitled):
		c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{
			"success": false, "message": err.Error(),
		})
	default:
		response.InternalError(c, "Request failed")
	}
}

// badQuery answers 400 when a listing request could not be understood, and
// reports whether the handler should stop.
func badQuery(c *gin.Context, p query.Params) bool {
	if p.Err == nil {
		return false
	}
	response.BadRequest(c, p.Err.Error())
	return true
}

// parseOptionalUUID returns nil for an empty or malformed id; callers that
// need to reject a malformed one validate separately.
func parseOptionalUUID(v string) *uuid.UUID {
	if v == "" {
		return nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil
	}
	return &id
}

// PatchDetail merges working data into an order's detail.
//
// @Summary  Patch order detail
// @Tags     Orders
// @Security BearerAuth
// @Param    id path string true "Order ID"
// @Router   /orders/{id}/detail [patch]
func (h *OrderHandler) PatchDetail(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id")
	if !ok {
		return
	}
	var patch map[string]interface{}
	if err := c.ShouldBindJSON(&patch); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	order, err := h.orders.PatchDetail(c.Request.Context(), actor, id, patch)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, order)
}
