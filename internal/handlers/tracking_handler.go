package handlers

import (
	"github.com/gin-gonic/gin"

	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/services"
)

// TrackingHandler issues public tracking links and serves them.
type TrackingHandler struct {
	tracking *services.TrackingService
}

func NewTrackingHandler(t *services.TrackingService) *TrackingHandler {
	return &TrackingHandler{tracking: t}
}

// Link returns (issuing on first use) the order's public tracking token.
//
// @Summary  Get or create the order's public tracking link
// @Tags     Orders
// @Security BearerAuth
// @Param    id path string true "Order ID"
// @Success  200 {object} response.Envelope
// @Router   /orders/{id}/tracking-link [post]
func (h *TrackingHandler) Link(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}
	link, err := h.tracking.Link(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, gin.H{"token": link.Token, "createdAt": link.CreatedAt})
}

// Revoke stops the order's shared links from resolving.
//
// @Summary  Revoke the order's public tracking links
// @Tags     Orders
// @Security BearerAuth
// @Param    id path string true "Order ID"
// @Success  200 {object} response.Envelope
// @Router   /orders/{id}/tracking-link [delete]
func (h *TrackingHandler) Revoke(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}
	if err := h.tracking.Revoke(c.Request.Context(), actor, id); err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, gin.H{"revoked": true})
}

// Public answers a tracking token. No authentication: the token is the
// credential, and the answer is scoped to that one order.
//
// @Summary  Public tracking snapshot
// @Tags     Orders
// @Param    token path string true "Tracking token"
// @Success  200 {object} response.Envelope
// @Router   /orders/track/{token} [get]
func (h *TrackingHandler) Public(c *gin.Context) {
	token := c.Param("token")
	if len(token) < 16 || len(token) > 128 {
		response.NotFound(c, "Link tidak ditemukan")
		return
	}
	snap, err := h.tracking.Snapshot(c.Request.Context(), token)
	if err != nil {
		// Every failure reads the same to the outside: not found. A revoked,
		// expired or made-up token must not be distinguishable.
		response.NotFound(c, "Link tidak ditemukan")
		return
	}
	c.Header("Cache-Control", "no-store")
	response.OK(c, snap)
}
