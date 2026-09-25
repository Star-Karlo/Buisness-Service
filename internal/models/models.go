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

	// Versioning. An agreement is a priced contract, and amending one in place
	// loses what the price WAS when work was done — so each amendment is a new
	// row and the lineage is what ties them together.
	Version int `json:"version"`
	// RootAgreementID is the first version of this lineage; every version
	// shares it. Self-referential on version 1.
	RootAgreementID uuid.UUID `gorm:"column:root_agreement_id;type:uuid" json:"rootAgreementId"`
	// SupersedesAgreementID is the immediate predecessor. The root answers
	// "show me the history"; this answers "what did this version change".
	SupersedesAgreementID *uuid.UUID `gorm:"column:supersedes_agreement_id;type:uuid" json:"supersedesAgreementId,omitempty"`

	RevisionKind *string `gorm:"column:revision_kind" json:"revisionKind,omitempty"`
	RevisionNote *string `gorm:"column:revision_note" json:"revisionNote,omitempty"`

	// Three different people on an approved update: the author, the requester
	// and the approver. Collapsing any two loses the separation the approval
	// exists to create.
	RequestedByUserID *uuid.UUID `gorm:"column:requested_by_user_id;type:uuid" json:"requestedByUserId,omitempty"`
	RequestedAt       *time.Time `gorm:"column:requested_at" json:"requestedAt,omitempty"`
	ApprovedByUserID  *uuid.UUID `gorm:"column:approved_by_user_id;type:uuid" json:"approvedByUserId,omitempty"`
	ApprovedAt        *time.Time `gorm:"column:approved_at" json:"approvedAt,omitempty"`
	RejectedByUserID  *uuid.UUID `gorm:"column:rejected_by_user_id;type:uuid" json:"rejectedByUserId,omitempty"`
	RejectedAt        *time.Time `gorm:"column:rejected_at" json:"rejectedAt,omitempty"`
	DecisionNote      *string    `gorm:"column:decision_note" json:"decisionNote,omitempty"`

	// SupersededAt, with ApprovedAt, answers "which version was live on date
	// X" — the question an invoice dispute asks.
	SupersededAt *time.Time `gorm:"column:superseded_at" json:"supersededAt,omitempty"`

	// The display names behind those two ids, resolved from the authentication
	// service and stored nowhere. A list rendered from the ids alone shows a
	// column of UUIDs, which is worse than showing nothing — and the client
	// cannot resolve them itself without a call per row.
	ShipperCompanyName     string `gorm:"-" json:"shipperCompanyName,omitempty"`
	TransporterCompanyName string `gorm:"-" json:"transporterCompanyName,omitempty"`

	CreatedByUserID uuid.UUID `gorm:"type:uuid;not null" json:"createdByUserId"`

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

	// Customers is who this agreement covers. One row for the ordinary
	// single-client agreement; several for a framework contract.
	Customers []AgreementCustomer `gorm:"foreignKey:AgreementID" json:"customers,omitempty"`

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

// IsUsable reports whether an order may be placed against this agreement
// for a job on the day `on`. strictVerification reflects the ordering
// company's active_agreement_verified_only setting.
//
// `on` is the order's load date, not the moment of booking. An agreement
// that starts next week is the right contract for an order loading next
// week, however early the planner books it — refusing until the start date
// meant the first days of every new contract could not be planned. Judging
// the load date keeps the other side too: a contract that starts after the
// truck loads, or ends before, does not price the order.
func (a *Agreement) IsUsable(on time.Time, strictVerification bool) bool {
	return a.UnusableReason(on, strictVerification) == ""
}

// BusinessZone is the calendar the validity dates are read in. The console
// stores a chosen date as midnight UTC; a planner in Jakarta reading
// "berlaku mulai 23 Sep" expects it to hold from 00:00 WIB, seven hours
// before that instant — so the comparison is made on calendar days here.
var BusinessZone = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Jakarta"); err == nil {
		return loc
	}
	return time.FixedZone("WIB", 7*3600)
}()

