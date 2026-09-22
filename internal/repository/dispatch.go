package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/karlo/business-service/internal/fieldconfig"
	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/routing"
)

// ---------------------------------------------------------------------------
// Field configuration
// ---------------------------------------------------------------------------

type FieldConfigRepository struct{ db *gorm.DB }

func NewFieldConfigRepository(db *gorm.DB) *FieldConfigRepository {
	return &FieldConfigRepository{db: db}
}

// SyncCatalog writes what the code declares into field_definitions.
//
// Run at startup, and it is the half of the contract that makes the table
// trustworthy: the configurator reads this table, so a field that exists only
// in code would be invisible, and a row that exists only here would be a toggle
// that changes nothing. Presentation the database owns — a company's own
// wording lives in company_field_config, not here — so overwriting label and
// group from code discards nobody's edit.
//
// Fields the code no longer declares are marked inactive rather than deleted.
// Deleting would cascade away every company's configuration of them, which
// turns "we renamed a field" into silent data loss, and would also hide the
// question of who was still using it.
func (r *FieldConfigRepository) SyncCatalog(ctx context.Context) error {
	now := time.Now()

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var declared []models.FieldDefinition

		for _, entity := range fieldconfig.Entities() {
			for _, f := range fieldconfig.Declared(entity) {
				declared = append(declared, models.FieldDefinition{
					Entity:             string(entity),
					Key:                f.Key,
					DataType:           string(f.DataType),
					Configurable:       !f.Locked,
					DefaultRequirement: string(f.Default),
					GroupName:          f.Group,
					Label:              f.Label,
					HelpText:           f.Help,
					SortOrder:          f.Sort,
					IsActive:           true,
					SyncedAt:           &now,
				})
			}
		}

		if len(declared) > 0 {
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "entity"}, {Name: "key"}},
				DoUpdates: clause.AssignmentColumns([]string{
					"data_type", "configurable", "default_requirement",
					"group_name", "label", "help_text", "sort_order",
					"is_active", "synced_at", "updated_at",
				}),
			}).Create(&declared).Error; err != nil {
				return err
			}
		}

		// Anything this run did not touch is no longer declared.
		return tx.Model(&models.FieldDefinition{}).
			Where("synced_at IS NULL OR synced_at < ?", now).
			Update("is_active", false).Error
	})
}

// Overrides returns one company's stored decisions for an entity.
func (r *FieldConfigRepository) Overrides(ctx context.Context, companyID uuid.UUID, entity fieldconfig.Entity) ([]fieldconfig.Override, error) {
	var rows []models.CompanyFieldConfig
	if err := r.db.WithContext(ctx).
		Where("company_id = ? AND entity = ?", companyID, string(entity)).
		Find(&rows).Error; err != nil {
		return nil, err
	}

	out := make([]fieldconfig.Override, 0, len(rows))
	for _, row := range rows {
		o := fieldconfig.Override{
			Entity:      fieldconfig.Entity(row.Entity),
			Key:         row.Key,
			Requirement: fieldconfig.Requirement(row.Requirement),
		}
		if row.LabelOverride != nil {
			o.LabelOverride = *row.LabelOverride
		}
		out = append(out, o)
	}
	return out, nil
}

// SetOverride records or clears one decision.
//
// Clearing DELETES the row rather than writing the default back into it. Only
// rows that differ from the declared default are stored, so a stored "same as
// default" would later look like a deliberate choice and would stop tracking
// the default if the code's default changed.
func (r *FieldConfigRepository) SetOverride(ctx context.Context, companyID uuid.UUID, entity fieldconfig.Entity, key string, requirement fieldconfig.Requirement, label *string, actor uuid.UUID) error {
	declared := fieldconfig.Catalog[entity][key]

	if requirement == declared.Default && (label == nil || *label == "") {
		return r.db.WithContext(ctx).
			Where("company_id = ? AND entity = ? AND key = ?", companyID, string(entity), key).
			Delete(&models.CompanyFieldConfig{}).Error
	}

	row := models.CompanyFieldConfig{
		CompanyID:       companyID,
		Entity:          string(entity),
		Key:             key,
		Requirement:     string(requirement),
		LabelOverride:   label,
		UpdatedByUserID: &actor,
	}

	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "company_id"}, {Name: "entity"}, {Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"requirement", "label_override", "updated_by_user_id", "updated_at",
		}),
	}).Create(&row).Error
}

