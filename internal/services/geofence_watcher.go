package services

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/karlo/business-service/internal/clients"
	masterdatav1 "github.com/karlo/business-service/internal/platform/genproto/karlo/masterdata/v1"
	"github.com/karlo/business-service/internal/repository"
	"github.com/karlo/business-service/internal/telemetry"
)

// GeofenceWatcher turns truck positions into arrival evidence.
//
// The legacy monolith owned the trackers and evaluated warehouse geofences as
// each position arrived. Positions now live in FMS, which knows nothing about
// shipments or warehouses (its geofences are a customer's own circles, keyed
// by vehicle), so the evaluation has to happen here: every tick, take the
// shipments in flight, ask telemetry where their trucks are, and compare each
// fix against the order's loading and unloading warehouse. A crossing is
// reported through the same ReportGeofenceEvent the gRPC edge uses, so the
// shipment does not care whether the evidence arrived by push or by poll.
//
// Polling rather than a push from FMS is a deliberate choice: FMS emits
// geofence alerts only for circles a customer drew in FMS and assigned
// vehicles to, and would have to hold order state to map them onto
// shipments. One minute of latency on "the truck has arrived" is well inside
// what a warehouse notices.
type GeofenceWatcher struct {
	shipments  *repository.ShipmentRepository
	shipment   *ShipmentService
	masterdata *clients.MasterData
	telemetry  *telemetry.Client
	interval   time.Duration

	// inside remembers, per shipment and warehouse, whether the truck was
	// within the fence at the last tick, so only crossings are reported.
	// In memory on purpose: a restart forgets, reports the current state once
	// more, and ReportGeofenceEvent is idempotent about that.
	mu     sync.Mutex
	inside map[fenceKey]bool

	// stops lets a journey be watched at every point it visits, not only at
	// the two the order names. Optional.
	stops *repository.OrderStopRepository
}

type fenceKey struct {
	shipment  uuid.UUID
	warehouse string
}

// defaultGeofenceRadiusMeters applies to a warehouse with no radius of its
// own. Matches the legacy monolith's inner band.
const defaultGeofenceRadiusMeters = 200

// staleFix is how old a position may be and still count. FMS's own "online"
// threshold; a fix older than this says where the truck was, not where it is.
const staleFix = 10 * time.Minute

// WithStops lets the watcher see every point of a journey, not only its two
// ends. Optional: without it the watcher behaves as it always did.
func (w *GeofenceWatcher) WithStops(stops *repository.OrderStopRepository) *GeofenceWatcher {
	w.stops = stops
	return w
}

// fencesFor is the warehouses this shipment should be watched against.
//
// The order's two columns are its FIRST loading point and its LAST unloading
// point. On a journey through several, the warehouses in between were watched
// by nobody: a truck could enter and leave Semarang without the server
// noticing, and the only record of the visit was whatever the driver's app
// managed to report. Every stop is watched where the journey has them.
func (w *GeofenceWatcher) fencesFor(ctx context.Context, a repository.ActiveShipment) []string {
	if w.stops != nil {
		if stops, err := w.stops.ListByOrder(ctx, a.OrderID); err == nil && len(stops) > 2 {
			ids := make([]string, 0, len(stops))
			for i := range stops {
				// A stop already finished is behind the truck; watching it
				// would report a crossing on the way past to somewhere else.
				if stops[i].FinishedAt == nil && stops[i].WarehouseID != "" {
					ids = append(ids, stops[i].WarehouseID)
				}
			}
			if len(ids) > 0 {
				return ids
			}
		}
	}
	ids := make([]string, 0, 2)
	for _, id := range []*string{a.OriginWarehouseID, a.DestinationWarehouseID} {
		if id != nil && *id != "" {
			ids = append(ids, *id)
		}
	}
	return ids
}

func NewGeofenceWatcher(
	shipments *repository.ShipmentRepository,
	shipment *ShipmentService,
	masterdata *clients.MasterData,
	tel *telemetry.Client,
	interval time.Duration,
) *GeofenceWatcher {
	if interval <= 0 {
		interval = time.Minute
	}
	return &GeofenceWatcher{
		shipments:  shipments,
		shipment:   shipment,
		masterdata: masterdata,
		telemetry:  tel,
		interval:   interval,
		inside:     map[fenceKey]bool{},
	}
}

