package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/karlo/business-service/internal/models"
)

// TrackingLinkRepository stores the public tracking tokens (see
// migrations/000010_tracking_links.up.sql).
type TrackingLinkRepository struct{ db *gorm.DB }

func NewTrackingLinkRepository(db *gorm.DB) *TrackingLinkRepository {
	return &TrackingLinkRepository{db: db}
}

// FindActiveByOrder returns the order's live link, if one was ever issued.
// One link per order: sharing it twice must hand out the same URL, or the
// first customer's link would quietly stop working when a colleague copies
// it again.
func (r *TrackingLinkRepository) FindActiveByOrder(ctx context.Context, orderID uuid.UUID) (*models.TrackingLink, error) {
	var link models.TrackingLink
	err := r.db.WithContext(ctx).
		Where("order_id = ? AND revoked_at IS NULL", orderID).
		Order("created_at DESC").
		First(&link).Error
	return one(&link, err)
}

// FindByToken resolves a token to its order. Revoked tokens are not found —
// to the public page a revoked link and a made-up one look the same.
func (r *TrackingLinkRepository) FindByToken(ctx context.Context, token string) (*models.TrackingLink, error) {
	var link models.TrackingLink
	err := r.db.WithContext(ctx).
		Where("token = ? AND revoked_at IS NULL", token).
		First(&link).Error
	return one(&link, err)
}

func (r *TrackingLinkRepository) Create(ctx context.Context, link *models.TrackingLink) error {
	return r.db.WithContext(ctx).Create(link).Error
}

// Revoke stamps every live link of the order. Used when a transporter wants a
// shared link to stop working.
func (r *TrackingLinkRepository) Revoke(ctx context.Context, orderID uuid.UUID) error {
	now := time.Now()
	return r.db.WithContext(ctx).Model(&models.TrackingLink{}).
		Where("order_id = ? AND revoked_at IS NULL", orderID).
		Update("revoked_at", now).Error
}
