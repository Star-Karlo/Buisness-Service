// Package routes wires the business service HTTP surface.
//
// The whole table is below and it is short. The monolith registered 183 routes
// that resolved to 28 handlers; here every path does one thing, and the status
// endpoints are driven by state machines rather than by whatever a client puts
// in the body.
package routes

import (
	"net/http"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	swaggerfiles "github.com/swaggo/files"
	ginswagger "github.com/swaggo/gin-swagger"

	// Imported for its side effect: the generated package registers the
	// OpenAPI document with the swagger runtime on init.
	_ "github.com/karlo/business-service/docs"
	"github.com/karlo/business-service/internal/config"
	"github.com/karlo/business-service/internal/handlers"
	"github.com/karlo/business-service/internal/middleware"
	"github.com/karlo/business-service/internal/platform/authctx"
)

type Deps struct {
	Config   *config.Config
	Verifier *authctx.Verifier
	Remote   authctx.RemoteValidator

	Order    *handlers.OrderHandler
	Shipment *handlers.ShipmentHandler
	Billing  *handlers.BillingHandler
}

func Setup(d Deps) *gin.Engine {
	if d.Config.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(middleware.RequestLogger())
	router.Use(cors.New(cors.Config{
		AllowOrigins:     d.Config.CORSAllowedOrigins,
		AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "X-Request-Id"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	router.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "business"})
	})

	// The interactive API browser. It is served only outside production: the
	// document describes every endpoint and its shapes, which is exactly the
	// reconnaissance an attacker would otherwise have to guess at.
	if !d.Config.IsProduction() {
		// /swagger/index.html is the browser; /swagger/doc.json is the raw
		// document, which is what client generators want.
		router.GET("/swagger/*any", ginswagger.WrapHandler(swaggerfiles.Handler))
	}

	api := router.Group("/api/v1")
	api.Use(authctx.RequireAuth(d.Verifier, d.Remote))

	// Orders.
	orders := api.Group("/orders")
	orders.GET("", authctx.RequireModule("order.read"), d.Order.List)
	orders.GET("/summary", authctx.RequireModule("dashboard.read"), d.Order.Summary)
	orders.POST("", authctx.RequireModule("order.create"), d.Order.Create)
	orders.GET("/:id", authctx.RequireModule("order.read"), d.Order.Get)
	orders.PUT("/:id", authctx.RequireModule("order.update"), d.Order.UpdateDraft)
	orders.GET("/:id/history", authctx.RequireModule("order.read"), d.Order.History)
	orders.GET("/:id/transitions", authctx.RequireModule("order.read"), d.Order.NextStates)
	orders.PUT("/:id/status", d.Order.Transition)
	orders.PUT("/:id/assign", authctx.RequireModule("order.assignDriver"), d.Order.AssignDriver)
	orders.GET("/:id/shipment", authctx.RequireModule("order.read"), d.Shipment.GetByOrder)

	// Shipments. No module permission on the status route: the shipment state
	// machine already restricts each step to one role, and drivers are
	// sub-accounts whose permission maps would otherwise have to enumerate
	// every step.
	shipments := api.Group("/shipments")
	shipments.PUT("/:id/status", d.Shipment.Advance)
	shipments.GET("/:id/transitions", d.Shipment.NextStates)
	shipments.POST("/:id/documents", d.Shipment.AttachDocument)
	shipments.GET("/:id/documents", d.Shipment.Documents)

	// Agreements.
	agreements := api.Group("/agreements")
	agreements.GET("", authctx.RequireModule("agreement.read"), d.Billing.ListAgreements)
	agreements.POST("", authctx.RequireModule("agreement.create"), d.Billing.CreateAgreement)
	agreements.GET("/:id", authctx.RequireModule("agreement.read"), d.Billing.GetAgreement)
	agreements.PUT("/:id/decision", authctx.RequireModule("agreement.approve"), d.Billing.DecideAgreement)
	agreements.PUT("/:id/verify", authctx.RequireModule("agreement.approve"), d.Billing.VerifyAgreement)

	// Invoices.
	invoices := api.Group("/invoices")
	invoices.GET("", authctx.RequireModule("invoice.read"), d.Billing.ListInvoices)
	invoices.POST("", authctx.RequireModule("invoice.create"), d.Billing.CreateInvoice)
	invoices.GET("/:id", authctx.RequireModule("invoice.read"), d.Billing.GetInvoice)
	invoices.PUT("/:id/status", authctx.RequireModule("invoice.update"), d.Billing.TransitionInvoice)

	return router
}
