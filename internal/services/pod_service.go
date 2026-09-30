package services

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/karlo/business-service/internal/clients"
	"github.com/karlo/business-service/internal/models"
	notificationv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/notification/v1"
	"github.com/karlo/business-service/internal/repository"
)

// PodService is the driver flow's proof of delivery: the driver submits
// photos for a stage, somebody reviews them, and approval moves the shipment
// on. It sits beside ShipmentService rather than inside it because the review
// is the one shipment step a company user takes, not the driver.
type PodService struct {
	pods      *repository.PodRepository
	shipments *repository.ShipmentRepository
	orders    *repository.OrderRepository
	lifecycle *ShipmentService
	notifier  clients.Notifier
	// stops is the journey's visit list. Nil, or a POD with no stop, means a
	// two-ended trip and the original behaviour. Optional (tests).
	stops *repository.OrderStopRepository
}

// WithStops lets an approval close one visit rather than the whole delivery.
func (s *PodService) WithStops(stops *repository.OrderStopRepository) *PodService {
	s.stops = stops
	return s
}

func NewPodService(pods *repository.PodRepository, shipments *repository.ShipmentRepository, orders *repository.OrderRepository, lifecycle *ShipmentService, notifier clients.Notifier) *PodService {
	return &PodService{pods: pods, shipments: shipments, orders: orders, lifecycle: lifecycle, notifier: notifier}
}

// PodPhoto is one photo in a submission.
type PodPhoto struct {
	DocType string `json:"docType"`
	FileURL string `json:"fileUrl"`
}

// SubmitInput is the driver's submission.
type SubmitInput struct {
	Stage  string
	Photos []PodPhoto
	Note   string
	// StopID names the visit this POD closes. Empty on a two-ended journey;
	// the service resolves the current stop itself when the caller does not
	// say, so an app that has not learned about stops still behaves.
	StopID string
}

var podDocTypes = map[string]bool{"suratJalan": true, "muatan": true, "pendukung": true}

