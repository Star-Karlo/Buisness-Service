package repository

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/karlo/business-service/internal/models"
)

// OrderStopRepository stores the points a journey must visit.
type OrderStopRepository struct{ db *gorm.DB }

func NewOrderStopRepository(db *gorm.DB) *OrderStopRepository {
	return &OrderStopRepository{db: db}
}

// Replace writes the stop list for an order, discarding any previous one.
//
// Replace rather than merge: the list comes from the order's own points, so a
// re-plan that changes them should leave no orphan from the old route. Visits
// already recorded are lost with it, which is correct — a stop that is no
// longer on the journey has no arrival to remember.
func (r *OrderStopRepository) Replace(ctx context.Context, orderID uuid.UUID, stops []models.OrderStop) error {
	if len(stops) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("order_id = ?", orderID).Delete(&models.OrderStop{}).Error; err != nil {
			return err
		}
		for i := range stops {
			stops[i].OrderID = orderID
		}
		return tx.Create(&stops).Error
	})
}

// ListByOrder returns a journey's stops in visit order.
func (r *OrderStopRepository) ListByOrder(ctx context.Context, orderID uuid.UUID) ([]models.OrderStop, error) {
	var rows []models.OrderStop
	err := r.db.WithContext(ctx).
		Where("order_id = ?", orderID).
		Order("seq ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("repository: list order stops: %w", err)
	}
	return rows, nil
}

// UpdateFields records progress at one stop.
func (r *OrderStopRepository) UpdateFields(ctx context.Context, id uuid.UUID, fields map[string]interface{}) error {
	fields["updated_at"] = gorm.Expr("NOW()")
	if err := r.db.WithContext(ctx).Model(&models.OrderStop{}).
		Where("id = ?", id).Updates(fields).Error; err != nil {
		return fmt.Errorf("repository: update order stop: %w", err)
	}
	return nil
}

// Upsert stores one stop, keyed by its position in the journey. Used where a
// caller edits a single stop rather than the whole list.
func (r *OrderStopRepository) Upsert(ctx context.Context, stop *models.OrderStop) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "order_id"}, {Name: "seq"}},
		DoUpdates: clause.AssignmentColumns([]string{"kind", "warehouse_id", "pic_name", "pic_phone", "updated_at"}),
	}).Create(stop).Error
}
