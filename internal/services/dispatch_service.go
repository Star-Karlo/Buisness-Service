package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/karlo/business-service/internal/clients"
	"github.com/karlo/business-service/internal/models"
	masterdatav1 "github.com/karlo/business-service/internal/platform/genproto/karlo/masterdata/v1"
	"github.com/karlo/business-service/internal/repository"
	"github.com/karlo/business-service/internal/routing"
	"github.com/karlo/business-service/internal/telemetry"
)

// ErrNotEntitled is a caller asking for something their company has not been
// sold. Distinct from ErrForbidden, which is a caller acting outside their
// authority within what the company does hold: the first is answered by a sales
// conversation and the second by an administrator, and a client that cannot
// tell them apart shows the wrong message for both.
var ErrNotEntitled = errors.New("not entitled")

// FeatureAdvancedRouting is the entitlement behind re-planning a route after
// assignment. Declared here next to the only code that checks it, so the
// gate and the reason for it stay together.
const FeatureAdvancedRouting = "routing.advanced"

// DispatchService is the planner's half of the flow: which truck, and what
// roads.
type DispatchService struct {
	orders     *repository.OrderRepository
	routes     *repository.OrderRouteRepository
	cache      *routing.Cache
	masterdata *clients.MasterData
	telemetry  *telemetry.Client
	// shipments answers "is this caller the driver of that shipment", for the
	// driver's own route. Optional (tests).
	shipments *repository.ShipmentRepository
}

func NewDispatchService(
	orders *repository.OrderRepository,
	routes *repository.OrderRouteRepository,
	cache *routing.Cache,
	masterdata *clients.MasterData,
	tel *telemetry.Client,
) *DispatchService {
	return &DispatchService{
		orders: orders, routes: routes, cache: cache,
		masterdata: masterdata, telemetry: tel,
	}
}

// PositionSource says where a truck's position came from.
//
// Surfaced to the planner rather than kept internal, because the two are not
// equally trustworthy and a bare coordinate does not say which it is. A live
// fix is where the truck is; a last-drop fix is where it finished its previous
// job, which is right for a parked truck and wrong for a moving one.
type PositionSource string

const (
	SourceLive     PositionSource = "live"
	SourceLastDrop PositionSource = "lastDrop"
	SourceNone     PositionSource = "none"
)

// truckPosition is a resolved position plus its provenance.
type truckPosition struct {
	Lat, Lon float64
	At       time.Time
	Source   PositionSource

	// Kecamatan and City are reverse-geocoded names, present only on a live
	// fix. Telemetry supplies them; a coordinate remembered from a shipment
	// does not carry them and looking them up would be a geocoding call per
	// truck to label a list.
	Kecamatan string
	City      string
}

// positionsFor resolves where each truck is, preferring live telemetry.
//
// The fallback is deliberate and ordered. Telemetry is authoritative but is a
// separate service that can be down, not configured, or simply have no fix for
// a device that has never reported; the last completed shipment's unloading
// coordinate is always available and is correct for a truck that has not moved
// since. Preferring the stale answer would be wrong; refusing to rank without
// the live one would take the planner's screen down with an outage in a service
// that is optional by design.
func (s *DispatchService) positionsFor(ctx context.Context, trucks []*masterdatav1.Truck) map[string]truckPosition {
	out := make(map[string]truckPosition, len(trucks))

	// Live first, by IMEI. Trucks with no device fitted are not asked about.
	imeiToTruck := make(map[string]string, len(trucks))
	imeis := make([]string, 0, len(trucks))
	for _, t := range trucks {
		if imei := t.GetImei(); imei != "" {
			imeiToTruck[imei] = t.GetId()
			imeis = append(imeis, imei)
		}
	}

	if s.telemetry.Configured() && len(imeis) > 0 {
		live, err := s.telemetry.Live(ctx, imeis)
		if err != nil {
			// Degrade to the fallback rather than fail. A candidate list ranked
			// on last-drop positions is still useful; no list at all is not.
			slog.WarnContext(ctx, "live telemetry unavailable, falling back to last drop",
				"error", err, "devices", len(imeis))
		} else {
			for imei, p := range live {
				out[imeiToTruck[imei]] = truckPosition{
					Lat: p.Lat, Lon: p.Lon, At: p.Time, Source: SourceLive,
					Kecamatan: p.Kecamatan, City: p.City(),
				}
			}
		}
	}

	// Fill the gaps from the last completed shipment.
	missing := make([]string, 0, len(trucks))
	for _, t := range trucks {
		if _, ok := out[t.GetId()]; !ok {
			missing = append(missing, t.GetId())
		}
	}
	if len(missing) > 0 {
		lastDrops, err := s.routes.LastKnownPositions(ctx, missing)
		if err != nil {
			slog.WarnContext(ctx, "last-drop positions unavailable", "error", err)
		}
		for id, p := range lastDrops {
			out[id] = truckPosition{Lat: p.Lat, Lon: p.Lon, At: p.At, Source: SourceLastDrop}
		}
	}

	return out
}

