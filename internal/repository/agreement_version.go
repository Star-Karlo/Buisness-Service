package repository

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/karlo/business-service/internal/models"
)

// Lineage returns every version of one contract, newest first.
func (r *AgreementRepository) Lineage(ctx context.Context, companyID, rootID uuid.UUID) ([]models.Agreement, error) {
	var out []models.Agreement
	err := r.db.WithContext(ctx).Preload("Rates").
		Where("root_agreement_id = ? AND deleted_at IS NULL", rootID).
		Where("shipper_company_id = ? OR transporter_company_id = ?", companyID, companyID).
		Order("version DESC").
		Find(&out).Error
	return out, err
}

// ActiveVersion returns the version of a lineage that is live now.
//
// Live means approved and not yet superseded — not merely "status is active",
// because a version whose successor took over keeps its dates and would still
// match a date range. The successor's existence is what ends it.
func (r *AgreementRepository) ActiveVersion(ctx context.Context, rootID uuid.UUID) (*models.Agreement, error) {
	var a models.Agreement
	err := r.db.WithContext(ctx).Preload("Rates").
		Where("root_agreement_id = ? AND deleted_at IS NULL", rootID).
		Where("status_code = ?", models.AgreementActive).
		Where("superseded_at IS NULL").
		Order("version DESC").
		First(&a).Error
	return one(&a, err)
}

// PendingApprovals lists the versions waiting on a decision.
func (r *AgreementRepository) PendingApprovals(ctx context.Context, companyID uuid.UUID) ([]models.Agreement, error) {
	var out []models.Agreement
	err := r.db.WithContext(ctx).Preload("Rates").
		Where("deleted_at IS NULL AND status_code = ?", models.AgreementPendingApproval).
		Where("shipper_company_id = ? OR transporter_company_id = ?", companyID, companyID).
		Order("requested_at ASC").
		Find(&out).Error
	return out, err
}

// CreateRevision writes a new version of a lineage.
//
// The whole thing is one transaction, and the reason is the unique index on
// (root_agreement_id, version): two people amending the same contract at the
// same moment must not both write version 3. The version number is computed
// from a locked read of the current maximum, so the second writer waits, sees
// 3, and writes 4 — rather than both computing 3 and one failing on a
// constraint the caller has no way to recover from.
//
// The predecessor is NOT superseded here. An amendment under discussion must
// leave the agreed price orderable until somebody approves the change; that
// happens in Approve.
func (r *AgreementRepository) CreateRevision(ctx context.Context, revision *models.Agreement, previousID uuid.UUID) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var previous models.Agreement
		// SELECT ... FOR UPDATE on the predecessor serialises concurrent
		// revisions of one lineage. Locking the predecessor rather than the
		// root because a lineage is amended from its current version, so that
		// is the row two racing callers both read.
		if err := tx.Clauses(lockForUpdate()).
			Where("id = ? AND deleted_at IS NULL", previousID).
			First(&previous).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}

		var maxVersion int
		if err := tx.Model(&models.Agreement{}).
			Where("root_agreement_id = ? AND deleted_at IS NULL", previous.RootAgreementID).
			Select("COALESCE(MAX(version), 0)").
			Scan(&maxVersion).Error; err != nil {
			return err
		}

		// A lineage may hold only one undecided revision at a time. Two
		// pending amendments to the same contract would leave the approver
		// choosing between them with no way to express "this one, not that
		// one" — and whichever was approved second would supersede the first
		// silently.
		var pending int64
		if err := tx.Model(&models.Agreement{}).
			Where("root_agreement_id = ? AND deleted_at IS NULL", previous.RootAgreementID).
			Where("status_code = ?", models.AgreementPendingApproval).
			Count(&pending).Error; err != nil {
			return err
		}
		if pending > 0 {
			return ErrConflict
		}

		if revision.ID == uuid.Nil {
			revision.ID = uuid.New()
		}
		revision.Version = maxVersion + 1
		revision.RootAgreementID = previous.RootAgreementID
		revision.SupersedesAgreementID = &previousID

		return tx.Session(&gorm.Session{FullSaveAssociations: true}).Create(revision).Error
	})
}

// Approve activates a pending version and retires the one it replaces.
//
// Both writes are one transaction because the invariant is "exactly one live
// version per lineage". Activating without superseding would leave two, and an
// order placed in the gap could be priced against either.
func (r *AgreementRepository) Approve(ctx context.Context, id, actor uuid.UUID, note string) (*models.Agreement, error) {
	var approved models.Agreement

	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(lockForUpdate()).
			Where("id = ? AND deleted_at IS NULL", id).
			First(&approved).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if approved.StatusCode != models.AgreementPendingApproval {
			return ErrConflict
		}

		now := time.Now()
		fields := map[string]interface{}{
			"status_code":         models.AgreementActive,
			"approved_by_user_id": actor,
			"approved_at":         now,
			"updated_at":          now,
		}
		if note != "" {
			fields["decision_note"] = note
		}
		if err := tx.Model(&models.Agreement{}).Where("id = ?", id).Updates(fields).Error; err != nil {
			return err
		}

		// Retire whichever versions were live. Plural deliberately: if an
		// earlier bug ever left two active, this closes both rather than
		// leaving one behind to be found later by an invoice that disagrees.
		if err := tx.Model(&models.Agreement{}).
			Where("root_agreement_id = ? AND id <> ?", approved.RootAgreementID, id).
			Where("status_code = ? AND superseded_at IS NULL", models.AgreementActive).
			Updates(map[string]interface{}{
				"status_code":   models.AgreementSuperseded,
				"superseded_at": now,
				"updated_at":    now,
			}).Error; err != nil {
			return err
		}

		approved.StatusCode = models.AgreementActive
		approved.ApprovedAt = &now
		approved.ApprovedByUserID = &actor
		return nil
	})

	if err != nil {
		return nil, err
	}
	return &approved, nil
}

