package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/karlo/business-service/internal/clients"
	"github.com/karlo/business-service/internal/fieldconfig"
	"github.com/karlo/business-service/internal/models"
	notificationv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/notification/v1"
	"github.com/karlo/business-service/internal/repository"
)

// ReviseAgreementInput is a proposed new version of a contract.
type ReviseAgreementInput struct {
	// Kind is renewal or update, and it decides whether an approval is needed.
	Kind string
	Note string

	// ValidFrom and ValidUntil are required on a renewal, which is a new term
	// by definition. Optional on an update, which usually keeps the dates.
	ValidFrom  *time.Time
	ValidUntil *time.Time

	PaymentTypeID string
	CurrencyID    string
	Detail        map[string]interface{}

	// Rates replaces the price matrix wholesale. Replace rather than patch: a
	// revision is a restatement of the bargain, and a partial edit would leave
	// the reader unable to see what the version actually says without diffing
	// it against its predecessor.
	Rates []RateInput

	Present map[string]bool
}

// Revise proposes a new version of an agreement.
//
// A renewal takes effect immediately; an update waits on approval. That is the
// PRD's rule and the reason for it is commercial rather than technical: a
// renewal is a fresh negotiation both sides have just agreed, while an update
// changes a bargain already struck and mid-flight, which is exactly the thing a
// Sales Manager exists to sign off.
//
// Either way the predecessor stays live until a decision. An amendment under
// discussion must not stop work being ordered at the agreed price.
func (s *BillingService) Revise(ctx context.Context, actor Actor, previousID uuid.UUID, in ReviseAgreementInput) (*models.Agreement, error) {
	if in.Kind != models.RevisionRenewal && in.Kind != models.RevisionUpdate {
		return nil, fmt.Errorf("%w: a revision is a renewal or an update, not %q", ErrValidation, in.Kind)
	}

	previous, err := s.agreements.FindByID(ctx, actor.CompanyID, previousID)
	if err != nil {
		return nil, err
	}

	// Only the transporter side revises. The shipper agreed to a price; it
	// does not get to restate it, and letting either party write a version
	// would make "who changed this" a question the row cannot answer.
	if previous.TransporterCompanyID != actor.CompanyID {
		return nil, fmt.Errorf("%w: only the transporter may revise an agreement", ErrForbidden)
	}

	// A rejected or cancelled version is not a base to build on: it never took
	// effect, so a version superseding it would claim to replace something
	// that was never in force.
	switch previous.StatusCode {
	case models.AgreementRejected, models.AgreementCancelled:
		return nil, fmt.Errorf("%w: version %d was never in force, so there is nothing to revise",
			ErrValidation, previous.Version)
	}

	if s.fields != nil {
		// A revision inherits the parties and the agreement type from the
		// version it replaces; they are not in its body and cannot change.
		// Without this every renewal failed "missing required: Agreement
		// type" — the catalogue's check is the same one create runs.
		present := map[string]bool{"customerId": true, "agreementType": true}
		for k, v := range in.Present {
			present[k] = v
		}
		if err := s.fields.Validate(ctx, actor.CompanyID, fieldconfig.EntityAgreement, present); err != nil {
			return nil, err
		}
	}

	revision, err := s.buildRevision(actor, previous, in)
	if err != nil {
		return nil, err
	}

	if err := s.agreements.CreateRevision(ctx, revision, previousID); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, fmt.Errorf("%w: this agreement already has a revision awaiting approval",
				ErrValidation)
		}
		return nil, err
	}

	// A renewal is live at once; an update is announced to whoever approves.
	//
	// The renewal is created PENDING and put in force through Approve — the
	// one place that activates a version and retires the one it replaces in
	// a single transaction. Creating it active and then approving it
	// answered 409 (Approve rightly refuses a non-pending row) with the new
	// version already committed, leaving two live versions of the contract.
	if in.Kind == models.RevisionRenewal {
		approved, err := s.agreements.Approve(ctx, revision.ID, actor.UserID, "Renewal, no approval required")
		if err != nil {
			return nil, err
		}
		revision = approved
	} else {
		s.notifier.Notify(ctx, clients.Event{
			Type:    notificationv1.EventType_EVENT_TYPE_AGREEMENT_CREATED,
			Subject: clients.Subject{ID: revision.ID.String(), Type: "agreement"},
			Audience: clients.ToCompanyRoles(actor.CompanyID.String(),
				models.RoleManager, models.RoleAdmin),
			ActorID:        actor.UserID.String(),
			IdempotencyKey: "agreement-revision:" + revision.ID.String(),
			Params: map[string]interface{}{
				"agreementNumber": revision.AgreementNumber,
				"version":         revision.Version,
				"revisionKind":    in.Kind,
			},
		})
	}

	return revision, nil
}