// Candidate is one truck the planner could assign, with the evidence for
// choosing it.
type Candidate struct {
	TruckID      string   `json:"truckId"`
	PoliceNumber string   `json:"policeNumber"`
	TruckTypeID  string   `json:"truckTypeId"`
	DriverIDs    []string `json:"driverIds"`

	// StraightLineMeters is the distance to the loading point as the crow
	// flies. Named for what it is rather than "distance", because the road is
	// always longer and a planner reading "42 km" should know which 42 km.
	StraightLineMeters int `json:"straightLineMeters"`

	// PositionAt is when the truck was last known to be there. Surfaced
	// because a position from three days ago should be weighed differently
	// from one from this morning, and hiding the age would make them look the
	// same.
	PositionAt *time.Time `json:"positionAt,omitempty"`

	// PositionSource says whether this is a live fix or the truck's last
	// unloading point. They are not equally trustworthy and a coordinate does
	// not say which it is, so the planner is told.
	PositionSource PositionSource `json:"positionSource"`

	// Where the truck is, in words. Present only on a live fix — telemetry
	// reverse-geocodes; a coordinate remembered from a shipment does not carry
	// names, and looking them up would be a geocoding call per truck to label
	// a list.
	Kecamatan string `json:"kecamatan,omitempty"`
	City      string `json:"city,omitempty"`

	// PositionKnown is false for a truck with no device fitted that has never
	// completed a shipment. Such trucks are still offered — a new truck is
	// assignable — but they sort last, because there is no evidence either way.
	PositionKnown bool `json:"positionKnown"`
}

// Candidates lists the trucks available for an order, nearest first.
//
// Ranked by straight-line distance rather than by road route deliberately.
// Routing every truck in the fleet would be one MAPID call per candidate —
// slow, billable, and on a sixty-truck fleet enough to make the screen unusable
// — and the straight line orders candidates correctly in all but contrived
// geography. The road route is computed once, for the truck actually chosen.
func (s *DispatchService) Candidates(ctx context.Context, actor Actor, orderID uuid.UUID) ([]Candidate, error) {
	order, err := s.orders.FindByID(ctx, actor.CompanyID, orderID)
	if err != nil {
		return nil, err
	}
	if order.OriginWarehouseID == nil {
		return nil, fmt.Errorf("%w: the order has no loading point to measure from", ErrValidation)
	}

	origin, err := s.warehousePoint(ctx, *order.OriginWarehouseID)
	if err != nil {
		return nil, err
	}

	trucks, err := s.masterdata.ListAvailableTrucks(ctx, actor.CompanyID.String())
	if err != nil {
		return nil, err
	}

	positions := s.positionsFor(ctx, trucks)

	out := make([]Candidate, 0, len(trucks))
	for _, t := range trucks {
		c := Candidate{
			TruckID:        t.GetId(),
			PoliceNumber:   t.GetPoliceNumber(),
			TruckTypeID:    t.GetTruckTypeId(),
			DriverIDs:      t.GetDriverIds(),
			PositionSource: SourceNone,
		}
		if p, ok := positions[t.GetId()]; ok {
			at := p.At
			c.PositionKnown = true
			c.PositionAt = &at
			c.PositionSource = p.Source
			c.Kecamatan, c.City = p.Kecamatan, p.City
			c.StraightLineMeters = int(routing.Haversine(routing.Point{Lon: p.Lon, Lat: p.Lat}, origin))
		}
		out = append(out, c)
	}

	sort.SliceStable(out, func(i, j int) bool {
		// Trucks with no known position sort last rather than first. A zero
		// distance would otherwise put every never-used truck at the top of
		// the list, which reads as "nearest" and is the opposite of the truth.
		if out[i].PositionKnown != out[j].PositionKnown {
			return out[i].PositionKnown
		}
		// A live fix outranks a last-drop fix at equal distance: both are
		// estimates of the same thing, and one of them is current.
		if out[i].StraightLineMeters == out[j].StraightLineMeters {
			return out[i].PositionSource == SourceLive && out[j].PositionSource != SourceLive
		}
		return out[i].StraightLineMeters < out[j].StraightLineMeters
	})

	return out, nil
}

