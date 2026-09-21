package models

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// JSONArray is a JSONB column holding a list rather than an object.
//
// Separate from JSONB because JSONB is a map and scanning `[]` into one fails.
// The allowance components and the route geometry are both lists, and both were
// initially typed as JSONB — which compiled, and then failed at the first read.
type JSONArray []interface{}

func (a JSONArray) Value() (driver.Value, error) {
	if a == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(a)
}

func (a *JSONArray) Scan(src interface{}) error {
	if src == nil {
		*a = JSONArray{}
		return nil
	}
	b, ok := src.([]byte)
	if !ok {
		return fmt.Errorf("models: cannot scan %T into JSONArray", src)
	}
	return json.Unmarshal(b, a)
}

// ---------------------------------------------------------------------------
// Configurable fields
// ---------------------------------------------------------------------------

// FieldDefinition is the stored copy of a field the code declares.
type FieldDefinition struct {
	Entity   string `gorm:"primaryKey" json:"entity"`
	Key      string `gorm:"primaryKey" json:"key"`
	DataType string `gorm:"column:data_type" json:"dataType"`

	Configurable       bool   `json:"configurable"`
	DefaultRequirement string `gorm:"column:default_requirement" json:"defaultRequirement"`

	GroupName string `gorm:"column:group_name" json:"group"`
	Label     string `json:"label"`
	HelpText  string `gorm:"column:help_text" json:"help,omitempty"`
	SortOrder int    `gorm:"column:sort_order" json:"sortOrder"`

	IsActive bool       `gorm:"column:is_active" json:"isActive"`
	SyncedAt *time.Time `gorm:"column:synced_at" json:"syncedAt,omitempty"`
}

func (FieldDefinition) TableName() string { return "field_definitions" }

// CompanyFieldConfig is one company's decision about one field. Only rows that
// differ from the declared default exist.
type CompanyFieldConfig struct {
	CompanyID uuid.UUID `gorm:"type:uuid;primaryKey" json:"companyId"`
	Entity    string    `gorm:"primaryKey" json:"entity"`
	Key       string    `gorm:"primaryKey" json:"key"`

	Requirement   string  `json:"requirement"`
	LabelOverride *string `gorm:"column:label_override" json:"labelOverride,omitempty"`

	UpdatedByUserID *uuid.UUID `gorm:"column:updated_by_user_id;type:uuid" json:"updatedByUserId,omitempty"`
	UpdatedAt       time.Time  `json:"updatedAt"`
}

func (CompanyFieldConfig) TableName() string { return "company_field_config" }

// OrderItem is one line of itemised cargo, for the companies that itemise.
type OrderItem struct {
	ID      uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrderID uuid.UUID `gorm:"type:uuid;not null" json:"orderId"`

	CatalogItemID *string `gorm:"column:catalog_item_id" json:"catalogItemId,omitempty"`

	Name      string  `json:"name"`
	Quantity  *Money  `gorm:"type:numeric(12,2)" json:"quantity,omitempty"`
	Unit      *string `json:"unit,omitempty"`
	Packaging *string `json:"packaging,omitempty"`
	WeightKg  *Money  `gorm:"column:weight_kg;type:numeric(12,2)" json:"weightKg,omitempty"`

	LengthCm *Money `gorm:"column:length_cm;type:numeric(10,2)" json:"lengthCm,omitempty"`
	WidthCm  *Money `gorm:"column:width_cm;type:numeric(10,2)" json:"widthCm,omitempty"`
	HeightCm *Money `gorm:"column:height_cm;type:numeric(10,2)" json:"heightCm,omitempty"`
	VolumeM3 *Money `gorm:"column:volume_m3;type:numeric(12,4)" json:"volumeM3,omitempty"`

	HandlingNotes *string `gorm:"column:handling_notes" json:"handlingNotes,omitempty"`

	LoadedQuantity   *Money `gorm:"column:loaded_quantity;type:numeric(12,2)" json:"loadedQuantity,omitempty"`
	UnloadedQuantity *Money `gorm:"column:unloaded_quantity;type:numeric(12,2)" json:"unloadedQuantity,omitempty"`

	SortOrder int       `gorm:"column:sort_order" json:"sortOrder"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (OrderItem) TableName() string { return "order_items" }

// ---------------------------------------------------------------------------
// Routing
// ---------------------------------------------------------------------------

// RouteCacheEntry is a MAPID route remembered by the content of its request.
type RouteCacheEntry struct {
	CacheKey   string `gorm:"column:cache_key;primaryKey" json:"cacheKey"`
	Profile    string `json:"profile"`
	AvoidTolls bool   `gorm:"column:avoid_tolls" json:"avoidTolls"`

	Points JSONArray `gorm:"type:jsonb" json:"points"`

	DistanceMeters  int `gorm:"column:distance_meters" json:"distanceMeters"`
	DurationSeconds int `gorm:"column:duration_seconds" json:"durationSeconds"`

	Geometry JSONArray `gorm:"type:jsonb" json:"geometry,omitempty"`
	// The column name is stated explicitly: GORM derives "b_box" from BBox,
	// which is not the column, and the mismatch only surfaces at the first
	// write.
	BBox         JSONArray `gorm:"column:bbox;type:jsonb" json:"bbox,omitempty"`
	TollSegments JSONArray `gorm:"column:toll_segments;type:jsonb" json:"tollSegments,omitempty"`

	HasToll            bool `gorm:"column:has_toll" json:"hasToll"`
	TollDistanceMeters int  `gorm:"column:toll_distance_meters" json:"tollDistanceMeters"`

	HitCount   int       `gorm:"column:hit_count" json:"hitCount"`
	LastUsedAt time.Time `gorm:"column:last_used_at" json:"lastUsedAt"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `gorm:"column:expires_at" json:"expiresAt"`
}