// ---------------------------------------------------------------------------
// Order items
// ---------------------------------------------------------------------------

type OrderItemRepository struct{ db *gorm.DB }

func NewOrderItemRepository(db *gorm.DB) *OrderItemRepository {
	return &OrderItemRepository{db: db}
}

func (r *OrderItemRepository) ListByOrder(ctx context.Context, orderID uuid.UUID) ([]models.OrderItem, error) {
	var items []models.OrderItem
	err := r.db.WithContext(ctx).
		Where("order_id = ?", orderID).Order("sort_order, created_at").
		Find(&items).Error
	return items, err
}

// ReplaceForOrder swaps an order's lines wholesale.
//
// Replace rather than diff: the client sends the table as the user left it, and
// reconciling that against stored rows by identity would need every row to
// carry an id the form has no reason to keep. The delete and the insert are one
// transaction, so a failure cannot leave the order with no cargo.
func (r *OrderItemRepository) ReplaceForOrder(ctx context.Context, orderID uuid.UUID, items []models.OrderItem) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("order_id = ?", orderID).Delete(&models.OrderItem{}).Error; err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		for i := range items {
			items[i].OrderID = orderID
			items[i].SortOrder = i
		}
		return tx.Create(&items).Error
	})
}

// ---------------------------------------------------------------------------
// Route cache
// ---------------------------------------------------------------------------

type RouteCacheRepository struct{ db *gorm.DB }

func NewRouteCacheRepository(db *gorm.DB) *RouteCacheRepository {
	return &RouteCacheRepository{db: db}
}

// Lookup returns a live cache entry, or nil for a miss.
//
// A miss and an expired entry are the same answer deliberately: the caller
// recomputes either way, and returning a stale route with a flag would put the
// decision in every call site.
func (r *RouteCacheRepository) Lookup(ctx context.Context, key string) (*routing.Entry, error) {
	var row models.RouteCacheEntry
	err := r.db.WithContext(ctx).
		Where("cache_key = ? AND expires_at > NOW()", key).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Usage accounting must not fail the read. It exists to answer "is this
	// cache earning its keep", and a lock wait on the counter should never
	// stop a planner seeing a route.
	go func() {
		//nolint:contextcheck // deliberately outlives the request
		_ = r.db.Model(&models.RouteCacheEntry{}).
			Where("cache_key = ?", key).
			UpdateColumns(map[string]interface{}{
				"hit_count":    gorm.Expr("hit_count + 1"),
				"last_used_at": time.Now(),
			}).Error
	}()

	return entryFromRow(row)
}

func (r *RouteCacheRepository) Save(ctx context.Context, e *routing.Entry) error {
	points := make(models.JSONArray, 0, len(e.Points))
	for _, p := range e.Points {
		points = append(points, []float64{p.Lon, p.Lat})
	}

	row := models.RouteCacheEntry{
		CacheKey:           e.CacheKey,
		Profile:            e.Profile,
		AvoidTolls:         e.AvoidTolls,
		Points:             points,
		DistanceMeters:     int(e.Route.DistanceMeters),
		DurationSeconds:    int(e.Route.Duration.Seconds()),
		Geometry:           toJSONArray(e.Route.Geometry),
		BBox:               toJSONArray(e.Route.BBox),
		TollSegments:       toJSONArray(e.Route.TollSegments),
		Toll:               toJSONB(e.Route.Toll),
		HasToll:            e.HasToll,
		TollDistanceMeters: e.TollDistanceMeters,
		ExpiresAt:          time.Now().Add(routing.CacheTTL),
	}

	// A concurrent save of the same key is two planners opening the same order
	// at once. Both computed the same route, so the later one is not a
	// conflict — it just refreshes the expiry.
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "cache_key"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"distance_meters", "duration_seconds", "geometry", "bbox",
			"toll_segments", "toll", "has_toll", "toll_distance_meters", "expires_at",
		}),
	}).Create(&row).Error
}