// UnusableReason says why an order for the day `on` cannot be placed against
// this agreement, or "" when it can. The reason is the message the planner
// sees, so it names the date rather than just refusing.
func (a *Agreement) UnusableReason(on time.Time, strictVerification bool) string {
	if a.StatusCode != AgreementActive {
		return "status " + StatusLabel(DomainAgreement, a.StatusCode) + " (bukan Aktif)"
	}
	if strictVerification && !a.Verified {
		return "belum diverifikasi"
	}
	// Dates are inclusive at both ends, on the calendar day: an agreement
	// valid until the 31st can still be used for a load on the 31st.
	day := func(t time.Time) time.Time {
		y, m, d := t.In(BusinessZone).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, BusinessZone)
	}
	loadDay := day(on)
	// The stored instants are midnight UTC of the chosen date; read the
	// date they name rather than the instant.
	from := time.Date(a.ValidFrom.UTC().Year(), a.ValidFrom.UTC().Month(), a.ValidFrom.UTC().Day(), 0, 0, 0, 0, BusinessZone)
	until := time.Date(a.ValidUntil.UTC().Year(), a.ValidUntil.UTC().Month(), a.ValidUntil.UTC().Day(), 0, 0, 0, 0, BusinessZone)
	if loadDay.Before(from) {
		return "berlaku mulai " + from.Format("2 Jan 2006") + ", tanggal muat " + loadDay.Format("2 Jan 2006")
	}
	if loadDay.After(until) {
		return "berakhir " + until.Format("2 Jan 2006") + ", tanggal muat " + loadDay.Format("2 Jan 2006")
	}
	return ""
}

// AgreementRate is one price line of an agreement.
type AgreementRate struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	AgreementID uuid.UUID `gorm:"type:uuid;not null" json:"agreementId"`

	OriginWarehouseID      *string `gorm:"column:origin_warehouse_id" json:"originWarehouseId,omitempty"`
	DestinationWarehouseID *string `gorm:"column:destination_warehouse_id" json:"destinationWarehouseId,omitempty"`
	OriginCityID           *string `gorm:"column:origin_city_id" json:"originCityId,omitempty"`
	DestinationCityID      *string `gorm:"column:destination_city_id" json:"destinationCityId,omitempty"`

	// Kecamatan, for the companies that price lanes below city level. NULL for
	// everyone else, and the city columns carry the lane on their own — which
	// is what lets a company turn district granularity on without invalidating
	// the agreements it wrote before.
	OriginDistrictID      *string `gorm:"column:origin_district_id" json:"originDistrictId,omitempty"`
	DestinationDistrictID *string `gorm:"column:destination_district_id" json:"destinationDistrictId,omitempty"`

	// CustomerCompanyID narrows this lane to one client. NULL means every
	// customer the agreement covers, which is the ordinary case.
	CustomerCompanyID *uuid.UUID `gorm:"column:customer_company_id;type:uuid" json:"customerCompanyId,omitempty"`

	// LaneLevel is warehouse, district, city or any. GENERATED by the
	// database from which columns are filled, so it cannot disagree with them —
	// which is why it is read-only here.
	LaneLevel string `gorm:"column:lane_level;->" json:"laneLevel"`

	// The road distance, once MAPID has been asked.
	//
	// Stored rather than computed on read: the rate was agreed against the
	// distance at signing, and a road that changes later must not silently
	// restate what the contract says.
	DistanceMeters *int       `gorm:"column:distance_meters" json:"distanceMeters,omitempty"`
	RouteCacheKey  *string    `gorm:"column:route_cache_key" json:"routeCacheKey,omitempty"`
	RoutedAt       *time.Time `gorm:"column:routed_at" json:"routedAt,omitempty"`

	// RouteStatus says why DistanceMeters is what it is. A city-level lane has
	// no points to route between, and saying so beats an empty column a reader
	// has to guess about.
	RouteStatus *string `gorm:"column:route_status" json:"routeStatus,omitempty"`

	TruckTypeID *string `gorm:"column:truck_type_id" json:"truckTypeId,omitempty"`

	PricingTypeID *string `gorm:"column:pricing_type_id" json:"pricingTypeId,omitempty"`
	Price         Money   `gorm:"type:numeric(18,2);not null" json:"price"`
	MinQuantity   *Money  `gorm:"type:numeric(12,2)" json:"minQuantity,omitempty"`
	CurrencyID    *string `gorm:"column:currency_id" json:"currencyId,omitempty"`

	LeadTimeHours *int      `json:"leadTimeHours,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

func (AgreementRate) TableName() string { return "agreement_rates" }

// Lane granularity, mirroring the generated column.
const (
	LaneWarehouse = "warehouse"
	LaneDistrict  = "district"
	LaneCity      = "city"
	LaneAny       = "any"
)

// Why a lane does or does not carry a distance.
const (
	RouteRouted = "routed"
	// RouteNoCoordinates is a city or district lane. There is no region table
	// with coordinates, so there is nothing to route between — this is a
	// property of the lane, not a failure.
	RouteNoCoordinates = "noCoordinates"
	// RouteUnroutable is a warehouse pair MAPID could not connect.
	RouteUnroutable = "unroutable"
	RoutePending    = "pending"
)

// AgreementCustomer is one client an agreement covers.
//
// A join rather than more columns on the agreement: a framework agreement can
// cover a dozen clients, and the count is genuinely unbounded.
type AgreementCustomer struct {
	AgreementID       uuid.UUID `gorm:"type:uuid;primaryKey" json:"agreementId"`
	CustomerCompanyID uuid.UUID `gorm:"type:uuid;primaryKey" json:"customerCompanyId"`

	// Label is what this client is called on this contract, when it differs
	// from the company's own name — framework agreements often name a division.
	Label *string `json:"label,omitempty"`

	// CustomerName is resolved from the authentication service for display and
	// stored nowhere: a company that renames itself would otherwise appear
	// under its old name on every agreement forever.
	CustomerName string `gorm:"-" json:"customerName,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
}

