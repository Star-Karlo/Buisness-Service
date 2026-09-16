package handlers

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/repository"
	"github.com/karlo/business-service/internal/services"
	"github.com/shopspring/decimal"
)

// BillingHandler serves agreements and invoices.
type BillingHandler struct {
	billing *services.BillingService
}

func NewBillingHandler(billing *services.BillingService) *BillingHandler {
	return &BillingHandler{billing: billing}
}

type customerRequest struct {
	CompanyID string `json:"companyId" binding:"required"`
	Label     string `json:"label"`
}

type rateRequest struct {
	// CustomerCompanyID narrows this lane to one client. Omitted, the lane
	// prices for every customer the agreement covers.
	CustomerCompanyID string `json:"customerCompanyId"`

	// Warehouse ids make this a warehouse-level lane, which is the only kind
	// that can be routed — a city pair has no coordinates to measure between.
	OriginWarehouseID      string `json:"originWarehouseId"`
	DestinationWarehouseID string `json:"destinationWarehouseId"`

	OriginCityID      string `json:"originCityId"`
	DestinationCityID string `json:"destinationCityId"`

	// Kecamatan, sent only by companies that price lanes below city level.
	// A company whose form configuration hides these must not send them: the
	// service refuses a hidden field carrying a value rather than dropping it.
	OriginDistrictID      string `json:"originDistrictId"`
	DestinationDistrictID string `json:"destinationDistrictId"`

	TruckTypeID   string           `json:"truckTypeId"`
	PricingTypeID string           `json:"pricingTypeId"`
	Price         decimal.Decimal  `json:"price"`
	MinQuantity   *decimal.Decimal `json:"minQuantity"`
	LeadTimeHours *int             `json:"leadTimeHours"`
}

// foldRateKeys lifts per-rate fields to the catalogue paths the form declares.
//
// Mutates `present` rather than returning a second map, so there is one answer
// to "what did the caller supply" rather than two that can disagree.
func foldRateKeys(present map[string]bool, rates []rateRequest) {
	mark := func(key string, supplied bool) {
		if supplied {
			present[key] = true
		}
	}
	for _, r := range rates {
		mark("customerId", r.CustomerCompanyID != "")
		mark("route.originCityId", r.OriginCityID != "")
		mark("route.destinationCityId", r.DestinationCityID != "")
		mark("route.originDistrictId", r.OriginDistrictID != "")
		mark("route.destinationDistrictId", r.DestinationDistrictID != "")
		mark("truckTypeId", r.TruckTypeID != "")
		mark("pricingTypeId", r.PricingTypeID != "")
		// Price is the one field where zero is a real value — a free leg on a
		// backhaul — so presence is the rate row existing, not the number
		// being non-zero.
		mark("price", true)
		mark("minQuantity", r.MinQuantity != nil)
	}
}

type createAgreementRequest struct {
	// One of the two: a shipper names the transporter; a transporter names
	// the customer (its shipper client) and is the transporter itself.
	TransporterCompanyID string            `json:"transporterCompanyId"`
	CustomerCompanyID    string            `json:"customerCompanyId"`
	ValidFrom            time.Time         `json:"validFrom" binding:"required"`
	ValidUntil           time.Time         `json:"validUntil" binding:"required"`
	PaymentTypeID        string            `json:"paymentTypeId"`
	CurrencyID           string            `json:"currencyId"`
	Detail               map[string]any    `json:"detail"`
	Rates                []rateRequest     `json:"rates" binding:"required,min=1"`
	Customers            []customerRequest `json:"customers"`
}

