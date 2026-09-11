package services

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/routing"
)

// RouteLanes fills in the road distance for an agreement's lanes.
//
// Run after an agreement is written, because the distance is what the price is
// read against — a rate of "per truck, Jakarta to Semarang" means little until
// somebody knows that is 430 km, and typing it by hand is how two people end up
// quoting different numbers for the same road.
//
// Only WAREHOUSE-level lanes can be routed, and that is a property of the data
// rather than a gap: a city-to-city lane has no coordinates to route between.
// There is no region table with points in it, and inventing a city centre would
// produce a confident number that is wrong by however far the actual depots sit
// from it. Those lanes are marked noCoordinates instead.
//
// Failure is never fatal to the agreement. A contract with an unrouted lane is
// a contract; one refused because MAPID was briefly unwell is a sale lost to
// somebody else's outage.
func (s *BillingService) RouteLanes(ctx context.Context, agreementID uuid.UUID) {
	if s.dispatch == nil {
		return
	}

	rates, err := s.agreements.RatesNeedingRoute(ctx, agreementID)
	if err != nil {
		slog.WarnContext(ctx, "could not read lanes for routing",
			"agreementId", agreementID, "error", err)
		return
	}

	for _, rate := range rates {
		status, distance, key := s.routeOne(ctx, rate)

		update := map[string]interface{}{"route_status": status, "routed_at": time.Now()}
		if distance != nil {
			update["distance_meters"] = *distance
		}
		if key != "" {
			update["route_cache_key"] = key
		}

		if err := s.agreements.UpdateRate(ctx, rate.ID, update); err != nil {
			slog.WarnContext(ctx, "lane routed but not recorded",
				"rateId", rate.ID, "error", err)
		}
	}
}

// routeOne resolves a single lane.
func (s *BillingService) routeOne(ctx context.Context, rate models.AgreementRate) (status string, distance *int, cacheKey string) {
	if rate.LaneLevel != models.LaneWarehouse {
		// Said explicitly rather than left blank. "This lane is a city pair and
		// cannot have a distance" is a different fact from "nobody has routed
		// this yet", and a reader cannot tell them apart from an empty column.
		return models.RouteNoCoordinates, nil, ""
	}

	from, err := s.dispatch.warehousePoint(ctx, deref(rate.OriginWarehouseID))
	if err != nil {
		slog.InfoContext(ctx, "lane origin has no coordinates",
			"rateId", rate.ID, "error", err)
		return models.RouteNoCoordinates, nil, ""
	}
	to, err := s.dispatch.warehousePoint(ctx, deref(rate.DestinationWarehouseID))
	if err != nil {
		slog.InfoContext(ctx, "lane destination has no coordinates",
			"rateId", rate.ID, "error", err)
		return models.RouteNoCoordinates, nil, ""
	}

	result, err := s.dispatch.cache.Route(ctx, routing.Request{
		Points:  []routing.Point{from, to},
		Profile: routing.ProfileTruck,
	})
	if err != nil {
		if errors.Is(err, routing.ErrNotConfigured) {
			// Routing is switched off in this deployment. Pending rather than
			// unroutable: the lane is fine, nobody has asked yet.
			return models.RoutePending, nil, ""
		}
		slog.WarnContext(ctx, "MAPID could not connect a lane",
			"rateId", rate.ID, "error", err)
		return models.RouteUnroutable, nil, ""
	}

	meters := int(result.Route.DistanceMeters)
	return models.RouteRouted, &meters, result.Key
}