// buildRevision assembles the new version from its predecessor and the changes.
//
// Fields the caller did not restate are carried forward. A revision is a new
// statement of the whole contract, so anything left unsaid means "unchanged" —
// not "cleared", which is what a plain struct copy of the input would do.
func (s *BillingService) buildRevision(actor Actor, previous *models.Agreement, in ReviseAgreementInput) (*models.Agreement, error) {
	now := time.Now()

	revision := &models.Agreement{
		// The number is the LINEAGE's, unchanged. A contract keeps its
		// identity across amendments — AGR-…-000004 version 2 is the same
		// contract — and minting a new number would make the history
		// unrecognisable to anyone holding the paper.
		AgreementNumber:      previous.AgreementNumber,
		ShipperCompanyID:     previous.ShipperCompanyID,
		TransporterCompanyID: previous.TransporterCompanyID,
		CreatedByUserID:      actor.UserID,
		ValidFrom:            previous.ValidFrom,
		ValidUntil:           previous.ValidUntil,
		PaymentTypeID:        previous.PaymentTypeID,
		CurrencyID:           previous.CurrencyID,
		Detail:               previous.Detail,
		RevisionKind:         &in.Kind,
		RequestedByUserID:    &actor.UserID,
		RequestedAt:          &now,
	}
	if in.Note != "" {
		revision.RevisionNote = &in.Note
	}
	if in.ValidFrom != nil {
		revision.ValidFrom = *in.ValidFrom
	}
	if in.ValidUntil != nil {
		revision.ValidUntil = *in.ValidUntil
	}
	setIfNotEmpty(&revision.PaymentTypeID, in.PaymentTypeID)
	setIfNotEmpty(&revision.CurrencyID, in.CurrencyID)
	if in.Detail != nil {
		revision.Detail = models.JSONB(in.Detail)
	}
	if revision.Detail == nil {
		revision.Detail = models.JSONB{}
	}

	if revision.ValidUntil.Before(revision.ValidFrom) {
		return nil, fmt.Errorf("%w: validUntil cannot be before validFrom", ErrValidation)
	}

	// A renewal must actually extend the term. Without this a "renewal" could
	// re-state the same dates, take effect with no approval, and quietly serve
	// as an unapproved price change — which is the control the update path
	// exists to impose.
	if in.Kind == models.RevisionRenewal && !revision.ValidUntil.After(previous.ValidUntil) {
		return nil, fmt.Errorf(
			"%w: a renewal must extend the term past %s; change price in place with an update",
			ErrValidation, previous.ValidUntil.Format("2006-01-02"))
	}

	rates := in.Rates
	if len(rates) == 0 {
		// Carry the price matrix forward unchanged — a renewal at the same
		// rates is the common case, and requiring it to be retyped invites
		// transcription errors into a priced contract.
		for _, r := range previous.Rates {
			carried := RateInput{Price: r.Price, MinQuantity: r.MinQuantity, LeadTimeHours: r.LeadTimeHours}
			carried.OriginCityID = deref(r.OriginCityID)
			carried.DestinationCityID = deref(r.DestinationCityID)
			carried.OriginDistrictID = deref(r.OriginDistrictID)
			carried.DestinationDistrictID = deref(r.DestinationDistrictID)
			carried.TruckTypeID = deref(r.TruckTypeID)
			carried.PricingTypeID = deref(r.PricingTypeID)
			rates = append(rates, carried)
		}
	}
	if len(rates) == 0 {
		return nil, fmt.Errorf("%w: an agreement needs at least one rate", ErrValidation)
	}

	for i, r := range rates {
		if r.Price.IsNegative() {
			return nil, fmt.Errorf("%w: rate %d has a negative price", ErrValidation, i)
		}
		rate := models.AgreementRate{
			Price: r.Price, MinQuantity: r.MinQuantity, LeadTimeHours: r.LeadTimeHours,
		}
		setIfNotEmpty(&rate.OriginCityID, r.OriginCityID)
		setIfNotEmpty(&rate.DestinationCityID, r.DestinationCityID)
		setIfNotEmpty(&rate.OriginDistrictID, r.OriginDistrictID)
		setIfNotEmpty(&rate.DestinationDistrictID, r.DestinationDistrictID)
		setIfNotEmpty(&rate.TruckTypeID, r.TruckTypeID)
		setIfNotEmpty(&rate.PricingTypeID, r.PricingTypeID)
		setIfNotEmpty(&rate.CurrencyID, deref(revision.CurrencyID))
		revision.Rates = append(revision.Rates, rate)
	}

	// Every revision is born pending. The rule the whole workflow turns on
	// is applied by Revise: a renewal is approved on the spot, an update
	// waits for someone other than its author.
	revision.StatusCode = models.AgreementPendingApproval

	return revision, nil
}