// CreateAgreement records a contract.
//
// @Summary  Create agreement
// @Tags     Agreements
// @Security BearerAuth
// @Success  201 {object} models.Agreement
// @Router   /agreements [post]
func (h *BillingHandler) CreateAgreement(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}

	// See OrderHandler.Create for why the raw keys are read before binding.
	present := presentKeys(buffered(c.Request))

	var req createAgreementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	var transporterID uuid.UUID
	var customerID *uuid.UUID
	switch {
	case req.CustomerCompanyID != "":
		id, err := uuid.Parse(req.CustomerCompanyID)
		if err != nil {
			response.BadRequest(c, "Invalid customerCompanyId")
			return
		}
		customerID = &id
		transporterID = actor.CompanyID
		present["customerId"] = true
	case req.TransporterCompanyID != "":
		id, err := uuid.Parse(req.TransporterCompanyID)
		if err != nil {
			response.BadRequest(c, "Invalid transporterCompanyId")
			return
		}
		transporterID = id
	default:
		response.BadRequest(c, "transporterCompanyId or customerCompanyId is required")
		return
	}

	// The field catalogue describes the FORM, whose route and pricing inputs
	// live on rate rows; the payload nests them in an array. A generic walk of
	// the body cannot bridge that, so the rate-level keys are folded up here:
	// a field counts as supplied when any rate supplies it, which is the same
	// question the form is asking — "is this input in use".
	foldRateKeys(present, req.Rates)
	// The form asks for a customer once per lane and derives the covered
	// list from it, so the request carries `customers` and per-rate
	// `customerCompanyId`, never a top-level `customerId`. The field
	// configuration's key is `customerId`; fold the two spellings onto it,
	// or a required customer can never be satisfied by the form that
	// collects it.
	if len(req.Customers) > 0 {
		present["customerId"] = true
	}

	in := services.CreateAgreementInput{
		Present:              present,
		TransporterCompanyID: transporterID,
		CustomerCompanyID:    customerID,
		ValidFrom:            req.ValidFrom,
		ValidUntil:           req.ValidUntil,
		PaymentTypeID:        req.PaymentTypeID,
		CurrencyID:           req.CurrencyID,
		Detail:               req.Detail,
	}
	// The clients this agreement covers. `c` is the gin context here, so the
	// loop variable is named for what it holds rather than shadowing it.
	for _, customer := range req.Customers {
		id, cerr := uuid.Parse(customer.CompanyID)
		if cerr != nil {
			response.BadRequest(c, "Invalid customer companyId: "+customer.CompanyID)
			return
		}
		in.Customers = append(in.Customers, services.CustomerInput{
			CompanyID: id, Label: customer.Label,
		})
	}

	for _, r := range req.Rates {
		rate := services.RateInput{
			OriginWarehouseID:      r.OriginWarehouseID,
			DestinationWarehouseID: r.DestinationWarehouseID,
			OriginCityID:           r.OriginCityID,
			DestinationCityID:      r.DestinationCityID,
			OriginDistrictID:       r.OriginDistrictID,
			DestinationDistrictID:  r.DestinationDistrictID,
			TruckTypeID:            r.TruckTypeID,
			PricingTypeID:          r.PricingTypeID,
			Price:                  r.Price,
			MinQuantity:            r.MinQuantity,
			LeadTimeHours:          r.LeadTimeHours,
		}
		if r.CustomerCompanyID != "" {
			id, cerr := uuid.Parse(r.CustomerCompanyID)
			if cerr != nil {
				response.BadRequest(c, "Invalid lane customerCompanyId: "+r.CustomerCompanyID)
				return
			}
			rate.CustomerCompanyID = &id
		}
		in.Rates = append(in.Rates, rate)
	}

	agreement, err := h.billing.CreateAgreement(c.Request.Context(), actor, in)
	if err != nil {
		writeError(c, err)
		return
	}

	response.Created(c, agreement)
}

// ListAgreements pages contracts.
//
// @Summary  List agreements
// @Tags     Agreements
// @Security BearerAuth
// @Success  200 {object} response.Meta
// @Router   /agreements [get]
func (h *BillingHandler) ListAgreements(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}

	params := parseQuery(c, repository.AgreementFields())
	if badQuery(c, params) {
		return
	}

	items, total, err := h.billing.ListAgreements(c.Request.Context(), actor, params)
	if err != nil {
		writeError(c, err)
		return
	}

	response.Paginated(c, items, meta(params, total))
}

// GetAgreement resolves one contract.
//
// @Summary  Get agreement
// @Tags     Agreements
// @Security BearerAuth
// @Param    id path string true "Agreement ID"
// @Success  200 {object} models.Agreement
// @Router   /agreements/{id} [get]
func (h *BillingHandler) GetAgreement(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}

	agreement, err := h.billing.GetAgreement(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, agreement)
}