// EvictExpired removes dead entries. Called by the same sweep that expires
// agreements.
func (r *RouteCacheRepository) EvictExpired(ctx context.Context) (int64, error) {
	res := r.db.WithContext(ctx).
		Where("expires_at < NOW()").Delete(&models.RouteCacheEntry{})
	return res.RowsAffected, res.Error
}

func entryFromRow(row models.RouteCacheEntry) (*routing.Entry, error) {
	route := &routing.Route{
		DistanceMeters: float64(row.DistanceMeters),
		Duration:       time.Duration(row.DurationSeconds) * time.Second,
	}
	if err := remarshal(row.Geometry, &route.Geometry); err != nil {
		return nil, err
	}
	if err := remarshal(row.BBox, &route.BBox); err != nil {
		return nil, err
	}
	if err := remarshal(row.TollSegments, &route.TollSegments); err != nil {
		return nil, err
	}
	if len(row.Toll) > 0 {
		if raw, err := json.Marshal(row.Toll); err == nil {
			var toll routing.Toll
			if err := json.Unmarshal(raw, &toll); err == nil && len(toll.Prices) > 0 {
				route.Toll = &toll
			}
		}
	}

	return &routing.Entry{
		CacheKey: row.CacheKey, Profile: row.Profile, AvoidTolls: row.AvoidTolls,
		Route: route, HasToll: row.HasToll, TollDistanceMeters: row.TollDistanceMeters,
	}, nil
}

// remarshal moves a value through JSON to land it in a typed destination.
//
// JSONB scans into []interface{}, and the typed shapes here — [][]float64, a
// slice of TollSegment — cannot be asserted out of that element by element
// without rewriting the decoder. A round trip is a few microseconds on a route
// that took MAPID seconds to compute.
func remarshal(src models.JSONArray, dst interface{}) error {
	if len(src) == 0 {
		return nil
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

// toJSONB lands a struct (or nil) in a JSONB column; nil stays NULL.
func toJSONB(v interface{}) models.JSONB {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out models.JSONB
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func toJSONArray(v interface{}) models.JSONArray {
	raw, err := json.Marshal(v)
	if err != nil {
		return models.JSONArray{}
	}
	var out models.JSONArray
	if err := json.Unmarshal(raw, &out); err != nil {
		return models.JSONArray{}
	}
	return out
}

// ---------------------------------------------------------------------------
// Order routes
// ---------------------------------------------------------------------------

type OrderRouteRepository struct{ db *gorm.DB }

func NewOrderRouteRepository(db *gorm.DB) *OrderRouteRepository {
	return &OrderRouteRepository{db: db}
}

func (r *OrderRouteRepository) ListByOrder(ctx context.Context, orderID uuid.UUID) ([]models.OrderRoute, error) {
	var legs []models.OrderRoute
	err := r.db.WithContext(ctx).Preload("Cache").
		Where("order_id = ?", orderID).Find(&legs).Error
	return legs, err
}

func (r *OrderRouteRepository) FindLeg(ctx context.Context, orderID uuid.UUID, leg string) (*models.OrderRoute, error) {
	var row models.OrderRoute
	err := r.db.WithContext(ctx).Preload("Cache").
		Where("order_id = ? AND leg = ?", orderID, leg).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &row, err
}

// Upsert replaces the current route for a leg.
//
// Reassigning a driver replaces the approach and must leave the haul alone,
// which is why the unique key is (order, leg) rather than the row id: without
// it, reassignment would accumulate approach legs and every later reader would
// have to guess which was live.
func (r *OrderRouteRepository) Upsert(ctx context.Context, leg *models.OrderRoute) error {
	return r.db.WithContext(ctx).Omit("Cache").Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "order_id"}, {Name: "leg"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"cache_key", "from_lat", "from_lon", "to_lat", "to_lon",
			"rerouted_at", "rerouted_by_user_id", "updated_at",
		}),
	}).Create(leg).Error
}

