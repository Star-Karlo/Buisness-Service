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

type AllowanceRepository struct{ db *gorm.DB }

func NewAllowanceRepository(db *gorm.DB) *AllowanceRepository {
	return &AllowanceRepository{db: db}
}

func (r *AllowanceRepository) FindByOrder(ctx context.Context, orderID uuid.UUID) (*models.OrderAllowance, error) {
	var row models.OrderAllowance
	err := r.db.WithContext(ctx).Where("order_id = ?", orderID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &row, err
}

// Save writes the advance and records the version it replaced, in one
// transaction.
//
// The history row is written from what is currently STORED, before the update,
// not from what the caller sends. Writing the new value to history instead
// would produce a log of what things became with no record of what they were,
// which is the shape the legacy system had and the reason "why was this driver
// paid more than the sheet says" had no answer.
func (r *AllowanceRepository) Save(ctx context.Context, a *models.OrderAllowance, reason string, actor uuid.UUID) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing models.OrderAllowance
		err := tx.Where("order_id = ?", a.OrderID).First(&existing).Error

		switch {
		case err == nil:
			if existing.FinalisedAt != nil {
				// A finalised advance has been committed to the driver.
				// Changing it is a revision with a reason, never a silent
				// overwrite.
				if reason == "" {
					return ErrConflict
				}
			}
			revision := models.OrderAllowanceRevision{
				OrderID:    existing.OrderID,
				Components: existing.Components,
				Total:      existing.Total,
			}
			if reason != "" {
				revision.Reason = &reason
			}
			if actor != uuid.Nil {
				revision.ChangedByUserID = &actor
			}
			if err := tx.Create(&revision).Error; err != nil {
				return err
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
			// First entry. Nothing to supersede.
		default:
			return err
		}

		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "order_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"haul_distance_meters", "approach_distance_meters", "toll_estimate",
				"components", "total", "currency_id", "note",
				"entered_by_user_id", "entered_at", "updated_at",
			}),
		}).Create(a).Error
	})
}

// Finalise commits the advance to the driver.
func (r *AllowanceRepository) Finalise(ctx context.Context, orderID, actor uuid.UUID) error {
	now := time.Now()
	res := r.db.WithContext(ctx).Model(&models.OrderAllowance{}).
		Where("order_id = ? AND finalised_at IS NULL", orderID).
		Updates(map[string]interface{}{
			"finalised_at":         now,
			"finalised_by_user_id": actor,
			"updated_at":           now,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		// Either there is no advance or it is already finalised. Both are a
		// conflict rather than a not-found: the caller asked to close
		// something that is not open.
		return ErrConflict
	}
	return nil
}

func (r *AllowanceRepository) History(ctx context.Context, orderID uuid.UUID) ([]models.OrderAllowanceRevision, error) {
	var rows []models.OrderAllowanceRevision
	err := r.db.WithContext(ctx).
		Where("order_id = ?", orderID).Order("changed_at DESC").
		Find(&rows).Error
	return rows, err
}

// ---------------------------------------------------------------------------
// Handover codes
// ---------------------------------------------------------------------------

type HandoverRepository struct{ db *gorm.DB }

func NewHandoverRepository(db *gorm.DB) *HandoverRepository {
	return &HandoverRepository{db: db}
}

// Issue stores a new code, retiring any live one for the same shipment stage.
//
// Retiring rather than reusing: a driver who asks for a new code because the
// PIC never received the first must not find that the old code still works.
// The partial unique index on (shipment, stage) WHERE verified_at IS NULL is
// what enforces one live code, so the old row is deleted rather than left.
func (r *HandoverRepository) Issue(ctx context.Context, h *models.ShipmentHandover) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("shipment_id = ? AND stage = ? AND verified_at IS NULL",
			h.ShipmentID, h.Stage).Delete(&models.ShipmentHandover{}).Error; err != nil {
			return err
		}
		return tx.Create(h).Error
	})
}

// FindLive returns the unverified code for a shipment stage.
func (r *HandoverRepository) FindLive(ctx context.Context, shipmentID uuid.UUID, stage string) (*models.ShipmentHandover, error) {
	var row models.ShipmentHandover
	err := r.db.WithContext(ctx).
		Where("shipment_id = ? AND stage = ? AND verified_at IS NULL", shipmentID, stage).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &row, err
}

// IsVerified says whether a stage's handover code has been confirmed.
func (r *HandoverRepository) IsVerified(ctx context.Context, shipmentID uuid.UUID, stage string) (bool, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&models.ShipmentHandover{}).
		Where("shipment_id = ? AND stage = ? AND verified_at IS NOT NULL", shipmentID, stage).
		Count(&n).Error
	return n > 0, err
}

// FindByFieldToken resolves the PIC's field link.
func (r *HandoverRepository) FindByFieldToken(ctx context.Context, token string) (*models.ShipmentHandover, error) {
	var row models.ShipmentHandover
	err := r.db.WithContext(ctx).Where("field_token = ?", token).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &row, err
}

// LatestForStage returns the most recent handover for a stage, verified or
// not. FindLive answers the driver's question ("is there a code I can use");
// this answers Web-Field's ("what code was issued for this delivery"), which
// must still resolve after the driver has confirmed it.
func (r *HandoverRepository) LatestForStage(ctx context.Context, shipmentID uuid.UUID, stage string) (*models.ShipmentHandover, error) {
	var row models.ShipmentHandover
	err := r.db.WithContext(ctx).
		Where("shipment_id = ? AND stage = ?", shipmentID, stage).
		Order("sent_at DESC").First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &row, err
}

