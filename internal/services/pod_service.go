package services

import (
	"context"
	"fmt"
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
	if shipment.StatusCode != wantStatus {
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
	if approved, err := s.pods.HasApproved(ctx, shipmentID, in.Stage); err != nil {
		return nil, err
	} else if approved {
		return nil, fmt.Errorf("%w: the %s POD is already approved", ErrTransition, in.Stage)
	}

	pod := &models.ShipmentPod{
		ShipmentID:        shipmentID,
		Stage:             in.Stage,
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
		next, err := s.lifecycle.AdvanceBySystem(ctx, actor, shipment, order, to)
		if err != nil {
			return nil, err
		}
		shipment = next
	}
	s.notify(ctx, actor, order, "podApproved", pod.Stage, "", driverOf(shipment))
	return pod, nil
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
