package unit

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/karlo/business-service/internal/config"
	"github.com/karlo/business-service/internal/platform/authctx"
	"github.com/karlo/business-service/internal/routes"
)

// buildRouter constructs the HTTP surface with no database behind it. Handlers
// are nil, which is safe because these tests exercise routing and middleware:
// every request is rejected before a handler runs.
func buildRouter(t *testing.T, environment string) http.Handler {
	t.Helper()

	verifier, err := authctx.NewVerifier(testPublicKeyPEM(t))
	if err != nil {
		t.Fatalf("could not build verifier: %v", err)
	}

	return routes.Setup(routes.Deps{
		Config: &config.Config{
			Environment:        environment,
			CORSAllowedOrigins: []string{"http://localhost:5173"},
		},
		Verifier: verifier,
	})
}

func testPublicKeyPEM(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("could not generate a key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("could not marshal the public key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func TestHealthEndpoint(t *testing.T) {
	router := buildRouter(t, "development")

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", rec.Code)
	}
}

func TestSwaggerVisibility(t *testing.T) {
	dev := buildRouter(t, "development")
	req := httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil)
	rec := httptest.NewRecorder()
	dev.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Error("the Swagger UI should be served in development")
	}

	prod := buildRouter(t, "production")
	req = httptest.NewRequest(http.MethodGet, "/swagger/index.html", nil)
	rec = httptest.NewRecorder()
	prod.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("the Swagger UI should be hidden in production, got %d", rec.Code)
	}
}

// TestEveryRouteRequiresAuthentication walks the whole business surface.
//
// This service holds every company's commercial data — prices, volumes,
// counterparties. There is no public route, deliberately: the monolith's
// unauthenticated `/order/detail` and `/track-order/:id` endpoints exposed
// order contents to anyone who could guess a number.
func TestEveryRouteRequiresAuthentication(t *testing.T) {
	router := buildRouter(t, "development")

	const id = "00000000-0000-0000-0000-000000000000"

	protected := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/orders"},
		{http.MethodGet, "/api/v1/orders/summary"},
		{http.MethodPost, "/api/v1/orders"},
		{http.MethodGet, "/api/v1/orders/" + id},
		{http.MethodPut, "/api/v1/orders/" + id},
		{http.MethodGet, "/api/v1/orders/" + id + "/history"},
		{http.MethodGet, "/api/v1/orders/" + id + "/transitions"},
		{http.MethodPut, "/api/v1/orders/" + id + "/status"},
		{http.MethodPut, "/api/v1/orders/" + id + "/assign"},
		{http.MethodGet, "/api/v1/orders/" + id + "/shipment"},
		{http.MethodPut, "/api/v1/shipments/" + id + "/status"},
		{http.MethodGet, "/api/v1/shipments/" + id + "/transitions"},
		{http.MethodPost, "/api/v1/shipments/" + id + "/documents"},
		{http.MethodGet, "/api/v1/shipments/" + id + "/documents"},
		{http.MethodGet, "/api/v1/agreements"},
		{http.MethodPost, "/api/v1/agreements"},
		{http.MethodGet, "/api/v1/agreements/" + id},
		{http.MethodPut, "/api/v1/agreements/" + id + "/decision"},
		{http.MethodPut, "/api/v1/agreements/" + id + "/verify"},
		{http.MethodGet, "/api/v1/invoices"},
		{http.MethodPost, "/api/v1/invoices"},
		{http.MethodGet, "/api/v1/invoices/" + id},
		{http.MethodPut, "/api/v1/invoices/" + id + "/status"},
	}

	for _, route := range protected {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("= %d, want 401 for an unauthenticated request", rec.Code)
			}
		})
	}
}

func TestCORSRejectsUnlistedOrigins(t *testing.T) {
	router := buildRouter(t, "development")

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/orders", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if allowed := rec.Header().Get("Access-Control-Allow-Origin"); allowed != "" {
		t.Errorf("an unlisted origin was allowed: %q", allowed)
	}
}
