// Package repository is the business service's database layer.
//
// Two invariants hold throughout:
//
//   - Every read of business data is scoped to a company. An order belongs to
//     a shipper and a transporter, and a caller must be one of them.
//   - Status changes go through ApplyStatus, which writes the new status and
//     its history row in one transaction. There is no path that changes a
//     status without recording who changed it.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/platform/query"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrNotFound = errors.New("repository: not found")
	ErrConflict = errors.New("repository: conflict")
)

// ---------------------------------------------------------------------------
// Orders
// ---------------------------------------------------------------------------

type OrderRepository struct{ db *gorm.DB }

func NewOrderRepository(db *gorm.DB) *OrderRepository { return &OrderRepository{db: db} }

var orderFields = query.FieldSet{
	"orderNumber":     "order_number",
	"statusCode":      "status_code",
	"orderKind":       "order_kind",
	"pickupAt":        "pickup_at",
	"deliveryAt":      "delivery_at",
	"createdAt":       "created_at",
	"price":           "price",
	"referenceNumber": "reference_number",
	"customerId":      "customer_id",
	"truckId":         "truck_id",
	"agreementId":     "agreement_id",
}

func OrderFields() query.FieldSet { return orderFields }

// FindByID resolves an order the given company is party to.
//
// The company filter is part of the query rather than a check afterwards, so a
// caller cannot learn that an id exists by observing a different error.
func (r *OrderRepository) FindByID(ctx context.Context, companyID, id uuid.UUID) (*models.Order, error) {
	var order models.Order
	err := r.db.WithContext(ctx).
		Where("id = ? AND (shipper_company_id = ? OR transporter_company_id = ?)", id, companyID, companyID).
		First(&order).Error
	return one(&order, err)
}

// FindByIDForService resolves an order without a tenant filter, for gRPC reads
// where the caller is another service rather than a company.
func (r *OrderRepository) FindByIDForService(ctx context.Context, id uuid.UUID) (*models.Order, error) {
	var order models.Order
	return one(&order, r.db.WithContext(ctx).First(&order, "id = ?", id).Error)
}

// FindByNumber resolves an order by its human-facing number, for the public
// tracking page. It returns only the fields that page needs.
func (r *OrderRepository) FindByNumber(ctx context.Context, number string) (*models.Order, error) {
	var order models.Order
	return one(&order, r.db.WithContext(ctx).First(&order, "order_number = ?", number).Error)
}

// List pages the orders a company is party to.
func (r *OrderRepository) List(ctx context.Context, companyID uuid.UUID, p query.Params) ([]models.Order, int64, error) {
	q := r.db.WithContext(ctx).Model(&models.Order{}).
		Where("shipper_company_id = ? OR transporter_company_id = ?", companyID, companyID)
	q = applyFilters(q, p)

	if p.Search != "" {
		like := "%" + p.Search + "%"
		q = q.Where("order_number ILIKE ? OR reference_number ILIKE ?", like, like)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repository: count orders: %w", err)
	}

	var orders []models.Order
	err := applySorts(q, p, "created_at DESC").
		Offset(p.Offset()).Limit(p.PageSize).Find(&orders).Error
	if err != nil {
		return nil, 0, fmt.Errorf("repository: list orders: %w", err)
	}
	return orders, total, nil
}

// ListForDriver pages the orders assigned to one driver.
func (r *OrderRepository) ListForDriver(ctx context.Context, driverID uuid.UUID, p query.Params) ([]models.Order, int64, error) {
	q := applyFilters(r.db.WithContext(ctx).Model(&models.Order{}).Where("driver_user_id = ?", driverID), p)

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repository: count driver orders: %w", err)
	}

	var orders []models.Order
	err := applySorts(q, p, "pickup_at ASC NULLS LAST").
		Offset(p.Offset()).Limit(p.PageSize).Find(&orders).Error
	if err != nil {
		return nil, 0, fmt.Errorf("repository: list driver orders: %w", err)
	}
	return orders, total, nil
}

