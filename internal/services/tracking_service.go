package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/karlo/business-service/internal/clients"
	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/repository"
)

// TrackingService issues public tracking links and answers them.
//
// The link is for the transporter's customer: where is my load, what state is
// it in, when is it due. It shows exactly that and nothing else — no prices,
// no phone numbers, no other orders — because a link forwarded on WhatsApp
// travels further than the person it was sent to.
type TrackingService struct {
	links     *repository.TrackingLinkRepository
	orders    *repository.OrderRepository
	shipments *repository.ShipmentRepository
	routes    *repository.OrderRouteRepository
	auth      *clients.Auth
	master    *clients.MasterData
	dispatch  *DispatchService
}

func NewTrackingService(
	links *repository.TrackingLinkRepository,
	orders *repository.OrderRepository,
	shipments *repository.ShipmentRepository,
	routes *repository.OrderRouteRepository,
	auth *clients.Auth,
	master *clients.MasterData,
	dispatch *DispatchService,
) *TrackingService {
	return &TrackingService{
		links: links, orders: orders, shipments: shipments, routes: routes,
		auth: auth, master: master, dispatch: dispatch,
	}
}

// Link returns the order's tracking token, issuing one on first use. The
// caller must be a party to the order — the repository's tenant filter says so.
func (s *TrackingService) Link(ctx context.Context, actor Actor, orderID uuid.UUID) (*models.TrackingLink, error) {
	if _, err := s.orders.FindByID(ctx, actor.CompanyID, orderID); err != nil {
		return nil, err
	}
	if existing, err := s.links.FindActiveByOrder(ctx, orderID); err == nil {
		return existing, nil
	} else if !errors.Is(err, repository.ErrNotFound) {
		return nil, err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("tracking: token: %w", err)
	}
	link := &models.TrackingLink{
		Token:     base64.RawURLEncoding.EncodeToString(raw),
		OrderID:   orderID,
		CompanyID: actor.CompanyID,
		CreatedBy: &actor.UserID,
	}
	if err := s.links.Create(ctx, link); err != nil {
		return nil, err
	}
	return link, nil
}

// Revoke stops every live link of the order from resolving.
func (s *TrackingService) Revoke(ctx context.Context, actor Actor, orderID uuid.UUID) error {
	if _, err := s.orders.FindByID(ctx, actor.CompanyID, orderID); err != nil {
		return err
	}
	return s.links.Revoke(ctx, orderID)
}

// TrackingStop is one end of the journey, as the customer should see it.
type TrackingStop struct {
	Name string  `json:"name"`
	City string  `json:"city,omitempty"`
	Lat  float64 `json:"lat,omitempty"`
	Lon  float64 `json:"lon,omitempty"`
}

// TrackingEvent is one milestone reached.
type TrackingEvent struct {
	Status string    `json:"status"`
	Label  string    `json:"label"`
	At     time.Time `json:"at"`
}

// TrackingPosition is where the truck was last seen.
type TrackingPosition struct {
	Lat    float64        `json:"lat"`
	Lon    float64        `json:"lon"`
	At     time.Time      `json:"at"`
	City   string         `json:"city,omitempty"`
	Source PositionSource `json:"source"`
}

// TrackingSnapshot is everything the public page shows.
type TrackingSnapshot struct {
	OrderNumber     string `json:"orderNumber"`
	ShipperName     string `json:"shipperName,omitempty"`
	TransporterName string `json:"transporterName,omitempty"`

	Origin      TrackingStop `json:"origin"`
	Destination TrackingStop `json:"destination"`

	OrderStatus    string `json:"orderStatus"`
	ShipmentStatus string `json:"shipmentStatus,omitempty"`
	StatusLabel    string `json:"statusLabel"`

	Truck  string `json:"truck,omitempty"`
	Driver string `json:"driver,omitempty"`

	PickupAt   *time.Time `json:"pickupAt,omitempty"`
	DeliveryAt *time.Time `json:"deliveryAt,omitempty"`

	Events   []TrackingEvent   `json:"events"`
	Position *TrackingPosition `json:"position,omitempty"`

	// The planned haul, so the page can draw the road.
	Route           [][]float64 `json:"route,omitempty"`
	DistanceMeters  int         `json:"distanceMeters,omitempty"`
	DurationSeconds int         `json:"durationSeconds,omitempty"`

	UpdatedAt time.Time `json:"updatedAt"`
}

