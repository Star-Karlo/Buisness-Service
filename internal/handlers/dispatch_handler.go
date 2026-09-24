package handlers

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/karlo/business-service/internal/fieldconfig"
	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/platform/authctx"
	"github.com/karlo/business-service/internal/platform/response"
	"github.com/karlo/business-service/internal/repository"
	"github.com/karlo/business-service/internal/services"
)

// DispatchHandler serves the planner's screen: candidate trucks, routes, and
// the paid re-plan.
type DispatchHandler struct {
	dispatch *services.DispatchService
}

func NewDispatchHandler(d *services.DispatchService) *DispatchHandler {
	return &DispatchHandler{dispatch: d}
}

// Candidates lists assignable trucks, nearest to the loading point first.
//
// @Summary  List candidate trucks for an order
// @Tags     Dispatch
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /orders/{id}/candidates [get]
func (h *DispatchHandler) Candidates(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid order id")
		return
	}

	candidates, err := h.dispatch.Candidates(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, candidates)
}

// Routes returns an order's planned legs.
//
// @Summary  Get order routes
// @Tags     Dispatch
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /orders/{id}/routes [get]
func (h *DispatchHandler) Routes(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid order id")
		return
	}

	legs, err := h.dispatch.Routes(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, legs)
}

type rerouteRequest struct {
	// Leg is "haul" or "approach". Defaults to approach, which is the one a
	// planner re-plans: the haul is fixed by the warehouses, while the
	// approach goes stale as the truck moves.
	Leg string `json:"leg"`
}

// Reroute re-plans a leg. This is the paid feature.
//
// The entitlement is read from the caller's token rather than looked up,
// because the token already carries the company's features and a second lookup
// would be a round trip that can disagree with the token in flight.
//
// @Summary  Re-plan an order route
// @Tags     Dispatch
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Failure  402 {object} response.Envelope
// @Router   /orders/{id}/reroute [post]
func (h *DispatchHandler) Reroute(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	principal, ok := authctx.Gin(c)
	if !ok {
		response.Unauthorized(c, "No token provided.")
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid order id")
		return
	}

	var req rerouteRequest
	_ = c.ShouldBindJSON(&req)
	if req.Leg == "" {
		req.Leg = models.LegApproach
	}

	entitled := principal.CompanyHasFeature(authctx.ProductTMS, services.FeatureAdvancedRouting)

	leg, err := h.dispatch.Reroute(c.Request.Context(), actor, entitled, id, req.Leg)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, leg)
}

// ---------------------------------------------------------------------------
// Field configuration
// ---------------------------------------------------------------------------

// FieldConfigHandler serves the per-company form configuration.
type FieldConfigHandler struct {
	config *services.FieldConfigService
}

func NewFieldConfigHandler(cfg *services.FieldConfigService) *FieldConfigHandler {
	return &FieldConfigHandler{config: cfg}
}

// Get returns the effective field configuration for an entity.
//
// The same answer the create handlers validate against, from the same method.
// A separate "what does the form look like" endpoint that computed it another
// way would drift, and the symptom would be a form that refuses its own output.
//
// @Summary  Get form field configuration
// @Tags     Configuration
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /config/fields/{entity} [get]
func (h *FieldConfigHandler) Get(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}

	entity := fieldconfig.Entity(c.Param("entity"))
	cfg, err := h.config.For(c.Request.Context(), actor.CompanyID, entity)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, gin.H{"entity": entity, "fields": cfg.Ordered()})
}

type updateFieldConfigRequest struct {
	Fields []services.FieldSetting `json:"fields" binding:"required"`
}

// Update changes what this company's form demands.
//
// @Summary  Update form field configuration
// @Tags     Configuration
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /config/fields/{entity} [put]
func (h *FieldConfigHandler) Update(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}

	var req updateFieldConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	entity := fieldconfig.Entity(c.Param("entity"))
	cfg, err := h.config.Update(c.Request.Context(), actor, entity, req.Fields)
	if err != nil {
		writeError(c, err)
		return
	}

	response.OK(c, gin.H{"entity": entity, "fields": cfg.Ordered()})
}

// ---------------------------------------------------------------------------
// Allowance
// ---------------------------------------------------------------------------

// AllowanceHandler serves uang sangu.
type AllowanceHandler struct {
	allowances *services.AllowanceService
}

func NewAllowanceHandler(a *services.AllowanceService) *AllowanceHandler {
	return &AllowanceHandler{allowances: a}
}

// Get returns the advance and the evidence behind it.
//
// @Summary  Get driver allowance
// @Tags     Allowance
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /orders/{id}/allowance [get]
// List is the Uang Sangu screen: every order that needs or has an advance.
func (h *AllowanceHandler) List(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "0"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	state := c.Query("state")

	rows, total, err := h.allowances.List(c.Request.Context(), actor, state, page, pageSize)
	if err != nil {
		writeError(c, err)
		return
	}
	if rows == nil {
		rows = []repository.AllowanceListRow{}
	}
	pages := 0
	if pageSize > 0 {
		pages = int((total + int64(pageSize) - 1) / int64(pageSize))
	}
	response.Paginated(c, rows, &response.Meta{Page: page, Limit: pageSize, TotalRows: total, TotalPages: pages})
}

func (h *AllowanceHandler) Get(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid order id")
		return
	}

	view, err := h.allowances.Get(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, view)
}

type allowanceComponentRequest struct {
	Code   string          `json:"code"`
	Label  string          `json:"label"`
	Amount decimal.Decimal `json:"amount"`
	Note   string          `json:"note"`
}

