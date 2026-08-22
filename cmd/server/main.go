// Command server runs the business service.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/karlo/business-service/internal/clients"
	"github.com/karlo/business-service/internal/config"
	"github.com/karlo/business-service/internal/grpcserver"
	"github.com/karlo/business-service/internal/handlers"
	"github.com/karlo/business-service/internal/platform/authctx"
	"github.com/karlo/business-service/internal/platform/cache"
	businessv1 "github.com/karlo/business-service/internal/platform/genproto/karlo/business/v1"
	"github.com/karlo/business-service/internal/platform/grpcutil"
	"github.com/karlo/business-service/internal/platform/logger"
	"github.com/karlo/business-service/internal/repository"
	"github.com/karlo/business-service/internal/routes"
	"github.com/karlo/business-service/internal/services"
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

	orderService := services.NewOrderService(orderRepo, shipmentRepo, agreementRepo, authClient, masterDataClient, notifier)
	shipmentService := services.NewShipmentService(shipmentRepo, orderRepo, authClient, masterDataClient, notifier)
	billingService := services.NewBillingService(agreementRepo, invoiceRepo, orderRepo, authClient, notifier)

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

	router := routes.Setup(routes.Deps{
		Config:   cfg,
		Verifier: verifier,
		Remote:   authClient,
		Order:    handlers.NewOrderHandler(orderService),
		Shipment: handlers.NewShipmentHandler(shipmentService),
		Billing:  handlers.NewBillingHandler(billingService),
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
