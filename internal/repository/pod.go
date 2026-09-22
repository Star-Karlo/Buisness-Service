package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/karlo/business-service/internal/models"
)

// PodRepository keeps the driver's POD submissions.
type PodRepository struct{ db *gorm.DB }

func NewPodRepository(db *gorm.DB) *PodRepository { return &PodRepository{db: db} }

// Submit records a new submission, replacing any still-open one for the
// stage: a driver who re-uploads before the review lands wants the reviewer
// to see the new photos, not both sets. Rejected and approved rows stay.
func (r *PodRepository) Submit(ctx context.Context, pod *models.ShipmentPod) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("shipment_id = ? AND stage = ? AND status = ?",
			pod.ShipmentID, pod.Stage, models.PodSubmitted).Delete(&models.ShipmentPod{}).Error; err != nil {
			return err
		}
		if err := tx.Create(pod).Error; err != nil {
			return fmt.Errorf("repository: submit pod: %w", err)
		}
		return nil
	})
}

func (r *PodRepository) FindByID(ctx context.Context, id uuid.UUID) (*models.ShipmentPod, error) {
	var row models.ShipmentPod
	err := r.db.WithContext(ctx).First(&row, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &row, err
}

// ListByShipment returns every submission, newest first.
func (r *PodRepository) ListByShipment(ctx context.Context, shipmentID uuid.UUID) ([]models.ShipmentPod, error) {
	var out []models.ShipmentPod
	err := r.db.WithContext(ctx).Where("shipment_id = ?", shipmentID).
		Order("submitted_at DESC").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("repository: list pods: %w", err)
	}
	return out, nil
}

// Latest returns the most recent submission per stage.
func (r *PodRepository) Latest(ctx context.Context, shipmentID uuid.UUID) ([]models.ShipmentPod, error) {
	var out []models.ShipmentPod
	err := r.db.WithContext(ctx).Raw(`
		SELECT DISTINCT ON (stage) * FROM shipment_pods
		WHERE shipment_id = ? ORDER BY stage, submitted_at DESC`, shipmentID).Scan(&out).Error
	if err != nil {
		return nil, fmt.Errorf("repository: latest pods: %w", err)
	}
	return out, nil
}

// HasApproved says whether a stage already has an approved POD.
func (r *PodRepository) HasApproved(ctx context.Context, shipmentID uuid.UUID, stage string) (bool, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&models.ShipmentPod{}).
		Where("shipment_id = ? AND stage = ? AND status = ?", shipmentID, stage, models.PodApproved).Count(&n).Error
	return n > 0, err
}

// Review closes a submission. Guarded on status so two reviewers cannot both
// decide the same one.
func (r *PodRepository) Review(ctx context.Context, id uuid.UUID, status string, reason *string, by uuid.UUID) error {
	res := r.db.WithContext(ctx).Model(&models.ShipmentPod{}).
		Where("id = ? AND status = ?", id, models.PodSubmitted).
		Updates(map[string]interface{}{
			"status":              status,
			"rejection_reason":    reason,
			"reviewed_by_user_id": by,
			"reviewed_at":         time.Now(),
		})
	if res.Error != nil {
		return fmt.Errorf("repository: review pod: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: this submission has already been reviewed", ErrConflict)
	}
	return nil
}