// PlanHaul computes the loading-to-unloading route for an order.
//
// Called when the order is created, because that route is known then and does
// not depend on who ends up driving it. Failure is not fatal to the order: an
// order that exists without a planned route is recoverable, an order refused
// because MAPID was briefly unwell is a sale lost to an outage.
func (s *DispatchService) PlanHaul(ctx context.Context, order *models.Order) error {
	if order.OriginWarehouseID == nil || order.DestinationWarehouseID == nil {
		return nil
	}

	from, err := s.warehousePoint(ctx, *order.OriginWarehouseID)
	if err != nil {
		return err
	}
	to, err := s.warehousePoint(ctx, *order.DestinationWarehouseID)
	if err != nil {
		return err
	}

	return s.planLeg(ctx, order.ID, models.LegHaul, from, to, uuid.Nil)
}

// PlanApproach computes the assigned truck's run to the loading point.
//
// Cannot exist before assignment, and is replaced on reassignment rather than
// added to — which is what the unique index on (order, leg) enforces.
func (s *DispatchService) PlanApproach(ctx context.Context, order *models.Order) error {
	if order.TruckID == nil || order.OriginWarehouseID == nil {
		return nil
	}

	p, ok := s.truckPosition(ctx, *order.TruckID)
	if !ok {
		// A truck with no device fitted that has never finished a shipment has
		// no position to start from. Not an error: the haul is still planned,
		// and the approach appears once the truck reports or completes a job.
		slog.InfoContext(ctx, "no known truck position, approach not planned",
			"orderId", order.ID, "truckId", *order.TruckID)
		return nil
	}

	to, err := s.warehousePoint(ctx, *order.OriginWarehouseID)
	if err != nil {
		return err
	}

	return s.planLeg(ctx, order.ID, models.LegApproach,
		routing.Point{Lon: p.Lon, Lat: p.Lat}, to, uuid.Nil)
}

// Reroute re-plans a leg on demand. This is the paid feature.
//
// The entitlement is checked here rather than only at the route table, because
// the same method is reachable from the order handler and from the planner, and
// a gate that lives in one caller is a gate the other forgets.
func (s *DispatchService) Reroute(ctx context.Context, actor Actor, entitled bool, orderID uuid.UUID, leg string) (*models.OrderRoute, error) {
	if !entitled {
		return nil, fmt.Errorf("%w: re-planning a route needs the advanced routing feature", ErrNotEntitled)
	}
	if leg != models.LegHaul && leg != models.LegApproach {
		return nil, fmt.Errorf("%w: no such leg %q", ErrValidation, leg)
	}

	order, err := s.orders.FindByID(ctx, actor.CompanyID, orderID)
	if err != nil {
		return nil, err
	}

	switch leg {
	case models.LegHaul:
		err = s.replanHaul(ctx, order, actor.UserID)
	case models.LegApproach:
		err = s.replanApproach(ctx, order, actor.UserID)
	}
	if err != nil {
		return nil, err
	}

	return s.routes.FindLeg(ctx, orderID, leg)
}

func (s *DispatchService) replanHaul(ctx context.Context, order *models.Order, actor uuid.UUID) error {
	if order.OriginWarehouseID == nil || order.DestinationWarehouseID == nil {
		return fmt.Errorf("%w: the order has no route to re-plan", ErrValidation)
	}
	from, err := s.warehousePoint(ctx, *order.OriginWarehouseID)
	if err != nil {
		return err
	}
	to, err := s.warehousePoint(ctx, *order.DestinationWarehouseID)
	if err != nil {
		return err
	}
	return s.planLeg(ctx, order.ID, models.LegHaul, from, to, actor)
}