// Start runs the loop until the returned stop function is called. Without
// telemetry configured it starts nothing: there would be no positions to
// evaluate, and logging that once is better than logging it every minute.
func (w *GeofenceWatcher) Start() func() {
	if w.telemetry == nil || !w.telemetry.Authenticated() {
		slog.Info("geofence watcher idle: no telemetry credential (service token or ingest key)")
		return func() {}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.Tick(ctx)
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// Tick evaluates every active shipment once. Exported so a test or an
// operator command can drive it without the timer.
func (w *GeofenceWatcher) Tick(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, w.interval)
	defer cancel()

	active, err := w.shipments.ListActive(ctx)
	if err != nil {
		slog.WarnContext(ctx, "geofence watcher: active shipments not listed", "error", err)
		return
	}
	if len(active) == 0 {
		w.forget(nil)
		return
	}

	// Trucks and warehouses are looked up once per tick, not once per
	// shipment: a fleet with thirty trucks in flight to the same two
	// warehouses is two warehouse lookups, not sixty.
	trucks := map[string]*masterdatav1.Truck{}
	warehouses := map[string]*masterdatav1.Warehouse{}
	imeis := make([]string, 0, len(active))
	for _, a := range active {
		if a.TruckID == nil {
			continue
		}
		if _, seen := trucks[*a.TruckID]; !seen {
			t, err := w.masterdata.GetTruck(ctx, *a.TruckID)
			if err != nil {
				slog.WarnContext(ctx, "geofence watcher: truck not resolved", "truckId", *a.TruckID, "error", err)
				trucks[*a.TruckID] = nil
				continue
			}
			trucks[*a.TruckID] = t
			if t.GetImei() != "" {
				imeis = append(imeis, t.GetImei())
			}
		}
		for _, id := range w.fencesFor(ctx, a) {
			if _, seen := warehouses[id]; !seen {
				wh, err := w.masterdata.GetWarehouse(ctx, id)
				if err != nil {
					slog.WarnContext(ctx, "geofence watcher: warehouse not resolved", "warehouseId", id, "error", err)
					warehouses[id] = nil
					continue
				}
				warehouses[id] = wh
			}
		}
	}
	if len(imeis) == 0 {
		return
	}

	live, err := w.telemetry.Live(ctx, imeis)
	if err != nil {
		slog.WarnContext(ctx, "geofence watcher: live positions unavailable", "error", err, "devices", len(imeis))
		return
	}

	now := time.Now()
	keep := make(map[fenceKey]struct{}, len(active)*2)
	for _, a := range active {
		if a.TruckID == nil {
			continue
		}
		truck := trucks[*a.TruckID]
		if truck == nil || truck.GetImei() == "" {
			continue
		}
		fix, ok := live[truck.GetImei()]
		if !ok || (fix.Lat == 0 && fix.Lon == 0) || now.Sub(fix.Time) > staleFix {
			continue
		}

		for _, id := range w.fencesFor(ctx, a) {
			wh := warehouses[id]
			if wh == nil || (wh.GetLatitude() == 0 && wh.GetLongitude() == 0) {
				continue
			}
			key := fenceKey{shipment: a.ShipmentID, warehouse: id}
			keep[key] = struct{}{}

			radius := float64(wh.GetGeofenceRadiusMeters())
			if radius <= 0 {
				radius = defaultGeofenceRadiusMeters
			}
			within := haversineMeters(fix.Lat, fix.Lon, wh.GetLatitude(), wh.GetLongitude()) <= radius

			w.mu.Lock()
			was, known := w.inside[key]
			w.inside[key] = within
			w.mu.Unlock()

			if !crossed(was, known, within) {
				continue
			}
			if _, err := w.shipment.ReportGeofenceEvent(ctx, a.ShipmentID, id, within, fix.Time); err != nil {
				slog.WarnContext(ctx, "geofence watcher: crossing not recorded",
					"shipmentId", a.ShipmentID, "warehouseId", id, "entering", within, "error", err)
				// Forget the transition so the next tick retries it.
				w.mu.Lock()
				if known {
					w.inside[key] = was
				} else {
					delete(w.inside, key)
				}
				w.mu.Unlock()
				continue
			}
			slog.InfoContext(ctx, "geofence crossing recorded",
				"shipmentId", a.ShipmentID, "warehouseId", id, "entering", within)
		}
	}
	w.forget(keep)
}

// forget drops state for shipments no longer in flight so the map does not
// grow with every shipment ever watched.
func (w *GeofenceWatcher) forget(keep map[fenceKey]struct{}) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range w.inside {
		if _, ok := keep[k]; !ok {
			delete(w.inside, k)
		}
	}
}

// crossed decides whether an observation is worth reporting. Only changes
// are: the same side of the fence as last tick is nothing. On the first
// observation of a fence, "inside" is a crossing — the truck was somewhere
// else when the shipment was created — and "outside" is nothing.
func crossed(was, known, within bool) bool {
	if known {
		return within != was
	}
	return within
}
