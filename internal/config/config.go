// Package config loads this service's configuration from the environment.
//
// The rule applied throughout: anything that is a security control has no
// default. A missing database password or signing key stops the process at
// startup instead of quietly falling back to something guessable, which is how
// the monolith ended up running with a JWT secret of "123".
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Environment string
	LogLevel    string

	HTTPPort string
	GRPCPort string

	Database Database

	// ServiceToken is the credential this service presents on outbound gRPC
	// calls, and AcceptedServiceTokens are the ones it honours inbound.
	ServiceToken          string
	AcceptedServiceTokens []string

	// Addresses of the services this one depends on.
	AuthGRPCAddr         string
	MasterDataGRPCAddr   string
	NotificationGRPCAddr string

	MapIDBaseURL string
	MapIDKey     string

	// Object storage for user uploads. An empty bucket disables uploads
	// rather than failing at startup: every other feature works without them.
	StorageBucket    string
	StorageRegion    string
	StorageEndpoint  string
	StorageAccessKey string
	StorageSecretKey string

	// TelemetryBaseURL is the FMS tracking service. Empty disables live
	// positions; dispatch then ranks on last unloading points instead.
	TelemetryBaseURL string
	// ConsoleBaseURL is where public console pages live (the tracking link,
	// the PIC's Web-Field page), for links put in messages.
	ConsoleBaseURL string
	TelemetryKey   string
	// GeocodeBaseURL is the platform's own geocoding service (fms-geocode),
	// reached over the VPC with the service token. Empty disables address
	// search on the warehouse form.
	GeocodeBaseURL string
	// GeofencePollInterval is how often active shipments' trucks are checked
	// against their warehouses' geofences. One minute is well inside what a
	// warehouse notices and one request per tick regardless of fleet size.
	GeofencePollInterval time.Duration

	// Cold storage. Aggregates untouched for their entity's retention move
	// to S3 Glacier Deep Archive as Parquet under ArchivePrefix, ArchiveBatch
	// per entity per run. Retention differs by entity: an order is rarely
	// opened a year after delivery; an agreement is a contract that may be
	// argued about for longer; an invoice sits under tax retention rules.
	// See internal/archive.
	ArchiveRetainOrders     time.Duration
	ArchiveRetainAgreements time.Duration
	ArchiveRetainInvoices   time.Duration
	ArchiveBatch            int
	ArchivePrefix           string

	CORSAllowedOrigins []string

	// TrustedProxies is the CIDR list Gin trusts for X-Forwarded-For.
	// Empty keeps Gin's default of trusting every proxy, so an unset variable
	// changes nothing; set it to the VPC CIDR behind a load balancer.
	TrustedProxies []string

	// NotifyTimeout bounds an outbound notification call. Notifications are
	// best-effort: a slow notification service must not hold a database
	// transaction open or fail the order it is reporting on.
	NotifyTimeout time.Duration
}

type Database struct {
	Host            string
	Port            string
	User            string
	Password        string
	Name            string
	SSLMode         string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// DSN renders the Postgres connection string.
func (d Database) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s TimeZone=Asia/Jakarta",
		d.Host, d.Port, d.User, d.Password, d.Name, d.SSLMode,
	)
}