func (s *DispatchService) replanApproach(ctx context.Context, order *models.Order, actor uuid.UUID) error {
	if order.TruckID == nil {
		return fmt.Errorf("%w: no truck is assigned, so there is no approach to plan", ErrValidation)
	}
	p, ok := s.truckPosition(ctx, *order.TruckID)
	if !ok {
		return fmt.Errorf("%w: the assigned truck has no known position", ErrValidation)
	}
	to, err := s.warehousePoint(ctx, *order.OriginWarehouseID)
	if err != nil {
		return err
	}
	return s.planLeg(ctx, order.ID, models.LegApproach,
		routing.Point{Lon: p.Lon, Lat: p.Lat}, to, actor)
}

// planLeg routes two points and stores the result against the order.
func (s *DispatchService) planLeg(ctx context.Context, orderID uuid.UUID, leg string, from, to routing.Point, reroutedBy uuid.UUID) error {
	result, err := s.cache.Route(ctx, routing.Request{
		Points:  []routing.Point{from, to},
		Profile: routing.ProfileTruck,
	})
	if err != nil {
		return err
	}

	row := &models.OrderRoute{
		OrderID:  orderID,
		Leg:      leg,
		CacheKey: result.Key,
		FromLat:  decimal.NewFromFloat(from.Lat),
		FromLon:  decimal.NewFromFloat(from.Lon),
		ToLat:    decimal.NewFromFloat(to.Lat),
		ToLon:    decimal.NewFromFloat(to.Lon),
	}
	if reroutedBy != uuid.Nil {
		now := time.Now()
		row.ReroutedAt = &now
		row.ReroutedByUserID = &reroutedBy
	}

	return s.routes.Upsert(ctx, row)
}

// Routes returns an order's planned legs.
// RouteForDriver returns a shipment's planned legs to the driver assigned to
// it. The shipment repository is injected by WithShipments; without it the
// call is refused rather than silently answering anyone.
func (s *DispatchService) RouteForDriver(ctx context.Context, actor Actor, shipmentID uuid.UUID) ([]models.OrderRoute, error) {
	if s.shipments == nil {
		return nil, fmt.Errorf("%w: route lookup is not available", ErrForbidden)
	}
	shipment, err := s.shipments.FindByID(ctx, shipmentID)
	if err != nil {
		return nil, err
	}
	if shipment.DriverUserID == nil || *shipment.DriverUserID != actor.UserID {
		return nil, fmt.Errorf("%w: only the assigned driver may read this shipment's route", ErrForbidden)
	}
	return s.Routes(ctx, actor, shipment.OrderID)
}

func (s *DispatchService) Routes(ctx context.Context, actor Actor, orderID uuid.UUID) ([]models.OrderRoute, error) {
	order, err := s.orders.FindByID(ctx, actor.CompanyID, orderID)
	if err != nil {
		return nil, err
	}
	legs, err := s.routes.ListByOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}

	// Backfill. Orders from before route planning at creation, or whose plan
	// failed at the time (MAPID outage), have no haul leg. The planned route is
	// part of the order's history — what the trip was quoted against — so it
	// is stored on first read rather than recomputed on every look: the cache
	// answers if the lane was ever planned, MAPID if not.
	hasHaul := false
	for _, l := range legs {
		if l.Leg == models.LegHaul {
			hasHaul = true
			break
		}
	}
	if !hasHaul && order.OriginWarehouseID != nil && order.DestinationWarehouseID != nil {
		if err := s.PlanHaul(ctx, order); err != nil {
			slog.WarnContext(ctx, "haul backfill failed", "orderId", orderID, "error", err)
		} else if legs, err = s.routes.ListByOrder(ctx, orderID); err != nil {
			return nil, err
		}
	}
	if s.cache.Reprice(ctx, unpricedTollKeys(legs)) {
		if legs, err = s.routes.ListByOrder(ctx, orderID); err != nil {
			return nil, err
		}
	}
	return legs, nil
}

// unpricedTollKeys lists the cache keys of legs that run on toll roads but
// were planned before fares were stored with the route.
func unpricedTollKeys(legs []models.OrderRoute) []string {
	var keys []string
	for _, l := range legs {
		if l.Cache != nil && l.Cache.HasToll && l.Cache.Toll == nil {
			keys = append(keys, l.CacheKey)
		}
	}
	return keys
}

