package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/services"
)

// PodHandler serves the driver flow's proof of delivery: submission by the
// driver, review by the company, and the cargo checks that gate them.
type PodHandler struct {
	pods      *services.PodService
	shipments *services.ShipmentService
}

func NewPodHandler(pods *services.PodService, shipments *services.ShipmentService) *PodHandler {
	return &PodHandler{pods: pods, shipments: shipments}
}

type acceptRequest struct {
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
}

// Accept is the driver's "terima order" swipe.
//
// @Summary  Accept the job (driver)
// @Tags     Shipments
// @Security BearerAuth
// @Param    id path string true "Shipment ID"
// @Success  200 {object} models.Shipment
// @Router   /shipments/{id}/accept [post]
func (h *PodHandler) Accept(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}
	var req acceptRequest
	_ = c.ShouldBindJSON(&req)
	var pos *services.Position
	if req.Latitude != nil && req.Longitude != nil {
		pos = &services.Position{Latitude: *req.Latitude, Longitude: *req.Longitude}
	}
	shipment, err := h.shipments.Accept(c.Request.Context(), actor, id, pos)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, shipment)
}

type cargoCheckRequest struct {
	Stage   string `json:"stage" binding:"required"`
	Matches *bool  `json:"matches" binding:"required"`
	Note    string `json:"note"`
}

// CargoCheck records "sesuai / tidak sesuai" for a stage.
//
// @Summary  Record the cargo check for a stage
// @Tags     Shipments
// @Security BearerAuth
// @Param    id path string true "Shipment ID"
// @Success  200 {object} models.Shipment
// @Router   /shipments/{id}/cargo-check [put]
func (h *PodHandler) CargoCheck(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}
	var req cargoCheckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	shipment, err := h.pods.CargoCheck(c.Request.Context(), actor, id, req.Stage, *req.Matches, req.Note)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, shipment)
}

type submitPodRequest struct {
	Stage  string              `json:"stage" binding:"required"`
	Photos []services.PodPhoto `json:"photos" binding:"required"`
	Note   string              `json:"note"`
}

// Submit files the driver's POD photos for review.
//
// @Summary  Submit POD photos (driver)
// @Tags     Shipments
// @Security BearerAuth
// @Param    id path string true "Shipment ID"
// @Success  201 {object} models.ShipmentPod
// @Router   /shipments/{id}/pod [post]
func (h *PodHandler) Submit(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}
	var req submitPodRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	pod, err := h.pods.Submit(c.Request.Context(), actor, id, services.SubmitInput{Stage: req.Stage, Photos: req.Photos, Note: req.Note})
	if err != nil {
		writeError(c, err)
		return
	}
	response.Created(c, pod)
}

// List returns every POD submission for a shipment, newest first.
//
// @Summary  List POD submissions
// @Tags     Shipments
// @Security BearerAuth
// @Param    id path string true "Shipment ID"
// @Success  200 {array} models.ShipmentPod
// @Router   /shipments/{id}/pod [get]
func (h *PodHandler) List(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}
	pods, err := h.pods.List(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, pods)
}

type reviewPodRequest struct {
	Approved *bool  `json:"approved" binding:"required"`
	Reason   string `json:"reason"`
}

// Review approves or rejects a submission; approval moves the shipment on.
//
// @Summary  Review a POD submission
// @Tags     Shipments
// @Security BearerAuth
// @Param    id path string true "Shipment ID"
// @Param    podId path string true "Submission ID"
// @Success  200 {object} models.ShipmentPod
// @Router   /shipments/{id}/pod/{podId}/review [put]
func (h *PodHandler) Review(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, ok := pathUUID(c)
	if !ok {
		return
	}
	podID, err := uuid.Parse(c.Param("podId"))
	if err != nil {
		response.BadRequest(c, "Invalid submission id")
		return
	}
	var req reviewPodRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	pod, err := h.pods.Review(c.Request.Context(), actor, id, podID, services.ReviewInput{Approved: *req.Approved, Reason: req.Reason})
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, pod)
}