// Load reads configuration, returning an error rather than exiting so that
// main can decide how to report it.
func Load() (*Config, error) {
	_ = godotenv.Load()

	env := envOr("ENVIRONMENT", "development")

	cfg := &Config{
		Environment: env,
		LogLevel:    envOr("LOG_LEVEL", "info"),
		HTTPPort:    envOr("HTTP_PORT", "5003"),
		GRPCPort:    envOr("GRPC_PORT", "6003"),

		Database: Database{
			Host:            envOr("DB_HOST", "localhost"),
			Port:            envOr("DB_PORT", "5432"),
			User:            envOr("DB_USER", "karlo"),
			Name:            envOr("DB_NAME", "karlo_business"),
			SSLMode:         envOr("DB_SSLMODE", sslDefault(env)),
			MaxOpenConns:    intOr("DB_MAX_OPEN_CONNS", 25),
			MaxIdleConns:    intOr("DB_MAX_IDLE_CONNS", 5),
			ConnMaxLifetime: durationOr("DB_CONN_MAX_LIFETIME", time.Hour),
		},

		AuthGRPCAddr:         envOr("AUTH_GRPC_ADDR", "localhost:6001"),
		MasterDataGRPCAddr:   envOr("MASTERDATA_GRPC_ADDR", "localhost:6002"),
		NotificationGRPCAddr: envOr("NOTIFICATION_GRPC_ADDR", "localhost:6004"),

		// Routing. The key is a credential and belongs on the server: routing is
		// proxied through this service precisely so it never reaches a browser
		// bundle, where it would be readable by every visitor.
		MapIDBaseURL: envOr("MAPID_BASE_URL", "https://routing.mapid.io/"),
		MapIDKey:     envOr("MAPID_KEY", ""),

		StorageBucket:    envOr("STORAGE_BUCKET", ""),
		StorageRegion:    envOr("STORAGE_REGION", "ap-southeast-3"),
		StorageEndpoint:  envOr("STORAGE_ENDPOINT", ""),
		StorageAccessKey: envOr("STORAGE_ACCESS_KEY", ""),
		StorageSecretKey: envOr("STORAGE_SECRET_KEY", ""),

		// Telemetry. Optional by design: an empty base URL leaves dispatch
		// ranking trucks by their last unloading point, which is correct for a
		// parked truck and the behaviour this service had before.
		TelemetryBaseURL:        envOr("TELEMETRY_BASE_URL", "https://fms-tracking.karlo.id"),
		ConsoleBaseURL:          envOr("CONSOLE_BASE_URL", "https://tms.karlo.id"),
		TelemetryKey:            envOr("TELEMETRY_INGEST_KEY", ""),
		GeocodeBaseURL:          envOr("GEOCODE_BASE_URL", ""),
		GeofencePollInterval:    durationOr("GEOFENCE_POLL_INTERVAL", time.Minute),
		ArchiveRetainOrders:     durationOr("ARCHIVE_RETAIN_ORDERS", 365*24*time.Hour),
		ArchiveRetainAgreements: durationOr("ARCHIVE_RETAIN_AGREEMENTS", 365*24*time.Hour),
		ArchiveRetainInvoices:   durationOr("ARCHIVE_RETAIN_INVOICES", 1095*24*time.Hour),
		ArchiveBatch:            intOr("ARCHIVE_BATCH", 500),
		ArchivePrefix:           envOr("ARCHIVE_PREFIX", "archive"),

		CORSAllowedOrigins: splitOr("CORS_ALLOWED_ORIGINS", nil),
		TrustedProxies:     splitOr("TRUSTED_PROXIES", nil),
		NotifyTimeout:      durationOr("NOTIFY_TIMEOUT", 5*time.Second),
	}

	var missing []string

	cfg.Database.Password = os.Getenv("DB_PASSWORD")
	if cfg.Database.Password == "" {
		missing = append(missing, "DB_PASSWORD")
	}

	cfg.ServiceToken = os.Getenv("SERVICE_TOKEN")
	if cfg.ServiceToken == "" {
		missing = append(missing, "SERVICE_TOKEN")
	}

	cfg.AcceptedServiceTokens = splitOr("ACCEPTED_SERVICE_TOKENS", nil)
	if len(cfg.AcceptedServiceTokens) == 0 {
		missing = append(missing, "ACCEPTED_SERVICE_TOKENS")
	}

	// A wildcard CORS policy on an authenticated API lets any origin drive the
	// browser's credentials. Require the list to be stated.
	if len(cfg.CORSAllowedOrigins) == 0 {
		missing = append(missing, "CORS_ALLOWED_ORIGINS")
	}

	if len(missing) > 0 {
		return nil, fmt.Errorf("config: required environment variables not set: %s", strings.Join(missing, ", "))
	}

	return cfg, nil
}

// IsProduction reports whether production safety rules apply.
//
// Terraform validates its environment variable as dev/staging/prod and passes
// it through unchanged, so the container sees ENVIRONMENT=prod. Matching only
// "production" left every production task with Swagger served, gRPC
// reflection on and every SQL statement logged. Both spellings are production.
func (c *Config) IsProduction() bool { return isProductionEnv(c.Environment) }

func isProductionEnv(env string) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "production", "prod":
		return true
	}
	return false
}

func sslDefault(env string) string {
	if isProductionEnv(env) {
		return "require"
	}
	return "disable"
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func intOr(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func durationOr(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func splitOr(key string, fallback []string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