func (AgreementCustomer) TableName() string { return "agreement_customers" }

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

	// AgreementID pins the exact VERSION this order was priced against, so an
	// amendment cannot restate work already done.
	AgreementID *uuid.UUID `gorm:"type:uuid" json:"agreementId,omitempty"`

	// AgreementRootID is the LINEAGE. It survives amendments, which is what
	// keeps "every order under this contract" answerable once the version the
	// order names has been superseded.
	AgreementRootID *uuid.UUID `gorm:"column:agreement_root_id;type:uuid" json:"agreementRootId,omitempty"`

	ShipperCompanyID     uuid.UUID  `gorm:"type:uuid;not null" json:"shipperCompanyId"`
	TransporterCompanyID *uuid.UUID `gorm:"type:uuid" json:"transporterCompanyId,omitempty"`
	CreatedByUserID      uuid.UUID  `gorm:"type:uuid;not null" json:"createdByUserId"`
	ParentOrderID        *uuid.UUID `gorm:"type:uuid" json:"parentOrderId,omitempty"`

	OrderKind string `gorm:"column:order_kind;not null;default:standard" json:"orderKind"`

	StatusCode  string `gorm:"column:status_code;not null" json:"statusCode"`
	Status      string `gorm:"-" json:"status"`
	StatusAlias string `gorm:"-" json:"statusAlias"`

	// DriverID is the master-data driver (Mongo ObjectId hex). DriverUserID
	// is that driver's login when they have one — most do not.
	DriverID     *string    `gorm:"column:driver_id" json:"driverId,omitempty"`
	DriverUserID *uuid.UUID `gorm:"type:uuid" json:"driverUserId,omitempty"`
	// TruckID is a Mongo ObjectId hex string, owned by the master data service.
	TruckID *string `gorm:"column:truck_id" json:"truckId,omitempty"`

	OriginWarehouseID      *string `gorm:"column:origin_warehouse_id" json:"originWarehouseId,omitempty"`
	DestinationWarehouseID *string `gorm:"column:destination_warehouse_id" json:"destinationWarehouseId,omitempty"`

	// The display names behind those ids, filled in from the master data
	// service and stored nowhere. They exist because an order list is unusable
	// without them: the ids are Mongo ObjectIds belonging to another service,
	// so a client that received only ids would have to fetch every warehouse
	// itself to render one column. Resolving once here costs a single call per
	// page; resolving in the client costs one per row.
	//
	// Empty when master data cannot be reached. A route column that degrades to
	// blank is better than a list that fails to load, so a resolution failure
	// is logged and not returned.
	OriginWarehouseName      string `gorm:"-" json:"originWarehouseName,omitempty"`
	DestinationWarehouseName string `gorm:"-" json:"destinationWarehouseName,omitempty"`
	TruckPoliceNumber        string `gorm:"-" json:"truckPoliceNumber,omitempty"`
	TruckTypeName            string `gorm:"-" json:"truckTypeName,omitempty"`
	DriverName               string `gorm:"-" json:"driverName,omitempty"`
	ShipperCompanyName       string `gorm:"-" json:"shipperCompanyName,omitempty"`
	TransporterCompanyName   string `gorm:"-" json:"transporterCompanyName,omitempty"`
	// ShipmentStatusCode is the current shipment's state, resolved on read.
	// The console derives its one displayed status from the order's own
	// state plus this, so a list needs it without a call per row.
	ShipmentStatusCode string `gorm:"-" json:"shipmentStatusCode,omitempty"`
	// ShipmentAcceptedAt: accepting keeps the status at "assigned" (only the
	// driver's movement changes it), so the status alone cannot say whether
	// the driver has taken the order. The driver app's Jadwal / Aktif tabs
	// split on exactly that.
	ShipmentAcceptedAt *time.Time `gorm:"-" json:"shipmentAcceptedAt,omitempty"`
	// The driver-flow checkpoints the status alone does not show: the cargo
	// checks at loading (driver) and unloading (PIC), the confirmed OTP, and
	// the latest POD per stage. Read-only, filled with the shipment.
	ShipmentLoadingCargoCheckedAt   *time.Time    `gorm:"-" json:"shipmentLoadingCargoCheckedAt,omitempty"`
	ShipmentLoadingCargoMatches     *bool         `gorm:"-" json:"shipmentLoadingCargoMatches,omitempty"`
	ShipmentUnloadingCargoCheckedAt *time.Time    `gorm:"-" json:"shipmentUnloadingCargoCheckedAt,omitempty"`
	ShipmentUnloadingCargoMatches   *bool         `gorm:"-" json:"shipmentUnloadingCargoMatches,omitempty"`
	ShipmentHandoverVerified        bool          `gorm:"-" json:"shipmentHandoverVerified,omitempty"`
	ShipmentPods                    []ShipmentPod `gorm:"-" json:"shipmentPods,omitempty"`

	CargoTypeID *string `gorm:"column:cargo_type_id" json:"cargoTypeId,omitempty"`
	ItemTypeID  *string `gorm:"column:item_type_id" json:"itemTypeId,omitempty"`
	Quantity    *Money  `gorm:"type:numeric(12,2)" json:"quantity,omitempty"`
	WeightKg    *Money  `gorm:"column:weight_kg;type:numeric(12,2)" json:"weightKg,omitempty"`
	VolumeM3    *Money  `gorm:"column:volume_m3;type:numeric(12,2)" json:"volumeM3,omitempty"`

	PickupAt   *time.Time `json:"pickupAt,omitempty"`
	DeliveryAt *time.Time `json:"deliveryAt,omitempty"`

	// ExpiresAt is when the order must be actioned by. Distinct from PickupAt:
	// pickup is when the truck is due, expiry is when the planner has run out
	// of time to find one.
	ExpiresAt *time.Time `gorm:"column:expires_at" json:"expiresAt,omitempty"`

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

	DriverID     *string    `gorm:"column:driver_id" json:"driverId,omitempty"`
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

	// The driver flow (K-Trip). Acceptance is the swipe that starts the job;
	// "toLoading" follows when the truck has moved off from where it was
	// accepted. The cargo checks gate the POD submissions: the driver's own
	// at loading, the receiving PIC's at unloading.
	AcceptedAt        *time.Time `json:"acceptedAt,omitempty"`
	AcceptedLatitude  *float64   `gorm:"type:numeric(10,7)" json:"acceptedLatitude,omitempty"`
	AcceptedLongitude *float64   `gorm:"type:numeric(10,7)" json:"acceptedLongitude,omitempty"`

	LoadingCargoMatches   *bool      `json:"loadingCargoMatches,omitempty"`
	LoadingCargoNote      *string    `json:"loadingCargoNote,omitempty"`
	LoadingCargoCheckedAt *time.Time `json:"loadingCargoCheckedAt,omitempty"`
	LoadingCargoCheckedBy *uuid.UUID `gorm:"type:uuid" json:"loadingCargoCheckedBy,omitempty"`

	UnloadingCargoMatches    *bool      `json:"unloadingCargoMatches,omitempty"`
	UnloadingCargoNote       *string    `json:"unloadingCargoNote,omitempty"`
	UnloadingCargoCheckedAt  *time.Time `json:"unloadingCargoCheckedAt,omitempty"`
	UnloadingCargoCheckedBy  *uuid.UUID `gorm:"type:uuid" json:"unloadingCargoCheckedBy,omitempty"`
	UnloadingCargoCheckedVia *string    `json:"unloadingCargoCheckedVia,omitempty"`

	// What the PIC actually counted at the gate, beside the ordered figures.
	// Nil where the PIC answered without counting; an absent figure is
	// honest, a figure copied from the order is not.
	UnloadingAuditWeightKg *Money `gorm:"column:unloading_audit_weight_kg;type:numeric(12,2)" json:"unloadingAuditWeightKg,omitempty"`
	UnloadingAuditVolumeM3 *Money `gorm:"column:unloading_audit_volume_m3;type:numeric(12,2)" json:"unloadingAuditVolumeM3,omitempty"`
	UnloadingAuditQuantity *Money `gorm:"column:unloading_audit_quantity;type:numeric(12,2)" json:"unloadingAuditQuantity,omitempty"`

	// Finalising closes the count. The PIC cannot reopen it from Web-Field;
	// the console can still reject the POD, which is the paperwork, not the
	// count.
	ManifestFinalizedAt   *time.Time `gorm:"column:manifest_finalized_at" json:"manifestFinalizedAt,omitempty"`
	ManifestFinalizedBy   *string    `gorm:"column:manifest_finalized_by" json:"manifestFinalizedBy,omitempty"`
	ManifestFinalizedNote *string    `gorm:"column:manifest_finalized_note" json:"manifestFinalizedNote,omitempty"`

	// Pods is the latest submission per stage, filled on read for the driver
	// app and the console so one call answers "what is the driver waiting
	// on". Not a column.
	Pods []ShipmentPod `gorm:"-" json:"pods,omitempty"`
	// PodHistory is every submission, newest first, filled on read: the
	// driver app's shipment detail shows a rejected loading POD beside the
	// one that replaced it, which Pods (latest per stage) cannot. Not a
	// column.
	PodHistory []ShipmentPod `gorm:"-" json:"podHistory,omitempty"`
	// HandoverVerified says the unloading OTP has been confirmed, and when.
	// Neither is a column: both come from the handover register on read.
	HandoverVerified   bool       `gorm:"-" json:"handoverVerified"`
	HandoverVerifiedAt *time.Time `gorm:"-" json:"handoverVerifiedAt,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (Shipment) TableName() string { return "shipments" }

// POD submission states.
const (
	PodSubmitted = "submitted"
	PodApproved  = "approved"
	PodRejected  = "rejected"
)

// ShipmentPod is one proof-of-delivery submission from the driver: the photos
// for one stage, and what the reviewer made of them. Approval is what moves
// the shipment past loading or unloading; a rejection sends the driver back
// to the camera, and the refused submission stays on record.
type ShipmentPod struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	ShipmentID uuid.UUID `gorm:"type:uuid;not null" json:"shipmentId"`
	Stage      string    `gorm:"not null" json:"stage"`

	// Photos is [{docType, fileUrl}], docType one of suratJalan | muatan |
	// pendukung; fileUrl is the storage key.
	Photos JSONArray `gorm:"type:jsonb" json:"photos"`
	Note   *string   `json:"note,omitempty"`

	Status          string  `gorm:"not null" json:"status"`
	RejectionReason *string `json:"rejectionReason,omitempty"`

	SubmittedByUserID *uuid.UUID `gorm:"type:uuid" json:"submittedByUserId,omitempty"`
	SubmittedAt       time.Time  `json:"submittedAt"`
	ReviewedByUserID  *uuid.UUID `gorm:"type:uuid" json:"reviewedByUserId,omitempty"`
	ReviewedAt        *time.Time `json:"reviewedAt,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
}