// ---------------------------------------------------------------------------
// Truck positions
// ---------------------------------------------------------------------------

// TruckPosition is where a truck was last seen.
type TruckPosition struct {
	TruckID string
	Lat     float64
	Lon     float64
	At      time.Time
}

// LastKnownPositions returns where each truck finished its most recent job.
//
// This is a fallback, and it is worth being honest about what it is: the
// authoritative answer is live telemetry, which is a separate service and not
// wired to this one yet. What business-service genuinely knows is where each
// truck's last completed shipment unloaded, which is correct for a truck parked
// since its last delivery and stale for one that has moved on.
//
// It is still the right basis for ranking candidates. The planner is choosing
// between trucks, so what matters is their order, and a truck that finished
// near the loading point is a better bet than one that finished three provinces
// away regardless of how it has drifted since. When telemetry arrives, this
// implementation is replaced and nothing above it changes.
func (r *OrderRouteRepository) LastKnownPositions(ctx context.Context, truckIDs []string) (map[string]TruckPosition, error) {
	out := make(map[string]TruckPosition, len(truckIDs))
	if len(truckIDs) == 0 {
		return out, nil
	}

	var rows []struct {
		TruckID    string
		Latitude   float64
		Longitude  float64
		FinishedAt time.Time
	}

	// DISTINCT ON is Postgres-specific and is the point: it takes the first row
	// per truck under the ORDER BY, which is the most recent finished shipment,
	// in one pass. The portable alternative is a correlated subquery per truck,
	// which on a fleet of sixty is sixty queries.
	if err := r.db.WithContext(ctx).Raw(`
		SELECT DISTINCT ON (truck_id)
		       truck_id,
		       unloading_latitude  AS latitude,
		       unloading_longitude AS longitude,
		       finished_at
		FROM shipments
		WHERE truck_id IN ?
		  AND finished_at IS NOT NULL
		  AND unloading_latitude IS NOT NULL
		  AND unloading_longitude IS NOT NULL
		ORDER BY truck_id, finished_at DESC
	`, truckIDs).Scan(&rows).Error; err != nil {
		return nil, err
	}

	for _, row := range rows {
		out[row.TruckID] = TruckPosition{
			TruckID: row.TruckID, Lat: row.Latitude, Lon: row.Longitude, At: row.FinishedAt,
		}
	}
	return out, nil
}

// DriverActivity is one driver's history with a company's trucks, for the
// pairing screen's suggestions: who has driven this truck most, who has been
// idle longest.
type DriverActivity struct {
	DriverID     string     `json:"driverId"`
	TruckID      string     `json:"truckId"`
	Trips        int        `json:"trips"`
	LastActiveAt *time.Time `json:"lastActiveAt,omitempty"`
}

// DriverActivity aggregates shipments by (driver, truck) for orders the
// company carried. Drivers are master-data ids; the caller joins names.
func (r *OrderRouteRepository) DriverActivity(ctx context.Context, transporterID uuid.UUID) ([]DriverActivity, error) {
	var out []DriverActivity
	err := r.db.WithContext(ctx).Raw(`
		SELECT s.driver_id, s.truck_id, COUNT(*) AS trips, MAX(s.created_at) AS last_active_at
		FROM shipments s
		JOIN orders o ON o.id = s.order_id
		WHERE o.transporter_company_id = ?
		  AND s.driver_id IS NOT NULL AND s.truck_id IS NOT NULL
		  AND s.status_code <> 'cancelled'
		GROUP BY s.driver_id, s.truck_id`, transporterID).Scan(&out).Error
	return out, err
}