// Reject refuses a pending version, leaving the current one live.
func (r *AgreementRepository) Reject(ctx context.Context, id, actor uuid.UUID, note string) error {
	now := time.Now()
	fields := map[string]interface{}{
		"status_code":         models.AgreementRejected,
		"rejected_by_user_id": actor,
		"rejected_at":         now,
		"updated_at":          now,
	}
	if note != "" {
		fields["decision_note"] = note
	}

	res := r.db.WithContext(ctx).Model(&models.Agreement{}).
		Where("id = ? AND deleted_at IS NULL", id).
		Where("status_code = ?", models.AgreementPendingApproval).
		Updates(fields)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// Either it does not exist or it is not awaiting a decision. A
		// conflict rather than a not-found: the caller asked to decide
		// something that is not open.
		return ErrConflict
	}
	return nil
}

// PriceHistory reads the view the PRD's "view price history" renders.
func (r *AgreementRepository) PriceHistory(ctx context.Context, companyID, rootID uuid.UUID) ([]models.AgreementPriceLine, error) {
	var out []models.AgreementPriceLine

	// The view carries no company column, so authority is checked against the
	// lineage first. Without this, any authenticated caller who guessed a root
	// id could read another company's negotiated prices.
	var permitted int64
	if err := r.db.WithContext(ctx).Model(&models.Agreement{}).
		Where("root_agreement_id = ? AND deleted_at IS NULL", rootID).
		Where("shipper_company_id = ? OR transporter_company_id = ?", companyID, companyID).
		Count(&permitted).Error; err != nil {
		return nil, err
	}
	if permitted == 0 {
		return nil, ErrNotFound
	}

	err := r.db.WithContext(ctx).
		Table("agreement_price_history").
		Where("root_agreement_id = ?", rootID).
		Order("version DESC, rate_id").
		Find(&out).Error
	return out, err
}

// lockForUpdate takes a row lock for the rest of the transaction.
//
// Used where two callers can race on one lineage: revising and approving both
// read a row, decide from it, and write — and without the lock the decision is
// made on a value another transaction is about to change.
func lockForUpdate() clause.Locking {
	return clause.Locking{Strength: "UPDATE"}
}

// ---------------------------------------------------------------------------
// Lanes and the customers an agreement covers
// ---------------------------------------------------------------------------

// RatesNeedingRoute returns the lanes of an agreement that have not been
// measured.
//
// Every lane, not only the warehouse-level ones: the caller marks a city pair
// noCoordinates so a reader can tell "cannot have a distance" from "nobody has
// asked yet", and it can only do that if it sees them.
func (r *AgreementRepository) RatesNeedingRoute(ctx context.Context, agreementID uuid.UUID) ([]models.AgreementRate, error) {
	var out []models.AgreementRate
	err := r.db.WithContext(ctx).
		Where("agreement_id = ? AND route_status IS NULL", agreementID).
		Find(&out).Error
	return out, err
}

// UpdateRate applies the result of routing one lane.
func (r *AgreementRepository) UpdateRate(ctx context.Context, rateID uuid.UUID, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Model(&models.AgreementRate{}).
		Where("id = ?", rateID).Updates(fields).Error
}

// SetCustomers replaces the set of clients an agreement covers.
//
// Replace rather than diff: the form sends the list as the user left it, and
// reconciling by identity would need each row to carry a key the form has no
// reason to keep. Delete and insert are one transaction, so a failure cannot
// leave an agreement covering nobody.
func (r *AgreementRepository) SetCustomers(ctx context.Context, agreementID uuid.UUID, customers []models.AgreementCustomer) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("agreement_id = ?", agreementID).
			Delete(&models.AgreementCustomer{}).Error; err != nil {
			return err
		}
		if len(customers) == 0 {
			return nil
		}
		for i := range customers {
			customers[i].AgreementID = agreementID
		}
		return tx.Create(&customers).Error
	})
}

// Customers returns who an agreement covers.
func (r *AgreementRepository) Customers(ctx context.Context, agreementID uuid.UUID) ([]models.AgreementCustomer, error) {
	var out []models.AgreementCustomer
	err := r.db.WithContext(ctx).
		Where("agreement_id = ?", agreementID).Find(&out).Error
	return out, err
}

// AgreementsForCustomer finds every agreement covering a client.
//
// Through the join rather than shipper_company_id, which names only the
// counterparty the contract was struck with. A framework agreement covering ten
// clients would otherwise be invisible to nine of them.
func (r *AgreementRepository) AgreementsForCustomer(ctx context.Context, customerID uuid.UUID) ([]models.Agreement, error) {
	var out []models.Agreement
	err := r.db.WithContext(ctx).Preload("Rates").
		Joins("JOIN agreement_customers c ON c.agreement_id = agreements.id").
		Where("c.customer_company_id = ? AND agreements.deleted_at IS NULL", customerID).
		Order("agreements.created_at DESC").
		Find(&out).Error
	return out, err
}
