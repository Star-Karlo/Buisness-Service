package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// JSONB is a free-form JSONB column.
type JSONB map[string]interface{}

func (j JSONB) Value() (driver.Value, error) {
	if j == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(j)
}

func (j *JSONB) Scan(src interface{}) error {
	if src == nil {
		*j = JSONB{}
		return nil
	}
	b, ok := src.([]byte)
	if !ok {
		return fmt.Errorf("models: cannot scan %T into JSONB", src)
	}
	return json.Unmarshal(b, j)
}

// Money is the type for every monetary amount.
//
// decimal.Decimal rather than float64: freight prices are multiplied by tax
// percentages and summed across invoice lines, and binary floating point
// cannot represent 0.02 exactly. The legacy system used JavaScript numbers
// throughout, so its invoice totals were subject to exactly that drift.
type Money = decimal.Decimal

// Agreement is a negotiated contract between a shipper and a transporter.
type Agreement struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	LegacyID        *string   `gorm:"column:legacy_id" json:"legacyId,omitempty"`
	AgreementNumber string    `gorm:"column:agreement_number;not null" json:"agreementNumber"`

	ShipperCompanyID     uuid.UUID `gorm:"type:uuid;not null" json:"shipperCompanyId"`
	TransporterCompanyID uuid.UUID `gorm:"type:uuid;not null" json:"transporterCompanyId"`
	CreatedByUserID      uuid.UUID `gorm:"type:uuid;not null" json:"createdByUserId"`

	StatusCode string `gorm:"column:status_code;not null" json:"statusCode"`
	Status     string `gorm:"-" json:"status"`
	// StatusAlias is derived, never stored; see the note on the status machine.
	StatusAlias string `gorm:"-" json:"statusAlias"`

	ValidFrom  time.Time `gorm:"type:date;not null" json:"validFrom"`
	ValidUntil time.Time `gorm:"type:date;not null" json:"validUntil"`

	Verified         bool       `gorm:"not null;default:false" json:"verified"`
	VerifiedByUserID *uuid.UUID `gorm:"type:uuid" json:"verifiedByUserId,omitempty"`
	VerifiedAt       *time.Time `json:"verifiedAt,omitempty"`

	PaymentTypeID *string `gorm:"column:payment_type_id" json:"paymentTypeId,omitempty"`
	CurrencyID    *string `gorm:"column:currency_id" json:"currencyId,omitempty"`

	Detail JSONB `gorm:"type:jsonb" json:"detail"`

	Rates []AgreementRate `gorm:"foreignKey:AgreementID" json:"rates,omitempty"`

	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

func (Agreement) TableName() string { return "agreements" }

// AfterFind fills the derived display fields.
func (a *Agreement) AfterFind(*gorm.DB) error {
	a.Status = StatusLabel(DomainAgreement, a.StatusCode)
	a.StatusAlias = StatusAlias(DomainAgreement, a.StatusCode)
	return nil
}

// IsUsable reports whether an order may be placed against this agreement now.
// strictVerification reflects the ordering company's
// active_agreement_verified_only setting.
func (a *Agreement) IsUsable(now time.Time, strictVerification bool) bool {
	if a.StatusCode != AgreementActive {
		return false
	}
	if strictVerification && !a.Verified {
		return false
	}
	// Dates are inclusive at both ends: an agreement valid until the 31st can
	// still be used on the 31st.
	return !now.Before(a.ValidFrom) && !now.After(a.ValidUntil.Add(24*time.Hour-time.Nanosecond))
}