type saveAllowanceRequest struct {
	Components []allowanceComponentRequest `json:"components"`
	CurrencyID string                      `json:"currencyId"`
	Note       string                      `json:"note"`
	Reason     string                      `json:"reason"`
}

// Save records the advance.
//
// @Summary  Set driver allowance
// @Tags     Allowance
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /orders/{id}/allowance [put]
func (h *AllowanceHandler) Save(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid order id")
		return
	}

	var req saveAllowanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	components := make([]models.AllowanceComponent, 0, len(req.Components))
	for _, comp := range req.Components {
		components = append(components, models.AllowanceComponent{
			Code: comp.Code, Label: comp.Label, Amount: comp.Amount, Note: comp.Note,
		})
	}

	view, err := h.allowances.Save(c.Request.Context(), actor, id, services.SaveAllowanceInput{
		Components: components,
		CurrencyID: req.CurrencyID,
		Note:       req.Note,
		Reason:     req.Reason,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, view)
}

// Finalise commits the advance to the driver.
//
// @Summary  Finalise driver allowance
// @Tags     Allowance
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /orders/{id}/allowance/finalise [post]
func (h *AllowanceHandler) Finalise(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid order id")
		return
	}

	view, err := h.allowances.Finalise(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, view)
}

// History returns every superseded version of the advance.
//
// @Summary  Driver allowance history
// @Tags     Allowance
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /orders/{id}/allowance/history [get]
func (h *AllowanceHandler) History(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid order id")
		return
	}

	rows, err := h.allowances.History(c.Request.Context(), actor, id)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, rows)
}

// ---------------------------------------------------------------------------
// Handover
// ---------------------------------------------------------------------------

// HandoverHandler serves the unloading-point code exchange.
type HandoverHandler struct {
	handovers *services.HandoverService
}

func NewHandoverHandler(h *services.HandoverService) *HandoverHandler {
	return &HandoverHandler{handovers: h}
}

type issueHandoverRequest struct {
	PICName string `json:"picName"`
	// Optional since Web-Field: the PIC's number addresses the courtesy
	// message with the field link, never the code.
	PICWhatsapp string `json:"picWhatsapp"`
}

// Issue creates the unloading code and gives it to the driver.
//
// The code is in the response because the driver is its recipient: they enter
// it in K-Trip and read it out to the PIC, who types it into Web-Field. It is
// also pushed to the driver's phone, so a driver who closed the app still has
// it.
//
// @Summary  Issue the unloading code to the driver
// @Tags     Shipments
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /shipments/{id}/handover [post]
func (h *HandoverHandler) Issue(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid shipment id")
		return
	}

	var req issueHandoverRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	issued, err := h.handovers.Issue(c.Request.Context(), actor, id, req.PICName, req.PICWhatsapp)
	if err != nil {
		writeError(c, err)
		return
	}
	row := issued.Handover

	out := gin.H{
		"code":      issued.Code,
		"expiresAt": row.ExpiresAt,
	}
	if row.PICWhatsApp != "" {
		out["picNotified"] = maskNumber(row.PICWhatsApp)
	}
	// The link the PIC was sent, so the driver can forward it themselves when
	// the message did not arrive. It opens the audit with nothing to type;
	// the token form is kept for clients that still read it.
	if issued.PICLink != "" {
		out["picUrl"] = issued.PICLink
	}
	if row.FieldToken != nil {
		out["fieldToken"] = *row.FieldToken
		out["fieldUrl"] = h.handovers.FieldURL(*row.FieldToken)
	}
	response.OK(c, out)
}

type verifyHandoverRequest struct {
	Code      string   `json:"code" binding:"required"`
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
}

// Verify checks the code the PIC read out.
//
// @Summary  Verify the handover code
// @Tags     Shipments
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /shipments/{id}/handover/verify [post]
func (h *HandoverHandler) Verify(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		response.BadRequest(c, "Invalid shipment id")
		return
	}

	var req verifyHandoverRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	row, err := h.handovers.Verify(c.Request.Context(), actor, id, services.VerifyInput{
		Code: req.Code, Latitude: req.Latitude, Longitude: req.Longitude,
	})
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, row)
}

// maskNumber shows enough of a number to confirm it is the right one, without
// echoing it back in full to a caller who may have typed it wrong.
func maskNumber(number string) string {
	if len(number) <= 4 {
		return number
	}
	return "••••••" + number[len(number)-4:]
}

// DriverActivity lists (driver, truck) trip counts and last activity for the
// caller's company, for the fleet pairing screen's suggestions.
//
// @Summary  Driver activity per truck
// @Tags     Dispatch
// @Security BearerAuth
// @Success  200 {object} response.Envelope
// @Router   /fleet/driver-activity [get]
func (h *DispatchHandler) DriverActivity(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	rows, err := h.dispatch.DriverActivity(c.Request.Context(), actor)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, rows)
}

// FleetLive lists where the company's trucks are, for the planner's map.
//
// @Summary  Live fleet positions
// @Tags     Dispatch
// @Security BearerAuth
// @Success  200 {array} services.FleetPosition
// @Router   /fleet/live [get]
func (h *DispatchHandler) FleetLive(c *gin.Context) {
	actor, ok := callerActor(c)
	if !ok {
		return
	}
	rows, err := h.dispatch.FleetPositions(c.Request.Context(), actor)
	if err != nil {
		writeError(c, err)
		return
	}
	response.OK(c, rows)
}