// Snapshot answers a public token. Every lookup failure short of the order
// itself degrades to a blank field: a customer should still see the status
// when master data is slow.
func (s *TrackingService) Snapshot(ctx context.Context, token string) (*TrackingSnapshot, error) {
	link, err := s.links.FindByToken(ctx, strings.TrimSpace(token))
	if err != nil {
		return nil, err
	}
	order, err := s.orders.FindByIDForService(ctx, link.OrderID)
	if err != nil {
		return nil, err
	}

	snap := &TrackingSnapshot{
		OrderNumber: order.OrderNumber,
		OrderStatus: order.StatusCode,
		StatusLabel: models.StatusLabel(models.DomainOrder, order.StatusCode),
		PickupAt:    order.PickupAt,
		DeliveryAt:  order.DeliveryAt,
		Events:      []TrackingEvent{},
		UpdatedAt:   time.Now(),
	}

	// Companies.
	if s.auth != nil {
		ids := []string{order.ShipperCompanyID.String()}
		if order.TransporterCompanyID != nil {
			ids = append(ids, order.TransporterCompanyID.String())
		}
		names := s.auth.CompanyNames(ctx, ids)
		snap.ShipperName = names[order.ShipperCompanyID.String()]
		if order.TransporterCompanyID != nil {
			snap.TransporterName = names[order.TransporterCompanyID.String()]
		}
	}

	// Ends of the journey.
	if s.master != nil {
		if order.OriginWarehouseID != nil {
			snap.Origin = s.stop(ctx, *order.OriginWarehouseID)
		}
		if order.DestinationWarehouseID != nil {
			snap.Destination = s.stop(ctx, *order.DestinationWarehouseID)
		}
		if order.TruckID != nil {
			if t, err := s.master.GetTruck(ctx, *order.TruckID); err == nil {
				snap.Truck = t.GetPoliceNumber()
			}
		}
		if order.DriverID != nil {
			if d, err := s.master.GetDriver(ctx, *order.DriverID); err == nil {
				// First name only: the customer needs to know who is at the
				// gate, not the driver's full identity.
				snap.Driver = strings.Fields(d.GetFullName() + " ")[0]
			}
		}
	}

	// Milestones: the shipment's own timestamps, then the order's closing steps.
	if sh, err := s.shipments.FindByOrder(ctx, order.ID); err == nil && sh != nil {
		snap.ShipmentStatus = sh.StatusCode
		if sh.StatusCode != "" && sh.StatusCode != models.ShipmentAssigned {
			snap.StatusLabel = models.StatusLabel(models.DomainShipment, sh.StatusCode)
		}
		add := func(code string, at *time.Time) {
			if at != nil {
				snap.Events = append(snap.Events, TrackingEvent{Status: code, Label: models.StatusLabel(models.DomainShipment, code), At: *at})
			}
		}
		add(models.ShipmentAssigned, &sh.CreatedAt)
		add(models.ShipmentToLoading, sh.StartedToLoadingAt)
		add(models.ShipmentAtLoading, sh.ArrivedLoadingAt)
		add(models.ShipmentLoading, sh.LoadingStartedAt)
		add(models.ShipmentLoaded, sh.LoadingFinishedAt)
		add(models.ShipmentToUnloading, sh.StartedToUnloadingAt)
		add(models.ShipmentAtUnloading, sh.ArrivedUnloadingAt)
		add(models.ShipmentUnloading, sh.UnloadingStartedAt)
		add(models.ShipmentUnloaded, sh.UnloadingFinishedAt)
		add(models.ShipmentFinished, sh.FinishedAt)
	} else {
		snap.Events = append(snap.Events, TrackingEvent{Status: order.StatusCode, Label: snap.StatusLabel, At: order.UpdatedAt})
	}
	if order.StatusCode == models.OrderCompleted {
		snap.Events = append(snap.Events, TrackingEvent{Status: models.OrderCompleted, Label: models.StatusLabel(models.DomainOrder, models.OrderCompleted), At: order.UpdatedAt})
	}

	// Where the truck is — only while the load is moving. A finished order
	// does not keep broadcasting the truck's whereabouts to the customer.
	if order.TruckID != nil && s.dispatch != nil &&
		(order.StatusCode == models.OrderAssigned || order.StatusCode == models.OrderInTransit) {
		if p, ok := s.dispatch.truckPosition(ctx, *order.TruckID); ok {
			snap.Position = &TrackingPosition{Lat: p.Lat, Lon: p.Lon, At: p.At, City: p.City, Source: p.Source}
		}
	}

	// The planned road.
	if leg, err := s.routes.FindLeg(ctx, order.ID, models.LegHaul); err == nil && leg != nil && leg.Cache != nil {
		snap.DistanceMeters = leg.Cache.DistanceMeters
		snap.DurationSeconds = leg.Cache.DurationSeconds
		for _, pt := range leg.Cache.Geometry {
			if pair, ok := pt.([]interface{}); ok && len(pair) == 2 {
				lon, okA := pair[0].(float64)
				lat, okB := pair[1].(float64)
				if okA && okB {
					snap.Route = append(snap.Route, []float64{lon, lat})
				}
			}
		}
	}

	return snap, nil
}

func (s *TrackingService) stop(ctx context.Context, warehouseID string) TrackingStop {
	wh, err := s.master.GetWarehouse(ctx, warehouseID)
	if err != nil || wh == nil {
		slog.WarnContext(ctx, "tracking: warehouse not resolved", "warehouseId", warehouseID, "error", err)
		return TrackingStop{}
	}
	return TrackingStop{Name: wh.GetName(), City: wh.GetCityId(), Lat: wh.GetLatitude(), Lon: wh.GetLongitude()}
}