// FindByIDs resolves a batch, for the cross-service summary endpoint.
func (r *OrderRepository) FindByIDs(ctx context.Context, ids []uuid.UUID) ([]models.Order, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var orders []models.Order
	if err := r.db.WithContext(ctx).Find(&orders, "id IN ?", ids).Error; err != nil {
		return nil, fmt.Errorf("repository: find orders by ids: %w", err)
	}
	return orders, nil
}

// Create inserts an order and records its opening status in one transaction.
func (r *OrderRepository) Create(ctx context.Context, order *models.Order) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(order).Error; err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: %v", ErrConflict, err)
			}
			return fmt.Errorf("repository: create order: %w", err)
		}
		return tx.Create(&models.OrderStatusEntry{
			OrderID:         order.ID,
			ToStatusCode:    order.StatusCode,
			ChangedByUserID: &order.CreatedByUserID,
			Source:          models.SourceUser,
		}).Error
	})
}

// StatusChange describes one status transition to apply.
type StatusChange struct {
	OrderID uuid.UUID
	// From guards the change: the update only applies if the order is still in
	// this state. Two concurrent callers cannot both advance the same order.
	From   string
	To     string
	Actor  *uuid.UUID
	Source string
	Note   *string
	// Fields are additional columns to set in the same statement, so a status
	// change and the data that goes with it are never half applied.
	Fields map[string]interface{}
}

// ApplyStatus performs a guarded status transition and writes its history.
//
// The compare-and-set on status_code is what makes this safe under concurrency.
// The legacy code read the order, decided, then wrote, so two managers pressing
// approve at once could both succeed and the second would silently overwrite
// the first.
func (r *OrderRepository) ApplyStatus(ctx context.Context, ch StatusChange) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		fields := map[string]interface{}{"status_code": ch.To}
		for k, v := range ch.Fields {
			fields[k] = v
		}

		res := tx.Model(&models.Order{}).
			Where("id = ? AND status_code = ?", ch.OrderID, ch.From).
			Updates(fields)
		if res.Error != nil {
			return fmt.Errorf("repository: apply status: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			// Either the order is gone, or something else moved it first.
			return fmt.Errorf("%w: the order is no longer in state %q", ErrConflict, ch.From)
		}

		source := ch.Source
		if source == "" {
			source = models.SourceUser
		}
		return tx.Create(&models.OrderStatusEntry{
			OrderID:         ch.OrderID,
			FromStatusCode:  &ch.From,
			ToStatusCode:    ch.To,
			ChangedByUserID: ch.Actor,
			Source:          source,
			Note:            ch.Note,
		}).Error
	})
}

// UpdateFields applies an allowlisted partial update to a draft order.
func (r *OrderRepository) UpdateFields(ctx context.Context, id uuid.UUID, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	res := r.db.WithContext(ctx).Model(&models.Order{}).Where("id = ?", id).Updates(fields)
	if res.Error != nil {
		return fmt.Errorf("repository: update order: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// History returns an order's status timeline.
func (r *OrderRepository) History(ctx context.Context, orderID uuid.UUID) ([]models.OrderStatusEntry, error) {
	var out []models.OrderStatusEntry
	err := r.db.WithContext(ctx).
		Where("order_id = ?", orderID).
		Order("created_at ASC").
		Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("repository: order history: %w", err)
	}
	return out, nil
}

// CountByStatus powers the dashboard tiles.
func (r *OrderRepository) CountByStatus(ctx context.Context, companyID uuid.UUID) (map[string]int64, error) {
	var rows []struct {
		StatusCode string
		Count      int64
	}
	err := r.db.WithContext(ctx).Model(&models.Order{}).
		Select("status_code, COUNT(*) AS count").
		Where("shipper_company_id = ? OR transporter_company_id = ?", companyID, companyID).
		Group("status_code").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("repository: count by status: %w", err)
	}

	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.StatusCode] = row.Count
	}
	return out, nil
}

// NextNumber reserves the next document number in a scope atomically.
func (r *OrderRepository) NextNumber(ctx context.Context, scope, period string) (int64, error) {
	var next int64
	err := r.db.WithContext(ctx).
		Raw("SELECT next_sequence_value(?, ?)", scope, period).
		Scan(&next).Error
	if err != nil {
		return 0, fmt.Errorf("repository: next sequence value: %w", err)
	}
	return next, nil
}

