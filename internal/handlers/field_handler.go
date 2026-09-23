package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/services"
)

// FieldHandler serves Web-Field: the receiving PIC's own screens.
//
// Five of its seven endpoints are unauthenticated, like the public tracking
// page. What stands in for an account is the code the driver holds: the order
// number gets you a lookup, the code gets you a session token, and the token
// is what every later call carries. A signed-in PIC gets two more — the inbox
// and an attributed verify — which save them typing an order number and buy
// them nothing else: opening a delivery still needs the driver's code.
//
// They are registered under /shipments/ because that is the prefix the load
// balancer routes here; see the group in internal/routes/routes.go.
type FieldHandler struct {
	handovers *services.HandoverService
}

func NewFieldHandler(h *services.HandoverService) *FieldHandler {
	return &FieldHandler{handovers: h}
}

type fieldLookupRequest struct {
	OrderNumber string `json:"orderNumber" binding:"required"`
}

// Lookup resolves an order number typed (or scanned) on the landing page.
//
// @Summary  Look up an order for Web-Field (public)
// @Tags     Web-Field
// @Success  200 {object} services.FieldLookup
// @Router   /shipments/field/lookup [post]
func (h *FieldHandler) Lookup(c *gin.Context) {
	var req fieldLookupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Masukkan nomor order yang tertera pada surat jalan.")
		return
	}
	out, err := h.handovers.FieldLookup(c.Request.Context(), req.OrderNumber)
	if err != nil {
		// The generic mapping answers "Not found", in English, to somebody
		// standing at a gate holding a piece of paper. Say what to do instead
		// — and say the same thing whether the order is unknown or simply not
		// at an unloading point, which is what keeps the endpoint from
		// answering questions about other people's orders.
		response.NotFound(c, "Nomor order tidak ditemukan, atau truk belum sampai di titik bongkar.")
		return
	}
	response.OK(c, out)
}

type fieldVerifyRequest struct {
	OrderNumber string `json:"orderNumber" binding:"required"`
	Code        string `json:"code" binding:"required"`
}

// Verify exchanges the driver's code for a session token.
//
// @Summary  Verify the driver's code (public)
// @Tags     Web-Field
// @Success  200 {object} response.Envelope
// @Router   /shipments/field/verify [post]
func (h *FieldHandler) Verify(c *gin.Context) {
	var req fieldVerifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// Not the binder's own wording: "Field validation for 'Code' failed on
		// the 'required' tag" is a sentence for a developer, and this endpoint
		// answers a warehouse.
		response.BadRequest(c, "Masukkan nomor order dan 6 digit Kode OTP dari driver.")
		return
	}

	// A signed-in PIC is recorded as the one who verified; a walk-up PIC is
	// recorded by the name they type on the audit screen.
	var picUserID *uuid.UUID
	if actor, ok := optionalActor(c); ok {
		id := actor.UserID
		picUserID = &id
	}

	token, err := h.handovers.FieldSignIn(c.Request.Context(), req.OrderNumber, req.Code, picUserID)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, gin.H{"token": token})
}

// Session returns the order sheet behind a token.
//
// @Summary  Read a Web-Field session (public, by token)
// @Tags     Web-Field
// @Success  200 {object} services.FieldView
// @Router   /shipments/field/session/{token} [get]
func (h *FieldHandler) Session(c *gin.Context) {
	token, ok := fieldToken(c)
	if !ok {
		return
	}
	view, err := h.handovers.Field(c.Request.Context(), token)
	if err != nil {
		response.NotFound(c, "Sesi tidak ditemukan")
		return
	}
	response.OK(c, view)
}

type fieldAuditRequest struct {
	Matches  *bool    `json:"matches" binding:"required"`
	Note     string   `json:"note"`
	PICName  string   `json:"picName"`
	WeightKg *float64 `json:"weightKg"`
	VolumeM3 *float64 `json:"volumeM3"`
	Quantity *float64 `json:"quantity"`
}

// Audit records what the PIC counted.
//
// @Summary  Record the PIC's audit (public, by token)
// @Tags     Web-Field
// @Success  200 {object} services.FieldView
// @Router   /shipments/field/session/{token}/audit [post]
func (h *FieldHandler) Audit(c *gin.Context) {
	token, ok := fieldToken(c)
	if !ok {
		return
	}
	var req fieldAuditRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Pilih hasil audit: data sesuai atau tidak sesuai.")
		return
	}
	view, err := h.handovers.RecordFieldAudit(c.Request.Context(), token, services.FieldAuditIn{
		Matches: *req.Matches, Note: req.Note, PICName: req.PICName,
		WeightKg: req.WeightKg, VolumeM3: req.VolumeM3, Quantity: req.Quantity,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, view)
}

type fieldFinalizeRequest struct {
	PICName string `json:"picName"`
	Note    string `json:"note"`
}

// Finalize closes the manifest. One-way from this page.
//
// @Summary  Finalise the manifest (public, by token)
// @Tags     Web-Field
// @Success  200 {object} services.FieldView
// @Router   /shipments/field/session/{token}/finalize [post]
func (h *FieldHandler) Finalize(c *gin.Context) {
	token, ok := fieldToken(c)
	if !ok {
		return
	}
	var req fieldFinalizeRequest
	_ = c.ShouldBindJSON(&req)
	view, err := h.handovers.FinalizeField(c.Request.Context(), token, services.FieldFinalizeIn{
		PICName: req.PICName, Note: req.Note,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, view)
}

// Inbox lists the deliveries arriving at the signed-in PIC's own sites.
//
// @Summary  Web-Field inbox
// @Tags     Web-Field
// @Security BearerAuth
// @Success  200 {array} services.FieldInboxRow
// @Router   /shipments/field/inbox [get]
func (h *FieldHandler) Inbox(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	rows, err := h.handovers.FieldInbox(c.Request.Context(), actor)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, rows)
}

// fieldToken reads the token out of the path, rejecting anything that is not
// the shape the issuer produces before it reaches the database.
func fieldToken(c *gin.Context) (string, bool) {
	token := c.Param("token")
	if len(token) < 16 || len(token) > 128 {
		response.NotFound(c, "Sesi tidak ditemukan")
		return "", false
	}
	return token, true
}
