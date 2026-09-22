package handlers

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/services"
	"github.com/karlo/business-service/internal/telemetry"
)

// MobileTelemetryHandler relays the driver app's positions to FMS.
//
// The phone never holds an ingest key: the driver posts under their own
// login, the service stamps the master-data driver id (FMS's identity for a
// phone track) from the driver's active shipment, and forwards the batch
// with its service token.
type MobileTelemetryHandler struct {
	shipments *services.ShipmentService
	telemetry *telemetry.Client
}

func NewMobileTelemetryHandler(shipments *services.ShipmentService, t *telemetry.Client) *MobileTelemetryHandler {
	return &MobileTelemetryHandler{shipments: shipments, telemetry: t}
}

type mobileFix struct {
	Latitude     float64        `json:"latitude" binding:"required"`
	Longitude    float64        `json:"longitude" binding:"required"`
	SpeedKmh     *float64       `json:"speedKmh"`
	Heading      *float64       `json:"heading"`
	Altitude     *float64       `json:"altitude"`
	Accuracy     *float64       `json:"accuracy"`
	BatteryLevel *float64       `json:"batteryLevel"`
	RecordedAt   *time.Time     `json:"recordedAt"`
	IO           map[string]any `json:"io"`
}

type mobileBatch struct {
	Points []mobileFix `json:"points" binding:"required"`
}

// Ingest takes a batch of phone fixes from the assigned driver.
//
// @Summary  Relay driver-phone positions to FMS
// @Tags     Shipments
// @Security BearerAuth
// @Success  202 {object} response.Envelope
// @Router   /telemetry/mobile [post]
func (h *MobileTelemetryHandler) Ingest(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	if actor.Role != models.RoleDriver {
		response.Forbidden(c, "only a driver's phone reports positions")
		return
	}
	var req mobileBatch
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if len(req.Points) == 0 || len(req.Points) > 5000 {
		response.BadRequest(c, "between 1 and 5000 points per batch")
		return
	}
	// The master-data driver id comes from the shipment the driver is on.
	// No active shipment: the points are accepted and dropped — there is no
	// truck to hang them on, and the app need not know the difference.
	shipment, err := h.shipments.ActiveForDriver(c.Request.Context(), actor.UserID)
	if err != nil || shipment == nil || shipment.DriverID == nil {
		response.Accepted(c, gin.H{"stored": 0})
		return
	}
	readings := make([]telemetry.MobileReading, 0, len(req.Points))
	for _, p := range req.Points {
		io := map[string]any{"shipmentId": shipment.ID.String(), "orderId": shipment.OrderID.String(), "source": "ktrip"}
		if shipment.TruckID != nil {
			io["vehicleId"] = *shipment.TruckID
		}
		if p.Accuracy != nil {
			io["accuracy"] = *p.Accuracy
		}
		for k, v := range p.IO {
			io[k] = v
		}
		r := telemetry.MobileReading{
			Driver: *shipment.DriverID, Latitude: p.Latitude, Longitude: p.Longitude,
			Speed: p.SpeedKmh, Bearing: p.Heading, Altitude: p.Altitude, BatteryLevel: p.BatteryLevel, IO: io,
		}
		if p.RecordedAt != nil {
			r.GpsCreatedAt = p.RecordedAt.UTC().Format(time.RFC3339)
		}
		readings = append(readings, r)
	}
	stored, err := h.telemetry.IngestMobile(c.Request.Context(), readings)
	if err != nil {
		response.BadGateway(c, fmt.Sprintf("tracking service: %v", err))
		return
	}
	response.Accepted(c, gin.H{"stored": stored})
}