// Submit files the driver's photos for review.
//
// Order of gates: the shipment must be in the stage's working state, the
// stage's cargo check must be on record (the driver's own at loading, the
// PIC's at unloading), and the stage must not already be approved.
func (s *PodService) Submit(ctx context.Context, actor Actor, shipmentID uuid.UUID, in SubmitInput) (*models.ShipmentPod, error) {
	shipment, order, err := s.load(ctx, actor, shipmentID)
	if err != nil {
		return nil, err
	}
	if err := s.lifecycle.assertActorMayAdvance(actor, shipment, order); err != nil {
		return nil, err
	}

	var wantStatus string
	var checked *time.Time
	switch in.Stage {
	case "loading":
		wantStatus, checked = models.ShipmentLoading, shipment.LoadingCargoCheckedAt
	case "unloading":
		wantStatus, checked = models.ShipmentUnloading, shipment.UnloadingCargoCheckedAt
	default:
		return nil, fmt.Errorf("%w: stage must be loading or unloading", ErrValidation)
	}
	// Which visit this POD closes. Resolved before the state check, because
	// on a journey with stops the STOP is what the POD has to agree with,
	// not the shipment.
	stopID := s.resolveStop(ctx, order.ID, in)
	stop := s.stopByID(ctx, order.ID, stopID)

	// The FIRST point of a kind is worked through the shipment's own statuses
	// — the driver app reports toLoading, atLoading, loading there, and its
	// copy of those onto the stop row is best effort. With geofencing on that
	// copy is refused outright when the phone's fix is outside the fence, so
	// the row keeps a null started_at for a visit the driver plainly began.
	// Gating that visit on the row therefore refused its POD; the shipment,
	// which did record the work, is the honest gate for it.
	stopDriven := stop != nil && !s.isFirstStopOfKind(ctx, order.ID, stop)
	if stop != nil && !stopDriven && stop.StartedAt != nil {
		// Unless the row was written after all, in which case it is as good
		// an account of the visit as the shipment's.
		stopDriven = true
	}

	if stopDriven {
		// A journey through several points has no single status that is true
		// of the point being worked: with the order Muat 1 → Bongkar 1 →
		// Muat 2 → Bongkar 2 the truck unloads at Semarang while a loading
		// point is still outstanding, and the shipment cannot read both. The
		// stop can, so the stop is the gate: work must have begun there, and
		// it must not already be closed.
		if stop.StartedAt == nil {
			return nil, fmt.Errorf("%w: mulai proses di %s dulu sebelum mengirim POD", ErrValidation, stopLabel(stop))
		}
		if stop.FinishedAt != nil {
			return nil, fmt.Errorf("%w: %s sudah selesai", ErrTransition, stopLabel(stop))
		}
		// The cargo check belongs to the visit too.
		checked = stop.CargoCheckedAt
	} else if shipment.StatusCode != wantStatus {
		return nil, fmt.Errorf("%w: the %s POD is submitted while the shipment is %s, not %s", ErrTransition, in.Stage, wantStatus, shipment.StatusCode)
	}
	// At unloading the check is the PIC's, and it is waived while Web-Field
	// is hidden — see WebFieldEnabled. The driver's own check at loading is
	// never waived: it is their statement about what they loaded.
	if checked == nil && (in.Stage == "loading" || WebFieldEnabled) {
		if in.Stage == "loading" {
			return nil, fmt.Errorf("%w: confirm whether the cargo matches the order before submitting the POD", ErrValidation)
		}
		return nil, fmt.Errorf("%w: the receiving PIC has not verified the cargo yet", ErrValidation)
	}
	if len(in.Photos) == 0 {
		return nil, fmt.Errorf("%w: at least one photo is required", ErrValidation)
	}
	photos := make(models.JSONArray, 0, len(in.Photos))
	for _, p := range in.Photos {
		if !podDocTypes[p.DocType] {
			return nil, fmt.Errorf("%w: docType must be suratJalan, muatan or pendukung", ErrValidation)
		}
		if strings.TrimSpace(p.FileURL) == "" {
			return nil, fmt.Errorf("%w: every photo needs a fileUrl", ErrValidation)
		}
		photos = append(photos, map[string]interface{}{"docType": p.DocType, "fileUrl": p.FileURL})
	}
	if approved, err := s.pods.HasApproved(ctx, shipmentID, in.Stage, stopID); err != nil {
		return nil, err
	} else if approved {
		return nil, fmt.Errorf("%w: the %s POD is already approved", ErrTransition, in.Stage)
	}

	pod := &models.ShipmentPod{
		ShipmentID:        shipmentID,
		Stage:             in.Stage,
		StopID:            stopID,
		Photos:            photos,
		Status:            models.PodSubmitted,
		SubmittedByUserID: &actor.UserID,
		SubmittedAt:       time.Now(),
	}
	if n := strings.TrimSpace(in.Note); n != "" {
		pod.Note = &n
	}
	if err := s.pods.Submit(ctx, pod); err != nil {
		return nil, err
	}

	// Tell the people who review: the transporter's staff, and the shipper's
	// warehouse PIC who signs the goods in or out.
	s.notify(ctx, actor, order, "podSubmitted", in.Stage, "", reviewers(order))
	return pod, nil
}

// List returns every submission for a shipment, newest first.
func (s *PodService) List(ctx context.Context, actor Actor, shipmentID uuid.UUID) ([]models.ShipmentPod, error) {
	if _, _, err := s.load(ctx, actor, shipmentID); err != nil {
		return nil, err
	}
	return s.pods.ListByShipment(ctx, shipmentID)
}

// ReviewInput is the reviewer's decision.
type ReviewInput struct {
	Approved bool
	Reason   string
}