// AgreementRate is one price line of an agreement.
type AgreementRate struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	AgreementID uuid.UUID `gorm:"type:uuid;not null" json:"agreementId"`

	OriginWarehouseID      *string `gorm:"column:origin_warehouse_id" json:"originWarehouseId,omitempty"`
	DestinationWarehouseID *string `gorm:"column:destination_warehouse_id" json:"destinationWarehouseId,omitempty"`
	OriginCityID           *string `gorm:"column:origin_city_id" json:"originCityId,omitempty"`
	DestinationCityID      *string `gorm:"column:destination_city_id" json:"destinationCityId,omitempty"`
	TruckTypeID            *string `gorm:"column:truck_type_id" json:"truckTypeId,omitempty"`

	PricingTypeID *string `gorm:"column:pricing_type_id" json:"pricingTypeId,omitempty"`
	Price         Money   `gorm:"type:numeric(18,2);not null" json:"price"`
	MinQuantity   *Money  `gorm:"type:numeric(12,2)" json:"minQuantity,omitempty"`
	CurrencyID    *string `gorm:"column:currency_id" json:"currencyId,omitempty"`

	LeadTimeHours *int      `json:"leadTimeHours,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

func (AgreementRate) TableName() string { return "agreement_rates" }

// Order kinds.
const (
	OrderKindStandard = "standard"
	OrderKindEmpty    = "empty"
	OrderKindThreePL  = "threepl"
)

// Order is one freight job.
type Order struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	LegacyID    *string   `gorm:"column:legacy_id" json:"legacyId,omitempty"`
	OrderNumber string    `gorm:"column:order_number;not null" json:"orderNumber"`

	AgreementID *uuid.UUID `gorm:"type:uuid" json:"agreementId,omitempty"`

	ShipperCompanyID     uuid.UUID  `gorm:"type:uuid;not null" json:"shipperCompanyId"`
	TransporterCompanyID *uuid.UUID `gorm:"type:uuid" json:"transporterCompanyId,omitempty"`
	CreatedByUserID      uuid.UUID  `gorm:"type:uuid;not null" json:"createdByUserId"`
	ParentOrderID        *uuid.UUID `gorm:"type:uuid" json:"parentOrderId,omitempty"`

	OrderKind string `gorm:"column:order_kind;not null;default:standard" json:"orderKind"`

	StatusCode  string `gorm:"column:status_code;not null" json:"statusCode"`
	Status      string `gorm:"-" json:"status"`
	StatusAlias string `gorm:"-" json:"statusAlias"`

	DriverUserID *uuid.UUID `gorm:"type:uuid" json:"driverUserId,omitempty"`
	// TruckID is a Mongo ObjectId hex string, owned by the master data service.
	TruckID *string `gorm:"column:truck_id" json:"truckId,omitempty"`

	OriginWarehouseID      *string `gorm:"column:origin_warehouse_id" json:"originWarehouseId,omitempty"`
	DestinationWarehouseID *string `gorm:"column:destination_warehouse_id" json:"destinationWarehouseId,omitempty"`

	CargoTypeID *string `gorm:"column:cargo_type_id" json:"cargoTypeId,omitempty"`
	ItemTypeID  *string `gorm:"column:item_type_id" json:"itemTypeId,omitempty"`
	Quantity    *Money  `gorm:"type:numeric(12,2)" json:"quantity,omitempty"`
	WeightKg    *Money  `gorm:"column:weight_kg;type:numeric(12,2)" json:"weightKg,omitempty"`
	VolumeM3    *Money  `gorm:"column:volume_m3;type:numeric(12,2)" json:"volumeM3,omitempty"`

	PickupAt   *time.Time `json:"pickupAt,omitempty"`
	DeliveryAt *time.Time `json:"deliveryAt,omitempty"`

	Price      *Money  `gorm:"type:numeric(18,2)" json:"price,omitempty"`
	CurrencyID *string `gorm:"column:currency_id" json:"currencyId,omitempty"`

	CustomerID      *string `gorm:"column:customer_id" json:"customerId,omitempty"`
	ReferenceNumber *string `gorm:"column:reference_number" json:"referenceNumber,omitempty"`

	Detail JSONB `gorm:"type:jsonb" json:"detail"`

	CancelledAt       *time.Time `json:"cancelledAt,omitempty"`
	CancelledByUserID *uuid.UUID `gorm:"type:uuid" json:"cancelledByUserId,omitempty"`
	CancelReason      *string    `json:"cancelReason,omitempty"`

	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

func (Order) TableName() string { return "orders" }

func (o *Order) AfterFind(*gorm.DB) error {
	o.Status = StatusLabel(DomainOrder, o.StatusCode)
	o.StatusAlias = StatusAlias(DomainOrder, o.StatusCode)
	return nil
}

// InvolvesCompany reports whether a company is a party to this order. It is the
// tenant check every read of a single order goes through.
func (o *Order) InvolvesCompany(companyID uuid.UUID) bool {
	if o.ShipperCompanyID == companyID {
		return true
	}
	return o.TransporterCompanyID != nil && *o.TransporterCompanyID == companyID
}

// OrderStatusEntry is one recorded status change.
type OrderStatusEntry struct {
	ID              int64      `gorm:"primaryKey" json:"id"`
	OrderID         uuid.UUID  `gorm:"type:uuid;not null" json:"orderId"`
	FromStatusCode  *string    `gorm:"column:from_status_code" json:"fromStatusCode,omitempty"`
	ToStatusCode    string     `gorm:"column:to_status_code;not null" json:"toStatusCode"`
	ChangedByUserID *uuid.UUID `gorm:"type:uuid" json:"changedByUserId,omitempty"`
	Source          string     `gorm:"not null;default:user" json:"source"`
	Note            *string    `json:"note,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
}