// DecideAgreement approves or rejects a submitted contract.
//
// @Summary  Approve or reject an agreement
// @Tags     Agreements
// @Security BearerAuth
// @Param    id path string true "Agreement ID"
// @Success  200 {object} object
// @Router   /agreements/{id}/decision [put]
func (h *BillingHandler) DecideAgreement(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}

	var body struct {
		Approve bool `json:"approve"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	if err := h.billing.DecideAgreement(c.Request.Context(), actor, id, body.Approve); err != nil {
		writeError(c, err)
		return
	}

	response.OKWithMessage(c, "Agreement updated", nil)
}

// VerifyAgreement records the shipper's sign-off.
//
// @Summary  Verify an agreement
// @Tags     Agreements
// @Security BearerAuth
// @Param    id path string true "Agreement ID"
// @Success  200 {object} object
// @Router   /agreements/{id}/verify [put]
func (h *BillingHandler) VerifyAgreement(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}

	if err := h.billing.VerifyAgreement(c.Request.Context(), actor, id); err != nil {
		writeError(c, err)
		return
	}

	response.OKWithMessage(c, "Agreement verified", nil)
}

type createInvoiceRequest struct {
	ShipperCompanyID string          `json:"shipperCompanyId" binding:"required"`
	OrderIDs         []string        `json:"orderIds" binding:"required,min=1"`
	CurrencyID       string          `json:"currencyId"`
	Adjustment       decimal.Decimal `json:"adjustment"`
	Notes            string          `json:"notes"`
	DueAt            *time.Time      `json:"dueAt"`
}

// CreateInvoice bills a set of completed orders.
//
// @Summary  Create invoice
// @Tags     Invoices
// @Security BearerAuth
// @Success  201 {object} models.Invoice
// @Router   /invoices [post]
func (h *BillingHandler) CreateInvoice(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}

	var req createInvoiceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	shipperID, err := uuid.Parse(req.ShipperCompanyID)
	if err != nil {
		response.BadRequest(c, "Invalid shipperCompanyId")
		return
	}

	orderIDs := make([]uuid.UUID, 0, len(req.OrderIDs))
	for _, raw := range req.OrderIDs {
		id, perr := uuid.Parse(raw)
		if perr != nil {
			response.BadRequest(c, "Invalid order id: "+raw)
			return
		}
		orderIDs = append(orderIDs, id)
	}

	invoice, err := h.billing.CreateInvoice(c.Request.Context(), actor, services.CreateInvoiceInput{
		ShipperCompanyID: shipperID,
		OrderIDs:         orderIDs,
		CurrencyID:       req.CurrencyID,
		Adjustment:       req.Adjustment,
		Notes:            req.Notes,
		DueAt:            req.DueAt,
	})
	if err != nil {
		writeError(c, err)
		return
	}

	response.Created(c, invoice)
}

// ListInvoices pages bills.
//
// @Summary  List invoices
// @Tags     Invoices
// @Security BearerAuth
// @Success  200 {object} response.Meta
// @Router   /invoices [get]
func (h *BillingHandler) ListInvoices(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}

	params := parseQuery(c, repository.InvoiceFields())
	if badQuery(c, params) {
		return
	}

	items, total, err := h.billing.ListInvoices(c.Request.Context(), actor, params)
	if err != nil {
		writeError(c, err)
		return
	}

	response.Paginated(c, items, meta(params, total))
}

// GetInvoice resolves one bill.
//
// @Summary  Get invoice
// @Tags     Invoices
// @Security BearerAuth
// @Param    id path string true "Invoice ID"
// @Success  200 {object} models.Invoice
// @Router   /invoices/{id} [get]
func (h *BillingHandler) GetInvoice(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}

	invoice, err := h.billing.GetInvoice(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, invoice)
}

// TransitionInvoice advances a bill.
//
// This replaces the eleven legacy invoice-status routes, which all pointed at
// the same generic update handler and enforced nothing about who could mark an
// invoice paid.
//
// @Summary  Change invoice status
// @Tags     Invoices
// @Security BearerAuth
// @Param    id path string true "Invoice ID"
// @Success  200 {object} object
// @Router   /invoices/{id}/status [put]
func (h *BillingHandler) TransitionInvoice(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}

	var body struct {
		Status string `json:"status" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	// A status that is not an invoice status at all is a client mistake worth
	// naming precisely, rather than letting it reach the service and come back
	// as a generic refusal.
	if !isKnownStatus(models.DomainInvoice, body.Status) {
		response.BadRequest(c, "Unknown invoice status: "+body.Status)
		return
	}

	if err := h.billing.TransitionInvoice(c.Request.Context(), actor, id, body.Status); err != nil {
		writeError(c, err)
		return
	}

	response.OKWithMessage(c, "Invoice updated", nil)
}

// isKnownStatus reports whether a status code exists in a domain.
func isKnownStatus(domain models.Domain, status string) bool {
	for _, known := range models.KnownStatuses(domain) {
		if known == status {
			return true
		}
	}
	return false
}