// Review approves or rejects a submission. Approval takes the shipment past
// the stage: loading → loaded, unloading → unloaded → finished (which
// completes the order). A rejection needs a reason, which the driver sees.
func (s *PodService) Review(ctx context.Context, actor Actor, shipmentID, podID uuid.UUID, in ReviewInput) (*models.ShipmentPod, error) {
	shipment, order, err := s.load(ctx, actor, shipmentID)
	if err != nil {
		return nil, err
	}
	if err := s.assertMayReview(actor, order); err != nil {
		return nil, err
	}
	pod, err := s.pods.FindByID(ctx, podID)
	if err != nil || pod.ShipmentID != shipmentID {
		return nil, repository.ErrNotFound
	}
	if pod.Status != models.PodSubmitted {
		return nil, fmt.Errorf("%w: this submission has already been reviewed", repository.ErrConflict)
	}

	if !in.Approved {
		reason := strings.TrimSpace(in.Reason)
		if reason == "" {
			return nil, fmt.Errorf("%w: say why the POD is rejected, so the driver knows what to fix", ErrValidation)
		}
		if err := s.pods.Review(ctx, podID, models.PodRejected, &reason, actor.UserID); err != nil {
			return nil, err
		}
		pod.Status, pod.RejectionReason = models.PodRejected, &reason
		s.notify(ctx, actor, order, "podRejected", pod.Stage, reason, driverOf(shipment))
		return pod, nil
	}

	if err := s.pods.Review(ctx, podID, models.PodApproved, nil, actor.UserID); err != nil {
		return nil, err
	}
	pod.Status = models.PodApproved

	// The visit this POD closes, on a journey with more than two points.
	// Recorded before the shipment moves, because whether the shipment moves
	// at all depends on whether any stop is still waiting.
	done, remaining, err := s.closeStop(ctx, pod, order.ID)
	if err != nil {
		return nil, err
	}
	if !done {
		// More stops of this kind to come. The delivery is not finished;
		// the driver's next arrival continues it.
		s.notify(ctx, actor, order, "podApproved", pod.Stage, "", driverOf(shipment))
		slog.InfoContext(ctx, "stop closed, journey continues",
			"shipmentId", shipment.ID, "stage", pod.Stage, "stopsRemaining", remaining)
		return pod, nil
	}

	// The step the approval means. Taken as the system, on the reviewer's
	// behalf; the state machine still checks the shipment is where the
	// stage says it is.
	// Approving the loading POD also sends the truck on its way: the status
	// sheet has "Menuju titik bongkar" triggered by the planner's approval,
	// not by the truck leaving the yard. The driver's geofence exit no longer
	// has a step to take, which is the point — the order stops sitting on
	// "POD muat terverifikasi" while a loaded truck waits for a gate.
	steps := []string{models.ShipmentLoaded, models.ShipmentToUnloading}
	if pod.Stage == "unloading" {
		steps = []string{models.ShipmentUnloaded, models.ShipmentFinished}
	}
	for _, to := range steps {
		// Walked rather than stepped. On an interleaved journey the shipment
		// may be anywhere on the chain when the last stop of a stage closes
		// — approving the final Bongkar while the shipment still reads
		// toUnloading is a jump the table does not allow — so it is taken
		// through the states in between.
		// No position: the planner is approving from a desk, and the steps
		// the system may take carry no geofence check.
		next, err := s.lifecycle.walkShipmentTo(ctx, actor, shipment, order, to, models.RoleSystem, nil)
		if err != nil {
			return nil, err
		}
		shipment = next
	}
	s.notify(ctx, actor, order, "podApproved", pod.Stage, "", driverOf(shipment))
	return pod, nil
}

// resolveStop decides which visit a submission belongs to.
//
// The driver app names the stop once it knows about stops; until then — and
// on a two-ended journey — the server works it out, because the alternative
// is a POD that closes nothing and a delivery that never finishes.
func (s *PodService) resolveStop(ctx context.Context, orderID uuid.UUID, in SubmitInput) *uuid.UUID {
	if s.stops == nil {
		return nil
	}
	if in.StopID != "" {
		if id, err := uuid.Parse(in.StopID); err == nil {
			return &id
		}
	}
	stops, err := s.stops.ListByOrder(ctx, orderID)
	if err != nil || len(stops) == 0 {
		return nil
	}
	kind := models.StopUnload
	if in.Stage == "loading" {
		kind = models.StopLoad
	}
	for i := range stops {
		if stops[i].Kind == kind && stops[i].FinishedAt == nil {
			id := stops[i].ID
			return &id
		}
	}
	return nil
}