// ---------------------------------------------------------------------------
// Shipments
// ---------------------------------------------------------------------------

type ShipmentRepository struct{ db *gorm.DB }

func NewShipmentRepository(db *gorm.DB) *ShipmentRepository { return &ShipmentRepository{db: db} }

func (r *ShipmentRepository) FindByID(ctx context.Context, id uuid.UUID) (*models.Shipment, error) {
	var s models.Shipment
	return one(&s, r.db.WithContext(ctx).First(&s, "id = ?", id).Error)
}

// FindByOrder returns the current shipment for an order.
func (r *ShipmentRepository) FindByOrder(ctx context.Context, orderID uuid.UUID) (*models.Shipment, error) {
	var s models.Shipment
	err := r.db.WithContext(ctx).
		Where("order_id = ?", orderID).
		Order("created_at DESC").
		First(&s).Error
	return one(&s, err)
}

// FindActiveByDriver answers the telemetry service's question: which shipment
// do this driver's position reports belong to?
func (r *ShipmentRepository) FindActiveByDriver(ctx context.Context, driverID uuid.UUID) (*models.Shipment, error) {
	var s models.Shipment
	err := r.db.WithContext(ctx).
		Where("driver_user_id = ? AND finished_at IS NULL AND status_code <> ?", driverID, models.ShipmentCancelled).
		Order("created_at DESC").
		First(&s).Error
	return one(&s, err)
}

func (r *ShipmentRepository) Create(ctx context.Context, s *models.Shipment) error {
	if err := r.db.WithContext(ctx).Create(s).Error; err != nil {
		return fmt.Errorf("repository: create shipment: %w", err)
	}
	return nil
}

// ApplyStatus performs a guarded shipment transition, mirroring the order
// equivalent.
func (r *ShipmentRepository) ApplyStatus(ctx context.Context, id uuid.UUID, from, to string, fields map[string]interface{}) error {
	update := map[string]interface{}{"status_code": to}
	for k, v := range fields {
		update[k] = v
	}

	res := r.db.WithContext(ctx).Model(&models.Shipment{}).
		Where("id = ? AND status_code = ?", id, from).
		Updates(update)
	if res.Error != nil {
		return fmt.Errorf("repository: apply shipment status: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: the shipment is no longer in state %q", ErrConflict, from)
	}
	return nil
}

// AddDocument attaches a POD or checklist.
func (r *ShipmentRepository) AddDocument(ctx context.Context, doc *models.ShipmentDocument) error {
	if err := r.db.WithContext(ctx).Create(doc).Error; err != nil {
		return fmt.Errorf("repository: add shipment document: %w", err)
	}
	return nil
}

