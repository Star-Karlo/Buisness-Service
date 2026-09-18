package handlers

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/routing"
)

// RoutingHandler plans journeys.
//
// It exists as a server endpoint rather than letting the planner call MAPID
// directly because the request carries an API key. A browser calling MAPID
// would ship that key to every visitor, and a key in a public bundle belongs to
// whoever finds it.
//
// It plans through the same cache the order flows use, so a lane the planner
// previews and then books costs one MAPID call, not two, and the same lane
// asked again tomorrow costs none.
type RoutingHandler struct {
	routes *routing.Cache
}

func NewRoutingHandler(r *routing.Cache) *RoutingHandler {
	return &RoutingHandler{routes: r}
}

type routeRequest struct {
	// Points are [longitude, latitude] pairs, in travel order. GeoJSON order,
	// which is the reverse of how coordinates are spoken — transposing them
	// puts an Indonesian route out of bounds rather than merely wrong, so it
	// usually fails loudly, but not always.
	Points [][2]float64 `json:"points" binding:"required"`

	// Profile defaults to truck. Planning a lorry on the car profile returns a
	// route the driver cannot legally take, and nothing on the map says so.
	Profile string `json:"profile"`

	AvoidTolls   bool `json:"avoidTolls"`
	IncludeTolls bool `json:"includeTolls"`
}

// Plan returns a road route between two or more points.
//
// @Summary  Plan a route
// @Tags     Routing
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /routing/route [post]
func (h *RoutingHandler) Plan(c *gin.Context) {
	var body routeRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if len(body.Points) < 2 {
		response.BadRequest(c, "A route needs at least two points")
		return
	}

	points := make([]routing.Point, 0, len(body.Points))
	for _, p := range body.Points {
		points = append(points, routing.Point{Lon: p[0], Lat: p[1]})
	}

	profile := routing.Profile(body.Profile)
	if profile == "" {
		profile = routing.ProfileTruck
	}
	if !profile.Valid() {
		response.BadRequest(c, "Unknown profile: "+body.Profile+" (car, truck, motorcycle, foot)")
		return
	}

	result, err := h.routes.Route(c.Request.Context(), routing.Request{
		Points:     points,
		Profile:    profile,
		AvoidTolls: body.AvoidTolls,
	})
	if err != nil {
		if errors.Is(err, routing.ErrNotConfigured) {
			response.InternalError(c, "Routing is not configured on this deployment")
			return
		}
		// MAPID's own messages are specific — out of bounds, no route found —
		// and far more useful to whoever is looking at the map than a generic
		// failure would be.
		response.BadRequest(c, err.Error())
		return
	}

	// Duration is seconds rather than a Go Duration: marshalled directly it
	// becomes nanoseconds, which reads as a nonsense integer to a client.
	route := result.Route
	response.OK(c, gin.H{
		"distanceMeters":  route.DistanceMeters,
		"durationSeconds": route.DurationSeconds(),
		"geometry":        route.Geometry,
		"bbox":            route.BBox,
		"tollSegments":    route.TollSegments,
		"hasToll":         result.HasToll,
		// Whether this answer came from the cache rather than MAPID.
		"cached": result.Cached,
	})
}
