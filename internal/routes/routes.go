// Package routes wires the business service HTTP surface.
//
// The whole table is below and it is short. The monolith registered 183 routes
// that resolved to 28 handlers; here every path does one thing, and the status
// endpoints are driven by state machines rather than by whatever a client puts
// in the body.
package routes

import (
	"fmt"
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

	// Revocations lets a locally-verified token be refused before it expires.
	Revocations authctx.RevocationChecker

	Order    *handlers.OrderHandler
	Shipment *handlers.ShipmentHandler
	Billing  *handlers.BillingHandler
	Routing  *handlers.RoutingHandler

	Dispatch    *handlers.DispatchHandler
	FieldConfig *handlers.FieldConfigHandler
	Upload      *handlers.UploadHandler
	Allowance   *handlers.AllowanceHandler
	Handover    *handlers.HandoverHandler
}

func Setup(d Deps) *gin.Engine {
	if d.Config.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	// Behind a load balancer every request arrives from a VPC address with the
	// real client in X-Forwarded-For. Gin trusts that header from anyone by
	// default, which lets a caller choose the IP the audit log records. When
	// the operator names the proxies, trust only those; an unset list keeps
	// the default, so this is opt-in and changes nothing until configured.
	if len(d.Config.TrustedProxies) > 0 {
		if err := router.SetTrustedProxies(d.Config.TrustedProxies); err != nil {
			panic(fmt.Sprintf("routes: TRUSTED_PROXIES: %v", err))
		}
	}
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
	api.Use(authctx.RequireAuthWithRevocations(d.Verifier, d.Remote, d.Revocations))

	// Routing. Guarded by order.read rather than a routing key of its own:
	// planning a journey is something anyone who can see an order does, and a
	// separate permission would have to be granted to everybody, which is a
	// permission that means nothing.
	api.POST("/routing/route", authctx.RequireModule("order.read"), d.Routing.Plan)

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

	// File uploads. Deliberately NOT gated on a domain module: every screen
	// that attaches a file needs this, and a truck photo and an agreement PDF
	// would otherwise each need their own permission for the same act. The
	// company scoping in the handler is what protects it.
	uploads := api.Group("/uploads")
	uploads.POST("/presign", d.Upload.Presign)
	uploads.POST("/download-url", d.Upload.DownloadURL)

	// Dispatch. The planner's screen: which trucks could take this, and what
	// roads they would drive.
	orders.GET("/:id/candidates", authctx.RequireModule("dispatch.read"), d.Dispatch.Candidates)
	orders.GET("/:id/routes", authctx.RequireModule("dispatch.read"), d.Dispatch.Routes)

	// Re-planning is the paid feature. The permission gate here is only half
	// the check: dispatch.reroute names routing.advanced as its feature, so a
	// company without the entitlement fails at RequireModule, and the handler
	// checks the entitlement again before spending a routing call. Two checks
	// because this endpoint costs money on every press.
	orders.POST("/:id/reroute", authctx.RequireModule("dispatch.reroute"), d.Dispatch.Reroute)

	// Uang sangu. Separate permissions from the order itself, because a
	// company decides independently who may see the figure, who may set it,
	// and who may commit it to the driver — for some that is sales, for others
	// finance.
	// A static segment beside the :id routes. Gin's tree prefers the literal
	// match, so "allowances" is never parsed as an order id.
	orders.GET("/allowances", authctx.RequireModule("order.allowance.read"), d.Allowance.List)
	orders.GET("/:id/allowance", authctx.RequireModule("order.allowance.read"), d.Allowance.Get)
	orders.PUT("/:id/allowance", authctx.RequireModule("order.allowance.write"), d.Allowance.Save)
	orders.POST("/:id/allowance/finalise", authctx.RequireModule("order.allowance.finalise"), d.Allowance.Finalise)
	orders.GET("/:id/allowance/history", authctx.RequireModule("order.allowance.read"), d.Allowance.History)

	// Shipments. No module permission on the status route: the shipment state
	// machine already restricts each step to one role, and drivers are
	// sub-accounts whose permission maps would otherwise have to enumerate
	// every step.
	shipments := api.Group("/shipments")
	shipments.PUT("/:id/status", d.Shipment.Advance)
	shipments.GET("/:id/transitions", d.Shipment.NextStates)
	shipments.POST("/:id/documents", d.Shipment.AttachDocument)
	shipments.GET("/:id/documents", d.Shipment.Documents)

	// The unloading handover. No module permission, for the same reason as the
	// status route above: the driver is the only caller, the service checks
	// that they are the one assigned, and requiring a permission would mean
	// every driver sub-account enumerated it.
	shipments.POST("/:id/handover", d.Handover.Issue)
	shipments.POST("/:id/handover/verify", d.Handover.Verify)

	// Agreements.
	agreements := api.Group("/agreements")
	agreements.GET("", authctx.RequireModule("agreement.read"), d.Billing.ListAgreements)
	agreements.POST("", authctx.RequireModule("agreement.create"), d.Billing.CreateAgreement)
	// The approval queue, before /:id so "pending-approval" is not parsed as
	// an agreement id.
	agreements.GET("/pending-approval", authctx.RequireModule("agreement.approveRevision"), d.Billing.PendingApprovals)

	agreements.GET("/:id", authctx.RequireModule("agreement.read"), d.Billing.GetAgreement)

	// Versioning and approval. An agreement is a priced contract, so an
	// amendment is a new version rather than an edit in place — otherwise a
	// price change silently restates work already done.
	agreements.POST("/:id/revisions", authctx.RequireModule("agreement.revise"), d.Billing.Revise)
	agreements.PUT("/:id/revisions/decision", authctx.RequireModule("agreement.approveRevision"), d.Billing.DecideRevision)
	agreements.GET("/:id/versions", authctx.RequireModule("agreement.read"), d.Billing.Lineage)
	agreements.GET("/:id/price-history", authctx.RequireModule("agreement.priceHistory"), d.Billing.PriceHistory)
	agreements.PUT("/:id/decision", authctx.RequireModule("agreement.approve"), d.Billing.DecideAgreement)
	agreements.PUT("/:id/verify", authctx.RequireModule("agreement.approve"), d.Billing.VerifyAgreement)

	// Form configuration. What an agreement or an order form demands of this
	// company — the one thing about the flow that varies between them.
	config := api.Group("/config")
	config.GET("/fields/:entity", authctx.RequireModule("config.fields.read"), d.FieldConfig.Get)
	config.PUT("/fields/:entity", authctx.RequireModule("config.fields.write"), d.FieldConfig.Update)

	// Invoices.
	invoices := api.Group("/invoices")
	invoices.GET("", authctx.RequireModule("invoice.read"), d.Billing.ListInvoices)
	invoices.POST("", authctx.RequireModule("invoice.create"), d.Billing.CreateInvoice)
	invoices.GET("/:id", authctx.RequireModule("invoice.read"), d.Billing.GetInvoice)
	invoices.PUT("/:id/status", authctx.RequireModule("invoice.update"), d.Billing.TransitionInvoice)

	return router
}