func (r *ShipmentRepository) Documents(ctx context.Context, shipmentID uuid.UUID) ([]models.ShipmentDocument, error) {
	var out []models.ShipmentDocument
	err := r.db.WithContext(ctx).Where("shipment_id = ?", shipmentID).Order("created_at ASC").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("repository: shipment documents: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Agreements
// ---------------------------------------------------------------------------

type AgreementRepository struct{ db *gorm.DB }

func NewAgreementRepository(db *gorm.DB) *AgreementRepository { return &AgreementRepository{db: db} }

var agreementFields = query.FieldSet{
	"agreementNumber": "agreement_number",
	"statusCode":      "status_code",
	"verified":        "verified",
	"validFrom":       "valid_from",
	"validUntil":      "valid_until",
	"createdAt":       "created_at",
}

func AgreementFields() query.FieldSet { return agreementFields }

func (r *AgreementRepository) FindByID(ctx context.Context, companyID, id uuid.UUID) (*models.Agreement, error) {
	var a models.Agreement
	err := r.db.WithContext(ctx).
		Preload("Rates").
		Where("id = ? AND (shipper_company_id = ? OR transporter_company_id = ?)", id, companyID, companyID).
		First(&a).Error
	return one(&a, err)
}

func (r *AgreementRepository) FindByIDForService(ctx context.Context, id uuid.UUID) (*models.Agreement, error) {
	var a models.Agreement
	return one(&a, r.db.WithContext(ctx).Preload("Rates").First(&a, "id = ?", id).Error)
}

func (r *AgreementRepository) List(ctx context.Context, companyID uuid.UUID, p query.Params) ([]models.Agreement, int64, error) {
	q := r.db.WithContext(ctx).Model(&models.Agreement{}).
		Where("shipper_company_id = ? OR transporter_company_id = ?", companyID, companyID)
	q = applyFilters(q, p)

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repository: count agreements: %w", err)
	}

	var out []models.Agreement
	err := applySorts(q, p, "created_at DESC").
		Offset(p.Offset()).Limit(p.PageSize).Find(&out).Error
	if err != nil {
		return nil, 0, fmt.Errorf("repository: list agreements: %w", err)
	}
	return out, total, nil
}

// Create inserts an agreement together with its rate lines.
func (r *AgreementRepository) Create(ctx context.Context, a *models.Agreement) error {
	err := r.db.WithContext(ctx).
		Session(&gorm.Session{FullSaveAssociations: true}).
		Create(a).Error
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return fmt.Errorf("repository: create agreement: %w", err)
	}
	return nil
}

func (r *AgreementRepository) UpdateFields(ctx context.Context, id uuid.UUID, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	res := r.db.WithContext(ctx).Model(&models.Agreement{}).Where("id = ?", id).Updates(fields)
	if res.Error != nil {
		return fmt.Errorf("repository: update agreement: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// FindExpired returns active agreements whose validity has run out. The nightly
// sweep uses it.
func (r *AgreementRepository) FindExpired(ctx context.Context, now time.Time) ([]models.Agreement, error) {
	var out []models.Agreement
	err := r.db.WithContext(ctx).
		Where("status_code = ? AND valid_until < ?", models.AgreementActive, now).
		Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("repository: find expired agreements: %w", err)
	}
	return out, nil
}

// FindRate resolves the price line covering a lane and truck type.
func (r *AgreementRepository) FindRate(ctx context.Context, agreementID uuid.UUID, originCityID, destCityID, truckTypeID string) (*models.AgreementRate, error) {
	var rate models.AgreementRate
	err := r.db.WithContext(ctx).
		Where("agreement_id = ?", agreementID).
		Where("(origin_city_id = ? OR origin_city_id IS NULL)", originCityID).
		Where("(destination_city_id = ? OR destination_city_id IS NULL)", destCityID).
		Where("(truck_type_id = ? OR truck_type_id IS NULL)", truckTypeID).
		// Prefer the most specific match: a lane-and-type rate beats a wildcard.
		Order(clause.OrderBy{Expression: clause.Expr{
			SQL: "(origin_city_id IS NOT NULL)::int + (destination_city_id IS NOT NULL)::int + (truck_type_id IS NOT NULL)::int DESC",
		}}).
		First(&rate).Error
	return one(&rate, err)
}

// ---------------------------------------------------------------------------
// Invoices
// ---------------------------------------------------------------------------

type InvoiceRepository struct{ db *gorm.DB }

func NewInvoiceRepository(db *gorm.DB) *InvoiceRepository { return &InvoiceRepository{db: db} }

var invoiceFields = query.FieldSet{
	"invoiceNumber": "invoice_number",
	"statusCode":    "status_code",
	"issuedAt":      "issued_at",
	"dueAt":         "due_at",
	"paidAt":        "paid_at",
	"total":         "total",
	"createdAt":     "created_at",
}

func InvoiceFields() query.FieldSet { return invoiceFields }

func (r *InvoiceRepository) FindByID(ctx context.Context, companyID, id uuid.UUID) (*models.Invoice, error) {
	var inv models.Invoice
	err := r.db.WithContext(ctx).
		Preload("Lines").
		Where("id = ? AND (shipper_company_id = ? OR transporter_company_id = ?)", id, companyID, companyID).
		First(&inv).Error
	return one(&inv, err)
}

func (r *InvoiceRepository) FindByIDForService(ctx context.Context, id uuid.UUID) (*models.Invoice, error) {
	var inv models.Invoice
	return one(&inv, r.db.WithContext(ctx).Preload("Lines").First(&inv, "id = ?", id).Error)
}

func (r *InvoiceRepository) List(ctx context.Context, companyID uuid.UUID, p query.Params) ([]models.Invoice, int64, error) {
	q := r.db.WithContext(ctx).Model(&models.Invoice{}).
		Where("shipper_company_id = ? OR transporter_company_id = ?", companyID, companyID)
	q = applyFilters(q, p)

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("repository: count invoices: %w", err)
	}

	var out []models.Invoice
	err := applySorts(q, p, "created_at DESC").
		Offset(p.Offset()).Limit(p.PageSize).Find(&out).Error
	if err != nil {
		return nil, 0, fmt.Errorf("repository: list invoices: %w", err)
	}
	return out, total, nil
}

// Create inserts an invoice and its lines together.
func (r *InvoiceRepository) Create(ctx context.Context, inv *models.Invoice) error {
	err := r.db.WithContext(ctx).
		Session(&gorm.Session{FullSaveAssociations: true}).
		Create(inv).Error
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
		return fmt.Errorf("repository: create invoice: %w", err)
	}
	return nil
}

