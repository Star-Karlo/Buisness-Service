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

type rateRequest struct {
	OriginCityID      string           `json:"originCityId"`
	DestinationCityID string           `json:"destinationCityId"`
	TruckTypeID       string           `json:"truckTypeId"`
	PricingTypeID     string           `json:"pricingTypeId"`
	Price             decimal.Decimal  `json:"price"`
	MinQuantity       *decimal.Decimal `json:"minQuantity"`
	LeadTimeHours     *int             `json:"leadTimeHours"`
}

type createAgreementRequest struct {
	TransporterCompanyID string         `json:"transporterCompanyId" binding:"required"`
	ValidFrom            time.Time      `json:"validFrom" binding:"required"`
	ValidUntil           time.Time      `json:"validUntil" binding:"required"`
	PaymentTypeID        string         `json:"paymentTypeId"`
	CurrencyID           string         `json:"currencyId"`
	Detail               map[string]any `json:"detail"`
	Rates                []rateRequest  `json:"rates" binding:"required,min=1"`
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

	var req createAgreementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	transporterID, err := uuid.Parse(req.TransporterCompanyID)
	if err != nil {
		response.BadRequest(c, "Invalid transporterCompanyId")
		return
	}

	in := services.CreateAgreementInput{
		TransporterCompanyID: transporterID,
		ValidFrom:            req.ValidFrom,
		ValidUntil:           req.ValidUntil,
		PaymentTypeID:        req.PaymentTypeID,
		CurrencyID:           req.CurrencyID,
		Detail:               req.Detail,
	}
	for _, r := range req.Rates {
		in.Rates = append(in.Rates, services.RateInput{
			OriginCityID:      r.OriginCityID,
			DestinationCityID: r.DestinationCityID,
			TruckTypeID:       r.TruckTypeID,
			PricingTypeID:     r.PricingTypeID,
			Price:             r.Price,
			MinQuantity:       r.MinQuantity,
			LeadTimeHours:     r.LeadTimeHours,
		})
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
	id, ok := pathUUID(c, "id")
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
	id, ok := pathUUID(c, "id")
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
	id, ok := pathUUID(c, "id")
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
	id, ok := pathUUID(c, "id")
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
	id, ok := pathUUID(c, "id")
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
