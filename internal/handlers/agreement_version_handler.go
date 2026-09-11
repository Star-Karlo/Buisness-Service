package handlers

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/services"
)

type reviseAgreementRequest struct {
	// Kind decides whether an approval is needed: "renewal" takes effect at
	// once, "update" waits on a Sales Manager.
	Kind string `json:"kind" binding:"required"`
	Note string `json:"note"`

	ValidFrom  *time.Time `json:"validFrom"`
	ValidUntil *time.Time `json:"validUntil"`

	PaymentTypeID string         `json:"paymentTypeId"`
	CurrencyID    string         `json:"currencyId"`
	Detail        map[string]any `json:"detail"`

	// Rates replaces the price matrix wholesale. Omitted entirely, the
	// predecessor's rates carry forward — a renewal at the same prices is the
	// common case, and retyping a priced matrix invites transcription errors.
	Rates []rateRequest `json:"rates"`
}

// Revise proposes a new version of an agreement.
//
// @Summary  Revise an agreement (renewal or update)
// @Tags     Agreements
// @Security BearerAuth
// @Success  201 {object} models.Agreement
// @Router   /agreements/{id}/revisions [post]
func (h *BillingHandler) Revise(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid agreement id")
		return
	}

	present := presentKeys(buffered(c.Request))

	var req reviseAgreementRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	foldRateKeys(present, req.Rates)

	in := services.ReviseAgreementInput{
		Kind:          req.Kind,
		Note:          req.Note,
		ValidFrom:     req.ValidFrom,
		ValidUntil:    req.ValidUntil,
		PaymentTypeID: req.PaymentTypeID,
		CurrencyID:    req.CurrencyID,
		Detail:        req.Detail,
		Present:       present,
	}
	for _, r := range req.Rates {
		in.Rates = append(in.Rates, services.RateInput{
			OriginCityID:          r.OriginCityID,
			DestinationCityID:     r.DestinationCityID,
			OriginDistrictID:      r.OriginDistrictID,
			DestinationDistrictID: r.DestinationDistrictID,
			TruckTypeID:           r.TruckTypeID,
			PricingTypeID:         r.PricingTypeID,
			Price:                 r.Price,
			MinQuantity:           r.MinQuantity,
			LeadTimeHours:         r.LeadTimeHours,
		})
	}

	revision, err := h.billing.Revise(c.Request.Context(), actor, id, in)
	if err != nil {
		writeError(c, err)
		return
	}
	response.Created(c, revision)
}

type approvalDecisionRequest struct {
	// Approve false rejects. One endpoint rather than two, matching how order
	// and agreement decisions already work here.
	Approve bool   `json:"approve"`
	Note    string `json:"note"`
}

// DecideRevision approves or rejects a pending version.
//
// @Summary  Approve or reject an agreement revision
// @Tags     Agreements
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /agreements/{id}/revisions/decision [put]
func (h *BillingHandler) DecideRevision(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid agreement id")
		return
	}

	var req approvalDecisionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	if !req.Approve {
		if err := h.billing.Reject(c.Request.Context(), actor, id, req.Note); err != nil {
			writeError(c, err)
			return
		}
		response.OKWithMessage(c, "Revision rejected.", nil)
		return
	}

	approved, err := h.billing.Approve(c.Request.Context(), actor, id, req.Note)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, approved)
}

// Lineage returns every version of a contract, newest first.
//
// @Summary  Agreement version history
// @Tags     Agreements
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /agreements/{id}/versions [get]
func (h *BillingHandler) Lineage(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid agreement id")
		return
	}

	versions, err := h.billing.Lineage(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, versions)
}

// PriceHistory answers the PRD's "view price history".
//
// @Summary  Agreement price history
// @Tags     Agreements
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /agreements/{id}/price-history [get]
func (h *BillingHandler) PriceHistory(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid agreement id")
		return
	}

	lines, err := h.billing.PriceHistory(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, lines)
}

// PendingApprovals is the approver's queue.
//
// @Summary  Agreement revisions awaiting a decision
// @Tags     Agreements
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /agreements/pending-approval [get]
func (h *BillingHandler) PendingApprovals(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}

	pending, err := h.billing.PendingApprovals(c.Request.Context(), actor)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, pending)
}
