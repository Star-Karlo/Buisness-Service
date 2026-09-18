// Command server runs the business service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/karlo/business-service/internal/platform/dbmigrate"

	"github.com/karlo/business-service/internal/clients"
	"github.com/karlo/business-service/internal/config"
	"github.com/karlo/business-service/internal/geocode"
	"github.com/karlo/business-service/internal/grpcserver"
	"github.com/karlo/business-service/internal/handlers"
	"github.com/karlo/business-service/internal/platform/authctx"
	"github.com/karlo/business-service/internal/platform/cache"
	businessv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/business/v1"
	"github.com/karlo/business-service/internal/platform/grpcutil"
	"github.com/karlo/business-service/internal/platform/logger"
	"github.com/karlo/business-service/internal/platform/revocation"
	"github.com/karlo/business-service/internal/repository"
	"github.com/karlo/business-service/internal/routes"
	"github.com/karlo/business-service/internal/routing"
	"github.com/karlo/business-service/internal/services"
	"github.com/karlo/business-service/internal/storage"
	"github.com/karlo/business-service/internal/telemetry"
)

// @title           Karlo Business API
// @version         1.0
// @description     The transactional core: agreements, orders, the shipment lifecycle and invoices.\n\nStatus changes go through state machines rather than accepting an arbitrary status string, so an illegal transition is refused with 409 rather than written.
// @termsOfService  https://karlo.co.id/terms
//
// @contact.name    Karlo Engineering
// @contact.email   engineering@karlo.co.id
//
// @host            localhost:5003
// @BasePath        /api/v1
// @schemes         http https
//
// @securityDefinitions.apikey BearerAuth
// @in                         header
// @name                       Authorization
// @description                RS256 access token issued by the authentication service, as "Bearer <token>".
func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// `server migrate`: bring the schema up to date and exit. Run as a one-off
	// ECS task from the same image and secrets as the service, which is the
	// only place RDS can be reached from. Exits non-zero on failure so the
	// task — and whatever invoked it — sees the failure.
	// Matched anywhere in the arguments, not only at [1]: an ECS command
	// override is appended to the image's ENTRYPOINT, so a caller that names
	// the binary again puts "migrate" at [2]. Reading only [1] made that a
	// silent no-op — the task started the server instead of migrating.
	if slices.Contains(os.Args[1:], "migrate") {
		// /migrations is where the Dockerfile puts them. Overridable so the
		// same command works from a checkout, where they are ./migrations.
		dir := os.Getenv("MIGRATIONS_DIR")
		if dir == "" {
			dir = "/migrations"
		}
		return dbmigrate.Run(cfg.Database.DSN(), cfg.Database.Name, dir)
	}

	// Logs go to stdout as JSON, and additionally to Fluentd when
	// FLUENTD_HOST is set. An unreachable collector degrades to
	// stdout-only rather than stopping the service.
	// This service belongs to TMS; authctx resolves HasModule, Role and
	// HasRole against it.
	authctx.SetProduct(authctx.ProductTMS)

	logger.InitFromEnv("business")
	defer logger.Close()

	verifier, err := authctx.NewVerifierFromEnv()
	if err != nil {
		return err
	}

	db, err := config.ConnectPostgres(cfg)
	if err != nil {
		return err
	}

	// `server archive [--dry-run]` and `server restore <entity> <id>`: cold
	// storage, run as one-off ECS tasks from the same image and secrets as
	// the service. See internal/archive.
	if slices.Contains(os.Args[1:], "archive") || slices.Contains(os.Args[1:], "restore") {
		return runArchive(cfg, db, os.Args[1:])
	}

	// Optional. Without REDIS_ADDR this is a no-op and company settings are
	// fetched from the authentication service on every call.
	cacheClient := cache.FromEnv("business")
	defer func() {
		if err := cacheClient.Close(); err != nil {
			slog.Error("cache close failed", "error", err)
		}
	}()

	authClient, err := clients.NewAuth(cfg.AuthGRPCAddr, "business", cfg.ServiceToken, cacheClient)
	if err != nil {
		return err
	}
	defer closeQuietly("auth client", authClient.Close)

	masterDataClient, err := clients.NewMasterData(cfg.MasterDataGRPCAddr, "business", cfg.ServiceToken)
	if err != nil {
		return err
	}
	defer closeQuietly("masterdata client", masterDataClient.Close)

	notifier, err := clients.NewNotification(cfg.NotificationGRPCAddr, "business", cfg.ServiceToken, cfg.NotifyTimeout)
	if err != nil {
		return err
	}
	defer closeQuietly("notification client", notifier.Close)

	orderRepo := repository.NewOrderRepository(db)
	shipmentRepo := repository.NewShipmentRepository(db)
	agreementRepo := repository.NewAgreementRepository(db)
	invoiceRepo := repository.NewInvoiceRepository(db)
	fieldConfigRepo := repository.NewFieldConfigRepository(db)
	orderItemRepo := repository.NewOrderItemRepository(db)
	routeCacheRepo := repository.NewRouteCacheRepository(db)
	orderRouteRepo := repository.NewOrderRouteRepository(db)
	allowanceRepo := repository.NewAllowanceRepository(db)
	handoverRepo := repository.NewHandoverRepository(db)
	ledgerRepo := repository.NewLedgerRepository(db)

	// Publish the configurable-field catalogue the code declares.
	//
	// Startup rather than migration, and it is the half of the contract that
	// makes the table trustworthy: the configurator reads field_definitions, so
	// a field declared only in code would be invisible there, and a row that
	// exists only in the table would be a toggle that saves happily and changes
	// nothing.
	//
	// A sync failure is fatal. Serving on a stale catalogue means administrators
	// configure fields that no longer exist and cannot configure ones that do,
	// which is worse than not starting.
	syncCtx, cancelSync := context.WithTimeout(context.Background(), 30*time.Second)
	err = fieldConfigRepo.SyncCatalog(syncCtx)
	cancelSync()
	if err != nil {
		return fmt.Errorf("sync field catalogue: %w", err)
	}

	// MAPID, wrapped in the database-backed cache. Everything that routes goes
	// through the cache rather than the bare client: the same warehouse pair is
	// planned for every order on a lane, and each miss is a billable call.
	routingClient := routing.New(cfg.MapIDBaseURL, cfg.MapIDKey)

	// Object storage. A missing bucket is not fatal — the client reports
	// itself unconfigured and the upload endpoints answer 501, which is what
	// the frontend reads to disable its file fields with a reason.
	storageClient, err := storage.New(context.Background(), storage.Config{
		Bucket:    cfg.StorageBucket,
		Region:    cfg.StorageRegion,
		Endpoint:  cfg.StorageEndpoint,
		AccessKey: cfg.StorageAccessKey,
		SecretKey: cfg.StorageSecretKey,
	})
	if err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
	if !storageClient.Configured() {
		slog.Warn("uploads disabled: STORAGE_BUCKET is not set")
	}
	routeCache := routing.NewCache(routingClient, routeCacheRepo)

	// Live vehicle positions. Optional: with no base URL configured, dispatch
	// falls back to each truck's last unloading point, which is where this
	// service got its positions before telemetry existed.
	telemetryClient := telemetry.New(cfg.TelemetryBaseURL, cfg.ServiceToken, cfg.TelemetryKey)
	geocodeClient := geocode.New(cfg.GeocodeBaseURL, cfg.ServiceToken)
	if !telemetryClient.Configured() {
		slog.Warn("telemetry not configured; dispatch will rank trucks on their last unloading point")
	}

	orderService := services.NewOrderService(orderRepo, shipmentRepo, agreementRepo, authClient, masterDataClient, notifier)
	shipmentService := services.NewShipmentService(shipmentRepo, orderRepo, authClient, masterDataClient, notifier)
	billingService := services.NewBillingService(agreementRepo, invoiceRepo, orderRepo, authClient, notifier)
	fieldConfigService := services.NewFieldConfigService(fieldConfigRepo)
	dispatchService := services.NewDispatchService(orderRepo, orderRouteRepo, routeCache, masterDataClient, telemetryClient)
	allowanceService := services.NewAllowanceService(orderRepo, allowanceRepo, orderRouteRepo)
	handoverService := services.NewHandoverService(shipmentRepo, orderRepo, handoverRepo, dispatchService, notifier)

	// The order service plans the haul route when an order is created. Injected
	// after construction rather than as a constructor argument because dispatch
	// needs the order repository, which the order service also owns — passing
	// each into the other's constructor is a cycle.
	orderService.WithDispatch(dispatchService)

	// The per-company field check. Attached to both create paths, so the
	// configuration a company sets shapes what the SERVER demands and not only
	// what the browser draws — the API is reachable by anyone with a token.
	orderService.WithFieldConfig(fieldConfigService, orderItemRepo)
	billingService.WithFieldConfig(fieldConfigService)
	// So an agreement's warehouse-level lanes can be measured on creation.
	billingService.WithDispatch(dispatchService)

	grpcSrv := grpcutil.NewServer(grpcutil.ServerConfig{
		Service:               "business",
		Addr:                  ":" + cfg.GRPCPort,
		Verifier:              verifier,
		AcceptedServiceTokens: cfg.AcceptedServiceTokens,
		EnableReflection:      !cfg.IsProduction(),
	})
	businessv1.RegisterBusinessServiceServer(
		grpcSrv.Registrar(),
		grpcserver.New(orderService, shipmentService, billingService, orderRepo),
	)

	// Revocations announced by the authentication service. Honoured locally,
	// so a suspension or a permission change takes effect at once without
	// putting authentication on the critical path of every request.
	watchCtx, stopWatching := context.WithCancel(context.Background())
	defer stopWatching()
	revocationChecker, _ := revocation.FromEnv(watchCtx, "business", tokenLifetimeHint())

	router := routes.Setup(routes.Deps{
		Config:      cfg,
		Verifier:    verifier,
		Remote:      authClient,
		Revocations: revocationChecker,
		Routing:     handlers.NewRoutingHandler(routeCache, geocodeClient),
		Upload:      handlers.NewUploadHandler(storageClient),
		Order:       handlers.NewOrderHandler(orderService),
		Shipment:    handlers.NewShipmentHandler(shipmentService),
		Billing:     handlers.NewBillingHandler(billingService),
		Dispatch:    handlers.NewDispatchHandler(dispatchService),
		FieldConfig: handlers.NewFieldConfigHandler(fieldConfigService),
		Allowance:   handlers.NewAllowanceHandler(allowanceService),
		Handover:    handlers.NewHandoverHandler(handoverService),
		Ledger:      handlers.NewLedgerHandler(ledgerRepo),
	})

	httpSrv := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 2)

	go func() {
		slog.Info("http server listening", "addr", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	go func() {
		if err := grpcSrv.Serve(); err != nil {
			errCh <- err
		}
	}()

	stopSweep := startAgreementExpirySweep(billingService)
	defer stopSweep()

	stopEviction := startRouteCacheEviction(routeCacheRepo)
	defer stopEviction()

	// Arrival evidence from FMS positions, authenticated with the platform
	// service token (or the ingest key while tracking still needs one). See
	// services.GeofenceWatcher for why this polls rather than waits for FMS
	// to push.
	geofenceWatcher := services.NewGeofenceWatcher(shipmentRepo, shipmentService, masterDataClient, telemetryClient, cfg.GeofencePollInterval)
	stopGeofence := geofenceWatcher.Start()
	defer stopGeofence()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case sig := <-quit:
		slog.Info("shutting down", "signal", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("http shutdown failed", "error", err)
	}
	grpcSrv.Shutdown(ctx)

	if sqlDB, err := db.DB(); err == nil {
		if err := sqlDB.Close(); err != nil {
			slog.Error("database close failed", "error", err)
		}
	}

	slog.Info("stopped")
	return nil
}

// startRouteCacheEviction deletes route cache entries past their expiry.
//
// Separate from the agreement sweep despite sharing an interval, because they
// fail independently: a cache table that will not delete should not stop
// agreements expiring, and an agreement sweep that errors should not leave the
// cache growing. Sharing a goroutine would couple the two.
//
// Nothing depends on this running. An expired entry is already ignored by
// Lookup, so the only cost of eviction failing is disk.
func startRouteCacheEviction(cache *repository.RouteCacheRepository) func() {
	ticker := time.NewTicker(time.Hour)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				n, err := cache.EvictExpired(ctx)
				cancel()
				if err != nil {
					slog.Error("route cache eviction failed", "error", err)
					continue
				}
				if n > 0 {
					slog.Info("evicted expired routes", "count", n)
				}
			case <-done:
				ticker.Stop()
				return
			}
		}
	}()

	return func() { close(done) }
}

// startAgreementExpirySweep marks contracts past their validity as expired.
//
// The legacy cron ran at a fixed 08:00 Asia/Jakarta. An interval is used here
// instead so that a restart does not skip a day, and because the operation is
// idempotent: an already-expired agreement is not matched again.
func startAgreementExpirySweep(billing *services.BillingService) func() {
	ticker := time.NewTicker(time.Hour)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				n, err := billing.ExpireAgreements(ctx)
				cancel()
				if err != nil {
					slog.Error("agreement expiry sweep failed", "error", err)
					continue
				}
				if n > 0 {
					slog.Info("expired agreements", "count", n)
				}
			case <-done:
				ticker.Stop()
				return
			}
		}
	}()

	return func() { close(done) }
}

func closeQuietly(name string, closer func() error) {
	if err := closer(); err != nil {
		slog.Error("close failed", "component", name, "error", err)
	}
}

// tokenLifetimeHint is how long a revocation entry must be kept: at least as
// long as the longest token that could still be in circulation.
//
// This service does not mint tokens and so cannot read the real setting. Two
// hours matches the authentication service's default; erring long is the safe
// direction, since an entry kept too long merely refuses a token that had
// already expired.
func tokenLifetimeHint() time.Duration { return 2 * time.Hour }