// closeStop marks the visit this POD belongs to as finished, and says whether
// the journey's stops of that kind are now all done.
//
// A delivery that unloads at Semarang and then at Priok files two unloading
// PODs. Without this, approving the first advanced the shipment to unloaded
// and finished — the second stop was never visited and the order was closed
// from a warehouse the goods had not reached. A journey with no stops
// recorded is "done" by definition, which is what keeps two-ended trips and
// everything filed before stops existed behaving exactly as before.
func (s *PodService) closeStop(ctx context.Context, pod *models.ShipmentPod, orderID uuid.UUID) (done bool, remaining int, err error) {
	if s.stops == nil || pod.StopID == nil {
		return true, 0, nil
	}
	now := time.Now()
	if err := s.stops.UpdateFields(ctx, *pod.StopID, map[string]interface{}{
		"finished_at": now,
		"pod_id":      pod.ID,
	}); err != nil {
		return false, 0, err
	}

	kind := models.StopUnload
	if pod.Stage == "loading" {
		kind = models.StopLoad
	}
	stops, err := s.stops.ListByOrder(ctx, orderID)
	if err != nil {
		// Unknown rather than wrong: refusing to advance would strand the
		// delivery, and advancing blindly would close it early. The safer
		// of the two is to carry on as a two-ended trip would.
		return true, 0, nil
	}
	for i := range stops {
		if stops[i].Kind == kind && stops[i].FinishedAt == nil && stops[i].ID != *pod.StopID {
			remaining++
		}
	}
	return remaining == 0, remaining, nil
}

// CargoCheck records the "sesuai / tidak sesuai" answer for a stage from an
// authenticated caller: the driver at loading, company staff or the
// warehouse PIC at unloading (the field page is the unauthenticated route).
func (s *PodService) CargoCheck(ctx context.Context, actor Actor, shipmentID uuid.UUID, stage string, matches bool, note string) (*models.Shipment, error) {
	shipment, order, err := s.load(ctx, actor, shipmentID)
	if err != nil {
		return nil, err
	}
	via := "console"
	switch stage {
	case "loading":
		if err := s.lifecycle.assertActorMayAdvance(actor, shipment, order); err != nil {
			return nil, err
		}
		// Loading must actually have begun. "Does what I loaded match the
		// order?" has no meaning before it, and answering early was a trap:
		// with geofencing on, a driver whose Muat slide was refused could
		// still record "tidak sesuai", which showed on the order as though
		// the load had been checked, while the POD stayed refused for a
		// shipment that was not loading — and the app, seeing the check
		// recorded, no longer offered the question. The driver could go
		// neither forward nor back.
		if !models.ShipmentStatusAtOrPast(shipment.StatusCode, models.ShipmentLoading) {
			return nil, fmt.Errorf("%w: mulai muat dulu sebelum menjawab kesesuaian item", ErrValidation)
		}
		via = "app"
	case "unloading":
		if actor.Role == models.RoleDriver && !actor.StatusBypass {
			return nil, fmt.Errorf("%w: the receiving PIC verifies the cargo at unloading, not the driver", ErrForbidden)
		}
		if err := s.assertMayReview(actor, order); err != nil {
			return nil, err
		}
	}
	by := actor.UserID
	if err := s.lifecycle.RecordCargoCheck(ctx, shipment, CargoCheckInput{Stage: stage, Matches: matches, Note: note, Via: via, By: &by}); err != nil {
		return nil, err
	}
	return s.shipments.FindByID(ctx, shipmentID)
}