// truckPosition resolves one truck, live first.
//
// Goes through the master data record rather than straight to telemetry
// because the IMEI lives there: the tracking service knows devices, not trucks,
// and the mapping is master data's to own.
func (s *DispatchService) truckPosition(ctx context.Context, truckID string) (truckPosition, bool) {
	truck, err := s.masterdata.GetTruck(ctx, truckID)
	if err != nil {
		slog.WarnContext(ctx, "truck not resolved for positioning", "truckId", truckID, "error", err)
		// Fall through: the last-drop lookup needs only the id, so a master
		// data failure costs the live fix and not the fallback.
		truck = &masterdatav1.Truck{Id: truckID}
	}

	positions := s.positionsFor(ctx, []*masterdatav1.Truck{truck})
	p, ok := positions[truckID]
	return p, ok
}

func (s *DispatchService) warehousePoint(ctx context.Context, id string) (routing.Point, error) {
	wh, err := s.masterdata.GetWarehouse(ctx, id)
	if err != nil {
		return routing.Point{}, err
	}
	if wh.GetLatitude() == 0 && wh.GetLongitude() == 0 {
		// Nought, nought is off the west coast of Africa. Treating it as a real
		// coordinate would produce a route from Indonesia to the Atlantic and a
		// distance in the tens of thousands of kilometres, which then flows
		// into a driver's advance.
		return routing.Point{}, fmt.Errorf("%w: warehouse %s has no coordinates", ErrValidation, id)
	}
	return routing.Point{Lon: wh.GetLongitude(), Lat: wh.GetLatitude()}, nil
}

// Warehouse exposes a resolved warehouse for callers that need its geofence.
// WithShipments lets the service answer a driver's own route.
func (s *DispatchService) WithShipments(shipments *repository.ShipmentRepository) *DispatchService {
	s.shipments = shipments
	return s
}

func (s *DispatchService) Warehouse(ctx context.Context, id string) (*masterdatav1.Warehouse, error) {
	return s.masterdata.GetWarehouse(ctx, id)
}

// FleetPosition is one truck on the planner's map.
type FleetPosition struct {
	TruckID      string         `json:"truckId"`
	PoliceNumber string         `json:"policeNumber"`
	Status       string         `json:"status"`
	IsAvailable  bool           `json:"isAvailable"`
	IMEI         string         `json:"imei,omitempty"`
	Lat          float64        `json:"lat"`
	Lon          float64        `json:"lon"`
	At           time.Time      `json:"at"`
	Source       PositionSource `json:"source"`
	City         string         `json:"city,omitempty"`
}

// FleetPositions is where every truck in the company's fleet is right now,
// live from telemetry where the truck has a device that has reported, and
// otherwise the last unloading point it was seen at. Trucks with neither are
// left out: the map shows what is known, not a guess at the depot.
func (s *DispatchService) FleetPositions(ctx context.Context, actor Actor) ([]FleetPosition, error) {
	trucks, err := s.masterdata.ListTrucks(ctx, actor.CompanyID.String())
	if err != nil {
		return nil, err
	}
	positions := s.positionsFor(ctx, trucks)
	out := make([]FleetPosition, 0, len(positions))
	for _, t := range trucks {
		p, ok := positions[t.GetId()]
		if !ok {
			continue
		}
		out = append(out, FleetPosition{
			TruckID:      t.GetId(),
			PoliceNumber: t.GetPoliceNumber(),
			Status:       t.GetStatus(),
			IsAvailable:  t.GetIsAvailable(),
			IMEI:         t.GetImei(),
			Lat:          p.Lat,
			Lon:          p.Lon,
			At:           p.At,
			Source:       p.Source,
			City:         p.City,
		})
	}
	return out, nil
}

// DriverActivity is the pairing screen's memory: which drivers have driven
// which of the company's trucks, how often, and when they were last on a
// job. Master-data knows the current pairing; only the order history knows
// the habit.
func (s *DispatchService) DriverActivity(ctx context.Context, actor Actor) ([]repository.DriverActivity, error) {
	return s.routes.DriverActivity(ctx, actor.CompanyID)
}