func (RouteCacheEntry) TableName() string { return "route_cache" }

// Route legs. An order has two journeys and they are not interchangeable.
const (
	// LegHaul is origin warehouse to destination warehouse. Known as soon as
	// the order exists and identical for every candidate truck.
	LegHaul = "haul"

	// LegApproach is the assigned driver's last position to the loading point.
	// Cannot exist before assignment, and differs per candidate — it is what
	// makes one truck a better choice than another.
	LegApproach = "approach"
)

// OrderRoute is one planned leg of an order.
type OrderRoute struct {
	ID       uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	OrderID  uuid.UUID `gorm:"type:uuid;not null" json:"orderId"`
	Leg      string    `json:"leg"`
	CacheKey string    `gorm:"column:cache_key" json:"cacheKey"`

	FromLat Money `gorm:"column:from_lat;type:numeric(10,7)" json:"fromLat"`
	FromLon Money `gorm:"column:from_lon;type:numeric(10,7)" json:"fromLon"`
	ToLat   Money `gorm:"column:to_lat;type:numeric(10,7)" json:"toLat"`
	ToLon   Money `gorm:"column:to_lon;type:numeric(10,7)" json:"toLon"`

	ReroutedAt       *time.Time `gorm:"column:rerouted_at" json:"reroutedAt,omitempty"`
	ReroutedByUserID *uuid.UUID `gorm:"column:rerouted_by_user_id;type:uuid" json:"reroutedByUserId,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// The cached computation, joined in for reading. Never written through
	// this association: two orders on a lane share one cache row, and letting
	// GORM cascade a save here would have one order's write clobber the other's.
	Cache *RouteCacheEntry `gorm:"foreignKey:CacheKey;references:CacheKey;-:migration" json:"route,omitempty"`
}

func (OrderRoute) TableName() string { return "order_routes" }

// ---------------------------------------------------------------------------
// Uang sangu
// ---------------------------------------------------------------------------

// AllowanceComponent is one line of the driver's advance.
type AllowanceComponent struct {
	Code   string `json:"code"`
	Label  string `json:"label"`
	Amount Money  `json:"amount"`
	Note   string `json:"note,omitempty"`
}

// OrderAllowance is the driver's cash advance for an order.
//
// Entered by hand — by sales or by finance, whichever the company decided — so
// there is no calculator here. What the system contributes is the evidence:
// both distances and a toll estimate, so the figure is typed against something.
type OrderAllowance struct {
	OrderID uuid.UUID `gorm:"type:uuid;primaryKey" json:"orderId"`

	// Snapshotted when the advance was set, not read live. A reroute afterwards
	// changes the current distance and must not restate what somebody was paid
	// against.
	HaulDistanceMeters     *int   `gorm:"column:haul_distance_meters" json:"haulDistanceMeters,omitempty"`
	ApproachDistanceMeters *int   `gorm:"column:approach_distance_meters" json:"approachDistanceMeters,omitempty"`
	TollEstimate           *Money `gorm:"column:toll_estimate;type:numeric(18,2)" json:"tollEstimate,omitempty"`

	Components JSONArray `gorm:"type:jsonb" json:"components"`
	Total      Money     `gorm:"type:numeric(18,2)" json:"total"`
	CurrencyID *string   `gorm:"column:currency_id" json:"currencyId,omitempty"`

	Note *string `json:"note,omitempty"`

	EnteredByUserID *uuid.UUID `gorm:"column:entered_by_user_id;type:uuid" json:"enteredByUserId,omitempty"`
	EnteredAt       *time.Time `gorm:"column:entered_at" json:"enteredAt,omitempty"`

	FinalisedAt       *time.Time `gorm:"column:finalised_at" json:"finalisedAt,omitempty"`
	FinalisedByUserID *uuid.UUID `gorm:"column:finalised_by_user_id;type:uuid" json:"finalisedByUserId,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (OrderAllowance) TableName() string { return "order_allowances" }

// OrderAllowanceRevision is one superseded version of the advance.
type OrderAllowanceRevision struct {
	ID      int64     `gorm:"primaryKey" json:"id"`
	OrderID uuid.UUID `gorm:"type:uuid" json:"orderId"`

	Components JSONArray `gorm:"type:jsonb" json:"components"`
	Total      Money     `gorm:"type:numeric(18,2)" json:"total"`

	Reason          *string    `json:"reason,omitempty"`
	ChangedByUserID *uuid.UUID `gorm:"column:changed_by_user_id;type:uuid" json:"changedByUserId,omitempty"`
	ChangedAt       time.Time  `gorm:"column:changed_at" json:"changedAt"`
}

func (OrderAllowanceRevision) TableName() string { return "order_allowance_history" }

// ---------------------------------------------------------------------------
// Handover
// ---------------------------------------------------------------------------

// ShipmentHandover is the one-time code the receiving PIC gives the driver.
//
// The code is what demonstrates that a real person at the destination accepted
// the goods: it goes to the PIC's WhatsApp, and the driver types back what the
// PIC reads out, so possession of that phone at that address is the proof.
type ShipmentHandover struct {
	ID         uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	ShipmentID uuid.UUID `gorm:"column:shipment_id;type:uuid;not null" json:"shipmentId"`
	Stage      string    `json:"stage"`

	PICName     *string `gorm:"column:pic_name" json:"picName,omitempty"`
	PICWhatsApp string  `gorm:"column:pic_whatsapp" json:"picWhatsapp"`

	// Never serialised. It is a short-lived, low-value credential, but it is
	// still a credential, and a JSON tag here would put it in an API response.
	CodeHash []byte `gorm:"column:code_hash" json:"-"`

	Attempts    int16 `json:"attempts"`
	MaxAttempts int16 `gorm:"column:max_attempts" json:"maxAttempts"`

	SentAt     time.Time  `gorm:"column:sent_at" json:"sentAt"`
	ExpiresAt  time.Time  `gorm:"column:expires_at" json:"expiresAt"`
	VerifiedAt *time.Time `gorm:"column:verified_at" json:"verifiedAt,omitempty"`

	VerifiedLat            *Money `gorm:"column:verified_lat;type:numeric(10,7)" json:"verifiedLat,omitempty"`
	VerifiedLon            *Money `gorm:"column:verified_lon;type:numeric(10,7)" json:"verifiedLon,omitempty"`
	VerifiedWithinGeofence *bool  `gorm:"column:verified_within_geofence" json:"verifiedWithinGeofence,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
}

func (ShipmentHandover) TableName() string { return "shipment_handovers" }

// ---------------------------------------------------------------------------
// Public tracking
// ---------------------------------------------------------------------------

// TrackingLink is a public, account-less window onto one order — the link a
// transporter copies from Control Tower for its customer. The token is the
// whole secret; see migrations/000010_tracking_links.up.sql.
type TrackingLink struct {
	Token     string     `gorm:"primaryKey" json:"token"`
	OrderID   uuid.UUID  `gorm:"type:uuid;not null" json:"orderId"`
	CompanyID uuid.UUID  `gorm:"type:uuid;not null" json:"companyId"`
	CreatedBy *uuid.UUID `gorm:"type:uuid" json:"createdBy,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

func (TrackingLink) TableName() string { return "order_tracking_links" }
