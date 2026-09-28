package handlers

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/services"
)

// StopHandler serves the driver's reports from one point of a journey.
//
// A trip with a stop in the middle cannot record itself on the shipment's own
// arrival columns, which hold one loading and one unloading time. These act
// on the stop instead. No module permission, like the rest of the driver
// flow: the service checks the caller is the driver this shipment is
// assigned to.
type StopHandler struct {
	shipments *services.ShipmentService
}

func NewStopHandler(s *services.ShipmentService) *StopHandler { return &StopHandler{shipments: s} }

type stopVisitRequest struct {
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
	Matches   *bool    `json:"matches"`
	Note      string   `json:"note"`
}

func (h *StopHandler) input(c *gin.Context) (services.StopVisitInput, uuid.UUID, bool) {
	shipmentID, ok := pathUUID(c)
	if !ok {
		return services.StopVisitInput{}, uuid.Nil, false
	}
	stopID, err := uuid.Parse(c.Param("stopId"))
	if err != nil {
		response.BadRequest(c, "Invalid stop id")
		return services.StopVisitInput{}, uuid.Nil, false
	}
	var req stopVisitRequest
	_ = c.ShouldBindJSON(&req)

	in := services.StopVisitInput{StopID: stopID, Matches: req.Matches, Note: req.Note}
	if req.Latitude != nil && req.Longitude != nil {
		in.Position = &services.Position{Latitude: *req.Latitude, Longitude: *req.Longitude}
	}
	return in, shipmentID, true
}

// Arrive records reaching one point of the journey.
//
// @Summary  Report arrival at a stop
// @Tags     Shipments
// @Security BearerAuth
// @Router   /shipments/{id}/stops/{stopId}/arrive [put]
func (h *StopHandler) Arrive(c *gin.Context) {
	in, shipmentID, ok := h.input(c)
	if !ok {
		return
	}
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	stop, err := h.shipments.ArriveAtStop(c.Request.Context(), actor, shipmentID, in)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, stop)
}

// Start records the work beginning at one point.
//
// @Summary  Start loading or unloading at a stop
// @Tags     Shipments
// @Security BearerAuth
// @Router   /shipments/{id}/stops/{stopId}/start [put]
func (h *StopHandler) Start(c *gin.Context) {
	in, shipmentID, ok := h.input(c)
	if !ok {
		return
	}
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	stop, err := h.shipments.StartStop(c.Request.Context(), actor, shipmentID, in)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, stop)
}

// CargoCheck records sesuai / tidak sesuai at one point.
//
// @Summary  Record the cargo check at a stop
// @Tags     Shipments
// @Security BearerAuth
// @Router   /shipments/{id}/stops/{stopId}/cargo-check [put]
func (h *StopHandler) CargoCheck(c *gin.Context) {
	in, shipmentID, ok := h.input(c)
	if !ok {
		return
	}
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	stop, err := h.shipments.CheckStopCargo(c.Request.Context(), actor, shipmentID, in)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, stop)
}
