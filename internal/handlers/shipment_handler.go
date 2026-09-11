package handlers

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/platform/authctx"
	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/services"
)

// ShipmentHandler serves the driver and warehouse endpoints.
type ShipmentHandler struct {
	shipments *services.ShipmentService
}

func NewShipmentHandler(shipments *services.ShipmentService) *ShipmentHandler {
	return &ShipmentHandler{shipments: shipments}
}

type advanceRequest struct {
	Status    string   `json:"status" binding:"required"`
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
	Note      string   `json:"note"`
}

// Advance moves a shipment one step along its lifecycle.
//
// This single endpoint replaces the 22 legacy shipment routes, several of which
// pointed at the same handler and were therefore indistinguishable after the
// fact. The state machine enforces both the order of the steps and which role
// may take each one.
//
// @Summary  Advance shipment status
// @Tags     Shipments
// @Security BearerAuth
// @Param    id path string true "Shipment ID"
// @Success  200 {object} models.Shipment
// @Router   /shipments/{id}/status [put]
func (h *ShipmentHandler) Advance(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id")
	if !ok {
		return
	}

	var req advanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	if !allowStatusChange(c, req.Status, models.PermissionForShipmentStatus) {
		return
	}

	in := services.AdvanceInput{
		ShipmentID: id,
		To:         req.Status,
		Note:       req.Note,
	}
	if req.Latitude != nil && req.Longitude != nil {
		in.Position = &services.Position{Latitude: *req.Latitude, Longitude: *req.Longitude}
	}

	shipment, err := h.shipments.Advance(c.Request.Context(), actor, in)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, shipment)
}

// GetByOrder returns the shipment behind an order.
//
// @Summary  Get an order's shipment
// @Tags     Shipments
// @Security BearerAuth
// @Param    orderId path string true "Order ID"
// @Success  200 {object} models.Shipment
// @Router   /orders/{orderId}/shipment [get]
func (h *ShipmentHandler) GetByOrder(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	orderID, ok := pathUUID(c, "orderId")
	if !ok {
		return
	}

	shipment, err := h.shipments.GetByOrder(c.Request.Context(), actor, orderID)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, shipment)
}

// NextStates lists the steps available to this caller right now.
//
// @Summary  Available shipment transitions
// @Tags     Shipments
// @Security BearerAuth
// @Param    id path string true "Shipment ID"
// @Success  200 {object} object
// @Router   /shipments/{id}/transitions [get]
func (h *ShipmentHandler) NextStates(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id")
	if !ok {
		return
	}

	// Reading through the order enforces the same tenant check the advance path
	// applies, rather than exposing a shipment by id alone.
	shipment, err := h.shipments.GetByOrderShipment(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}

	next := models.NextShipmentStates(shipment.StatusCode, actor.Role)

	// Same filtering as the order transitions, and for the same reason: a
	// client renders buttons from this list, so an entry the caller cannot
	// perform becomes a button that 403s. It matters more here — the two
	// approval steps are held by the warehouse and the movement steps by the
	// driver, so an unfiltered list offers each of them the other's actions.
	out := make([]gin.H, 0, len(next))
	for _, s := range next {
		if !callerMayReachShipment(c, s) {
			continue
		}
		out = append(out, gin.H{
			"status": s,
			"label":  models.StatusLabel(models.DomainShipment, s),
			"alias":  models.StatusAlias(models.DomainShipment, s),
		})
	}

	response.OK(c, gin.H{"current": shipment.StatusCode, "transitions": out})
}

type documentRequest struct {
	DocType  string                 `json:"docType" binding:"required"`
	Stage    string                 `json:"stage"`
	FileURL  string                 `json:"fileUrl" binding:"required"`
	Metadata map[string]interface{} `json:"metadata"`
}

// AttachDocument records a POD or checklist upload.
//
// @Summary  Attach a shipment document
// @Tags     Shipments
// @Security BearerAuth
// @Param    id path string true "Shipment ID"
// @Success  201 {object} object
// @Router   /shipments/{id}/documents [post]
func (h *ShipmentHandler) AttachDocument(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id")
	if !ok {
		return
	}

	var req documentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	doc := models.ShipmentDocument{
		DocType:  req.DocType,
		FileURL:  req.FileURL,
		Metadata: models.JSONB(req.Metadata),
	}
	if req.Stage != "" {
		if req.Stage != "loading" && req.Stage != "unloading" {
			response.BadRequest(c, `stage must be "loading" or "unloading"`)
			return
		}
		doc.Stage = &req.Stage
	}

	if err := h.shipments.AttachDocument(c.Request.Context(), actor, id, doc); err != nil {
		writeError(c, err)
		return
	}

	response.Created(c, gin.H{"attached": true})
}

// Documents lists a shipment's attachments.
//
// @Summary  List shipment documents
// @Tags     Shipments
// @Security BearerAuth
// @Param    id path string true "Shipment ID"
// @Success  200 {object} object
// @Router   /shipments/{id}/documents [get]
func (h *ShipmentHandler) Documents(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c, "id")
	if !ok {
		return
	}

	docs, err := h.shipments.Documents(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, docs)
}

// callerMayReachShipment reports whether this caller holds the permission a
// shipment status change requires, sharing its lookup with allowStatusChange so
// the offer and the refusal cannot disagree.
func callerMayReachShipment(c *gin.Context, status string) bool {
	key, known := models.PermissionForShipmentStatus(status)
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