// RecordFieldAttempt counts a wrong code typed on Web-Field, separately from
// the driver's attempts: a PIC fumbling the digits must not use up the
// driver's allowance, and vice versa.
func (r *HandoverRepository) RecordFieldAttempt(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).Model(&models.ShipmentHandover{}).
		Where("id = ?", id).
		UpdateColumn("field_attempts", gorm.Expr("field_attempts + 1")).Error
}

// MarkFieldVerified opens the PIC's session. Repeatable: the PIC may close the
// page and come back with the same code while the window is open.
func (r *HandoverRepository) MarkFieldVerified(ctx context.Context, id uuid.UUID, picUserID *uuid.UUID) error {
	fields := map[string]interface{}{"field_verified_at": time.Now()}
	if picUserID != nil {
		fields["field_pic_user_id"] = *picUserID
	}
	return r.db.WithContext(ctx).Model(&models.ShipmentHandover{}).
		Where("id = ?", id).Updates(fields).Error
}

// SetPICName fills the PIC's name in when the field page learns it.
func (r *HandoverRepository) SetPICName(ctx context.Context, id uuid.UUID, name string) error {
	return r.db.WithContext(ctx).Model(&models.ShipmentHandover{}).Where("id = ?", id).UpdateColumn("pic_name", name).Error
}

// RecordAttempt increments the failure counter.
//
// Counted in the database rather than in memory because the driver app retries
// across requests and may hit a different instance each time. An in-process
// counter would make the cap advisory, and a six-digit code with an advisory
// cap is guessable in an afternoon.
func (r *HandoverRepository) RecordAttempt(ctx context.Context, id uuid.UUID) error {
	return r.db.WithContext(ctx).Model(&models.ShipmentHandover{}).
		Where("id = ?", id).
		UpdateColumn("attempts", gorm.Expr("attempts + 1")).Error
}

// MarkVerified closes a code, recording where the driver was.
func (r *HandoverRepository) MarkVerified(ctx context.Context, id uuid.UUID, lat, lon *models.Money, withinGeofence *bool) error {
	now := time.Now()
	return r.db.WithContext(ctx).Model(&models.ShipmentHandover{}).
		Where("id = ? AND verified_at IS NULL", id).
		Updates(map[string]interface{}{
			"verified_at":              now,
			"verified_lat":             lat,
			"verified_lon":             lon,
			"verified_within_geofence": withinGeofence,
		}).Error
}

// AllowanceListRow is one order as the Uang Sangu screen shows it: the order's
// own identifying fields with its advance alongside, whether or not one has
// been entered yet.
type AllowanceListRow struct {
	OrderID                uuid.UUID  `json:"orderId"`
	OrderNumber            string     `json:"orderNumber"`
	StatusCode             string     `json:"statusCode"`
	CustomerID             *string    `json:"customerId,omitempty"`
	OriginWarehouseID      *string    `json:"originWarehouseId,omitempty"`
	DestinationWarehouseID *string    `json:"destinationWarehouseId,omitempty"`
	TruckID                *string    `json:"truckId,omitempty"`
	PickupAt               *time.Time `json:"pickupAt,omitempty"`

	// Nil until somebody has entered an advance.
	Total       *models.Money `json:"total,omitempty"`
	EnteredAt   *time.Time    `json:"enteredAt,omitempty"`
	FinalisedAt *time.Time    `json:"finalisedAt,omitempty"`
}

// ListByCompany pages through a company's orders with their advances.
//
// One query with a LEFT JOIN rather than an allowance per order from the
// browser: the screen lists twenty orders, and twenty round trips to render one
// page is the shape that made the legacy list take four seconds.
//
// Orders without an advance are INCLUDED. The screen's job is to show which
// orders still need one, and a list that only showed entered advances would
// hide exactly the rows somebody has to act on.
//
// `state` narrows: "pending" is entered but not finalised, "final" is
// finalised, "none" is no advance yet, "" is everything.
func (r *AllowanceRepository) ListByCompany(ctx context.Context, companyID uuid.UUID, state string, offset, limit int) ([]AllowanceListRow, int64, error) {
	base := r.db.WithContext(ctx).
		Table("orders o").
		Joins("LEFT JOIN order_allowances a ON a.order_id = o.id").
		Where("o.deleted_at IS NULL").
		Where("(o.shipper_company_id = ? OR o.transporter_company_id = ?)", companyID, companyID).
		// An advance is for a truck that is going somewhere. Drafts and
		// cancelled orders never need one.
		Where("o.status_code IN ?", []string{
			models.OrderReadyToPlan, models.OrderAssigned, models.OrderInTransit,
			models.OrderDelivered, models.OrderCompleted,
		})

	switch state {
	case "pending":
		base = base.Where("a.order_id IS NOT NULL AND a.finalised_at IS NULL")
	case "final":
		base = base.Where("a.finalised_at IS NOT NULL")
	case "none":
		base = base.Where("a.order_id IS NULL")
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var rows []AllowanceListRow
	err := base.
		Select(`o.id AS order_id, o.order_number, o.status_code, o.customer_id,
		        o.origin_warehouse_id, o.destination_warehouse_id, o.truck_id, o.pickup_at,
		        a.total, a.entered_at, a.finalised_at`).
		// Unfinalised first, then by pickup: what needs attention floats up,
		// and within that the truck leaving soonest comes first.
		Order("a.finalised_at NULLS FIRST, o.pickup_at ASC NULLS LAST").
		Offset(offset).Limit(limit).
		Scan(&rows).Error
	return rows, total, err
}