// Approve puts a pending version into force and retires the one it replaces.
//
// The approver must not be the requester. That is the entire point of the
// control: a Sales person who could approve their own price change has an
// approval step that costs a click and prevents nothing. Enforced here rather
// than by permissions alone, because one person can legitimately hold both keys
// in a small company and still must not use them on the same revision.
func (s *BillingService) Approve(ctx context.Context, actor Actor, id uuid.UUID, note string) (*models.Agreement, error) {
	agreement, err := s.agreements.FindByID(ctx, actor.CompanyID, id)
	if err != nil {
		return nil, err
	}
	if agreement.TransporterCompanyID != actor.CompanyID {
		return nil, fmt.Errorf("%w: only the transporter may approve its own revisions", ErrForbidden)
	}
	if agreement.RequestedByUserID != nil && *agreement.RequestedByUserID == actor.UserID {
		return nil, fmt.Errorf("%w: a revision cannot be approved by the person who requested it", ErrForbidden)
	}

	approved, err := s.agreements.Approve(ctx, id, actor.UserID, note)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return nil, fmt.Errorf("%w: this revision is not awaiting a decision", ErrValidation)
		}
		return nil, err
	}

	s.notifier.Notify(ctx, clients.Event{
		Type:           notificationv1.EventType_EVENT_TYPE_AGREEMENT_APPROVED,
		Subject:        clients.Subject{ID: id.String(), Type: "agreement"},
		Audience:       clients.ToUsers(userIDs(agreement.RequestedByUserID)...),
		ActorID:        actor.UserID.String(),
		IdempotencyKey: "agreement-approved:" + id.String(),
		Params: map[string]interface{}{
			"agreementNumber": agreement.AgreementNumber,
			"version":         agreement.Version,
		},
	})

	return approved, nil
}

// Reject refuses a pending version. The current one stays live.
func (s *BillingService) Reject(ctx context.Context, actor Actor, id uuid.UUID, note string) error {
	agreement, err := s.agreements.FindByID(ctx, actor.CompanyID, id)
	if err != nil {
		return err
	}
	if agreement.TransporterCompanyID != actor.CompanyID {
		return fmt.Errorf("%w: only the transporter may decide its own revisions", ErrForbidden)
	}
	if agreement.RequestedByUserID != nil && *agreement.RequestedByUserID == actor.UserID {
		return fmt.Errorf("%w: a revision cannot be decided by the person who requested it", ErrForbidden)
	}

	if err := s.agreements.Reject(ctx, id, actor.UserID, note); err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return fmt.Errorf("%w: this revision is not awaiting a decision", ErrValidation)
		}
		return err
	}

	s.notifier.Notify(ctx, clients.Event{
		Type:           notificationv1.EventType_EVENT_TYPE_AGREEMENT_REJECTED,
		Subject:        clients.Subject{ID: id.String(), Type: "agreement"},
		Audience:       clients.ToUsers(userIDs(agreement.RequestedByUserID)...),
		ActorID:        actor.UserID.String(),
		IdempotencyKey: "agreement-rejected:" + id.String(),
		Params: map[string]interface{}{
			"agreementNumber": agreement.AgreementNumber,
			"version":         agreement.Version,
			"note":            note,
		},
	})
	return nil
}

// Lineage returns every version of a contract, newest first.
func (s *BillingService) Lineage(ctx context.Context, actor Actor, rootID uuid.UUID) ([]models.Agreement, error) {
	versions, err := s.agreements.Lineage(ctx, actor.CompanyID, rootID)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, repository.ErrNotFound
	}
	return versions, nil
}

// PendingApprovals is the approver's queue.
func (s *BillingService) PendingApprovals(ctx context.Context, actor Actor) ([]models.Agreement, error) {
	return s.agreements.PendingApprovals(ctx, actor.CompanyID)
}

// PriceHistory answers the PRD's "view price history".
func (s *BillingService) PriceHistory(ctx context.Context, actor Actor, rootID uuid.UUID) ([]models.AgreementPriceLine, error) {
	return s.agreements.PriceHistory(ctx, actor.CompanyID, rootID)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// userIDs drops the nils, so an audience is never a list containing an empty
// string — which the notification service would treat as a user and fail to
// resolve.
func userIDs(ids ...*uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != nil {
			out = append(out, id.String())
		}
	}
	return out
}