func (s *PodService) load(ctx context.Context, actor Actor, shipmentID uuid.UUID) (*models.Shipment, *models.Order, error) {
	shipment, err := s.shipments.FindByID(ctx, shipmentID)
	if err != nil {
		return nil, nil, err
	}
	order, err := s.orders.FindByID(ctx, actor.CompanyID, shipment.OrderID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: this shipment does not belong to your company", ErrForbidden)
	}
	return shipment, order, nil
}

// assertMayReview: anyone on either side of the order except its driver.
func (s *PodService) assertMayReview(actor Actor, order *models.Order) error {
	if actor.Role == models.RoleAdmin || actor.Role == models.RoleSuperadmin || actor.StatusBypass {
		return nil
	}
	if actor.Role == models.RoleDriver {
		return fmt.Errorf("%w: a driver cannot review their own POD", ErrForbidden)
	}
	if !order.InvolvesCompany(actor.CompanyID) {
		return fmt.Errorf("%w: your company is not a party to this order", ErrForbidden)
	}
	return nil
}

func reviewers(order *models.Order) *notificationv1.Audience {
	if order.TransporterCompanyID != nil {
		return clients.ToCompanyRoles(order.TransporterCompanyID.String(), models.RoleTransporter, models.RoleManager, models.RoleAdmin)
	}
	return clients.ToCompanyRoles(order.ShipperCompanyID.String(), models.RoleShipper, models.RoleWarehousePic, models.RoleAdmin)
}

func driverOf(shipment *models.Shipment) *notificationv1.Audience {
	if shipment.DriverUserID == nil {
		return nil
	}
	return clients.ToUsers(shipment.DriverUserID.String())
}

func (s *PodService) notify(ctx context.Context, actor Actor, order *models.Order, kind, stage, reason string, audience *notificationv1.Audience) {
	if audience == nil {
		return
	}
	eventType := map[string]notificationv1.EventType{
		"podSubmitted": notificationv1.EventType_EVENT_TYPE_POD_SUBMITTED,
		"podApproved":  notificationv1.EventType_EVENT_TYPE_POD_APPROVED,
		"podRejected":  notificationv1.EventType_EVENT_TYPE_POD_REJECTED,
	}[kind]
	stageLabel := map[string]string{"loading": "muat", "unloading": "bongkar"}[stage]
	s.notifier.Notify(ctx, clients.Event{
		Type:           eventType,
		Subject:        clients.Subject{ID: order.ID.String(), Type: "order"},
		Audience:       audience,
		ActorID:        actor.UserID.String(),
		IdempotencyKey: fmt.Sprintf("pod:%s:%s:%s:%d", order.ID, stage, kind, time.Now().Unix()),
		Params: map[string]interface{}{
			"orderNumber": order.OrderNumber,
			"event":       kind,
			"stage":       stage,
			"stageLabel":  stageLabel,
			"reason":      reason,
		},
	})
}

// stopByID reads one of an order's visits, or nil when the journey has no
// stops, the id is unknown, or the journey has only its two ends — a
// two-ended trip's statuses and its visits are the same events, so it keeps
// the shipment-level checks it always had.
func (s *PodService) stopByID(ctx context.Context, orderID uuid.UUID, stopID *uuid.UUID) *models.OrderStop {
	if s.stops == nil || stopID == nil {
		return nil
	}
	stops, err := s.stops.ListByOrder(ctx, orderID)
	if err != nil || len(stops) <= 2 {
		return nil
	}
	for i := range stops {
		if stops[i].ID == *stopID {
			return &stops[i]
		}
	}
	return nil
}

// isFirstStopOfKind says whether a visit is the earliest of its kind on the
// journey — the one the shipment's own statuses describe.
func (s *PodService) isFirstStopOfKind(ctx context.Context, orderID uuid.UUID, stop *models.OrderStop) bool {
	if s.stops == nil {
		return false
	}
	stops, err := s.stops.ListByOrder(ctx, orderID)
	if err != nil {
		return false
	}
	for i := range stops {
		if stops[i].Kind != stop.Kind {
			continue
		}
		return stops[i].ID == stop.ID
	}
	return false
}