func (OrderStatusEntry) TableName() string { return "order_status_history" }

// Status change sources.
const (
	SourceUser        = "user"
	SourceSystem      = "system"
	SourceCron        = "cron"
	SourceGeofence    = "geofence"
	SourceIntegration = "integration"
)

// Shipment is one physical execution of an order.
type Shipment struct {
	ID      uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrderID uuid.UUID `gorm:"type:uuid;not null" json:"orderId"`

	DriverUserID *uuid.UUID `gorm:"type:uuid" json:"driverUserId,omitempty"`
	TruckID      *string    `gorm:"column:truck_id" json:"truckId,omitempty"`

	StatusCode  string `gorm:"column:status_code;not null" json:"statusCode"`
	Status      string `gorm:"-" json:"status"`
	StatusAlias string `gorm:"-" json:"statusAlias"`

	StartedToLoadingAt   *time.Time `json:"startedToLoadingAt,omitempty"`
	ArrivedLoadingAt     *time.Time `json:"arrivedLoadingAt,omitempty"`
	LoadingStartedAt     *time.Time `json:"loadingStartedAt,omitempty"`
	LoadingFinishedAt    *time.Time `json:"loadingFinishedAt,omitempty"`
	StartedToUnloadingAt *time.Time `json:"startedToUnloadingAt,omitempty"`
	ArrivedUnloadingAt   *time.Time `json:"arrivedUnloadingAt,omitempty"`
	UnloadingStartedAt   *time.Time `json:"unloadingStartedAt,omitempty"`
	UnloadingFinishedAt  *time.Time `json:"unloadingFinishedAt,omitempty"`
	FinishedAt           *time.Time `json:"finishedAt,omitempty"`

	LoadingLatitude    *float64 `gorm:"type:numeric(10,7)" json:"loadingLatitude,omitempty"`
	LoadingLongitude   *float64 `gorm:"type:numeric(10,7)" json:"loadingLongitude,omitempty"`
	UnloadingLatitude  *float64 `gorm:"type:numeric(10,7)" json:"unloadingLatitude,omitempty"`
	UnloadingLongitude *float64 `gorm:"type:numeric(10,7)" json:"unloadingLongitude,omitempty"`

	LoadingWithinGeofence   *bool `json:"loadingWithinGeofence,omitempty"`
	UnloadingWithinGeofence *bool `json:"unloadingWithinGeofence,omitempty"`

	DistanceMeters *int   `json:"distanceMeters,omitempty"`
	TollCost       *Money `gorm:"type:numeric(18,2)" json:"tollCost,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (Shipment) TableName() string { return "shipments" }

func (s *Shipment) AfterFind(*gorm.DB) error {
	s.Status = StatusLabel(DomainShipment, s.StatusCode)
	s.StatusAlias = StatusAlias(DomainShipment, s.StatusCode)
	return nil
}

// ShipmentDocument is a POD or checklist attached to a shipment.
type ShipmentDocument struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	ShipmentID uuid.UUID `gorm:"type:uuid;not null" json:"shipmentId"`

	DocType string  `gorm:"column:doc_type;not null" json:"docType"`
	Stage   *string `json:"stage,omitempty"`
	FileURL string  `gorm:"column:file_url;not null" json:"fileUrl"`

	UploadedByUserID *uuid.UUID `gorm:"type:uuid" json:"uploadedByUserId,omitempty"`
	VerifiedAt       *time.Time `json:"verifiedAt,omitempty"`
	VerifiedByUserID *uuid.UUID `gorm:"type:uuid" json:"verifiedByUserId,omitempty"`

	Metadata  JSONB     `gorm:"type:jsonb" json:"metadata,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

func (ShipmentDocument) TableName() string { return "shipment_documents" }

// Invoice is a freight bill covering one or more orders.
type Invoice struct {
	ID            uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	LegacyID      *string   `gorm:"column:legacy_id" json:"legacyId,omitempty"`
	InvoiceNumber string    `gorm:"column:invoice_number;not null" json:"invoiceNumber"`

	ShipperCompanyID     uuid.UUID `gorm:"type:uuid;not null" json:"shipperCompanyId"`
	TransporterCompanyID uuid.UUID `gorm:"type:uuid;not null" json:"transporterCompanyId"`

	StatusCode  string `gorm:"column:status_code;not null" json:"statusCode"`
	Status      string `gorm:"-" json:"status"`
	StatusAlias string `gorm:"-" json:"statusAlias"`

	Subtotal        Money   `gorm:"type:numeric(18,2);not null" json:"subtotal"`
	PPNPercentage   Money   `gorm:"column:ppn_percentage;type:numeric(6,4);not null" json:"ppnPercentage"`
	PPNAmount       Money   `gorm:"column:ppn_amount;type:numeric(18,2);not null" json:"ppnAmount"`
	PPH23Percentage Money   `gorm:"column:pph23_percentage;type:numeric(6,4);not null" json:"pph23Percentage"`
	PPH23Amount     Money   `gorm:"column:pph23_amount;type:numeric(18,2);not null" json:"pph23Amount"`
	Adjustment      Money   `gorm:"type:numeric(18,2);not null" json:"adjustment"`
	Total           Money   `gorm:"type:numeric(18,2);not null" json:"total"`
	CurrencyID      *string `gorm:"column:currency_id" json:"currencyId,omitempty"`

	IssuedAt    *time.Time `json:"issuedAt,omitempty"`
	DueAt       *time.Time `json:"dueAt,omitempty"`
	SubmittedAt *time.Time `json:"submittedAt,omitempty"`
	VerifiedAt  *time.Time `json:"verifiedAt,omitempty"`
	PaidAt      *time.Time `json:"paidAt,omitempty"`
	CancelledAt *time.Time `json:"cancelledAt,omitempty"`

	Notes  *string `json:"notes,omitempty"`
	Detail JSONB   `gorm:"type:jsonb" json:"detail"`

	Lines []InvoiceLine `gorm:"foreignKey:InvoiceID" json:"lines,omitempty"`

	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
}

func (Invoice) TableName() string { return "invoices" }

func (i *Invoice) AfterFind(*gorm.DB) error {
	i.Status = StatusLabel(DomainInvoice, i.StatusCode)
	i.StatusAlias = StatusAlias(DomainInvoice, i.StatusCode)
	return nil
}

// Recalculate recomputes the tax amounts and total from the subtotal.
//
// PPN is added to the bill; PPH23 is withheld from it. Getting that sign wrong
// is the single most consequential arithmetic error available here, so the two
// are computed in one place and tested.
func (i *Invoice) Recalculate() {
	i.PPNAmount = i.Subtotal.Mul(i.PPNPercentage).Round(2)
	i.PPH23Amount = i.Subtotal.Mul(i.PPH23Percentage).Round(2)
	i.Total = i.Subtotal.
		Add(i.PPNAmount).
		Sub(i.PPH23Amount).
		Add(i.Adjustment).
		Round(2)
}

// InvoiceLine bills one order.
type InvoiceLine struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	InvoiceID uuid.UUID `gorm:"type:uuid;not null" json:"invoiceId"`
	OrderID   uuid.UUID `gorm:"type:uuid;not null" json:"orderId"`

	Description *string `json:"description,omitempty"`
	Quantity    Money   `gorm:"type:numeric(12,2);not null" json:"quantity"`
	UnitPrice   Money   `gorm:"column:unit_price;type:numeric(18,2);not null" json:"unitPrice"`
	Amount      Money   `gorm:"type:numeric(18,2);not null" json:"amount"`

	CreatedAt time.Time `json:"createdAt"`
}

func (InvoiceLine) TableName() string { return "invoice_lines" }

// Rating is one party's score of another after an order.
type Rating struct {
	ID      uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrderID uuid.UUID `gorm:"type:uuid;not null" json:"orderId"`

	RatedUserID   uuid.UUID `gorm:"type:uuid;not null" json:"ratedUserId"`
	RatedByUserID uuid.UUID `gorm:"type:uuid;not null" json:"ratedByUserId"`
	Score         int16     `gorm:"not null" json:"score"`
	Comment       *string   `json:"comment,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
}

func (Rating) TableName() string { return "ratings" }