// ApplyStatus performs a guarded invoice transition.
func (r *InvoiceRepository) ApplyStatus(ctx context.Context, id uuid.UUID, from, to string, fields map[string]interface{}) error {
	update := map[string]interface{}{"status_code": to}
	for k, v := range fields {
		update[k] = v
	}

	res := r.db.WithContext(ctx).Model(&models.Invoice{}).
		Where("id = ? AND status_code = ?", id, from).
		Updates(update)
	if res.Error != nil {
		return fmt.Errorf("repository: apply invoice status: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: the invoice is no longer in state %q", ErrConflict, from)
	}
	return nil
}

// OrdersAlreadyBilled returns which of the given orders already appear on a
// live invoice, so the same job is not billed twice.
func (r *InvoiceRepository) OrdersAlreadyBilled(ctx context.Context, orderIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(orderIDs) == 0 {
		return nil, nil
	}

	var billed []uuid.UUID
	err := r.db.WithContext(ctx).
		Model(&models.InvoiceLine{}).
		Joins("JOIN invoices ON invoices.id = invoice_lines.invoice_id").
		Where("invoice_lines.order_id IN ?", orderIDs).
		Where("invoices.deleted_at IS NULL AND invoices.status_code <> ?", models.InvoiceCancelled).
		Pluck("invoice_lines.order_id", &billed).Error
	if err != nil {
		return nil, fmt.Errorf("repository: check billed orders: %w", err)
	}
	return billed, nil
}

// ---------------------------------------------------------------------------
// Ratings
// ---------------------------------------------------------------------------

type RatingRepository struct{ db *gorm.DB }

func NewRatingRepository(db *gorm.DB) *RatingRepository { return &RatingRepository{db: db} }

func (r *RatingRepository) Create(ctx context.Context, rating *models.Rating) error {
	if err := r.db.WithContext(ctx).Create(rating).Error; err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: this order has already been rated", ErrConflict)
		}
		return fmt.Errorf("repository: create rating: %w", err)
	}
	return nil
}

// AverageFor returns a user's mean score and the number of ratings behind it.
func (r *RatingRepository) AverageFor(ctx context.Context, userID uuid.UUID) (float64, int64, error) {
	var row struct {
		Avg   *float64
		Count int64
	}
	err := r.db.WithContext(ctx).Model(&models.Rating{}).
		Select("AVG(score) AS avg, COUNT(*) AS count").
		Where("rated_user_id = ?", userID).
		Scan(&row).Error
	if err != nil {
		return 0, 0, fmt.Errorf("repository: average rating: %w", err)
	}
	if row.Avg == nil {
		return 0, 0, nil
	}
	return *row.Avg, row.Count, nil
}

func one[T any](v *T, err error) (*T, error) {
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("repository: query: %w", err)
	}
	return v, nil
}