func (ShipmentPod) TableName() string { return "shipment_pods" }

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

	// The display names behind those two ids, resolved from the authentication
	// service and stored nowhere. A list rendered from the ids alone shows a
	// column of UUIDs, which is worse than showing nothing — and the client
	// cannot resolve them itself without a call per row.
	ShipperCompanyName     string `gorm:"-" json:"shipperCompanyName,omitempty"`
	TransporterCompanyName string `gorm:"-" json:"transporterCompanyName,omitempty"`

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

// AgreementPriceLine is one row of the agreement_price_history view: one priced
// lane of one version, with the version's own approval detail alongside.
//
// A view rather than a table, and read-only here for the same reason — the
// rates and versions already hold every fact, so a second copy would be one
// more thing to keep in step.
type AgreementPriceLine struct {
	RootAgreementID uuid.UUID `gorm:"column:root_agreement_id" json:"rootAgreementId"`
	AgreementID     uuid.UUID `gorm:"column:agreement_id" json:"agreementId"`
	AgreementNumber string    `gorm:"column:agreement_number" json:"agreementNumber"`
	Version         int       `json:"version"`

	RevisionKind *string `gorm:"column:revision_kind" json:"revisionKind,omitempty"`
	RevisionNote *string `gorm:"column:revision_note" json:"revisionNote,omitempty"`
	StatusCode   string  `gorm:"column:status_code" json:"statusCode"`

	ValidFrom  time.Time `gorm:"column:valid_from" json:"validFrom"`
	ValidUntil time.Time `gorm:"column:valid_until" json:"validUntil"`

	ApprovedAt       *time.Time `gorm:"column:approved_at" json:"approvedAt,omitempty"`
	ApprovedByUserID *uuid.UUID `gorm:"column:approved_by_user_id;type:uuid" json:"approvedByUserId,omitempty"`
	SupersededAt     *time.Time `gorm:"column:superseded_at" json:"supersededAt,omitempty"`

	RateID                *uuid.UUID `gorm:"column:rate_id;type:uuid" json:"rateId,omitempty"`
	OriginCityID          *string    `gorm:"column:origin_city_id" json:"originCityId,omitempty"`
	DestinationCityID     *string    `gorm:"column:destination_city_id" json:"destinationCityId,omitempty"`
	OriginDistrictID      *string    `gorm:"column:origin_district_id" json:"originDistrictId,omitempty"`
	DestinationDistrictID *string    `gorm:"column:destination_district_id" json:"destinationDistrictId,omitempty"`
	TruckTypeID           *string    `gorm:"column:truck_type_id" json:"truckTypeId,omitempty"`
	PricingTypeID         *string    `gorm:"column:pricing_type_id" json:"pricingTypeId,omitempty"`
	Price                 *Money     `gorm:"type:numeric(18,2)" json:"price,omitempty"`
	CurrencyID            *string    `gorm:"column:currency_id" json:"currencyId,omitempty"`
}
