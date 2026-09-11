//go:build integration

// Package integration exercises the repository layer against a real Postgres.
//
// These tests are build-tagged so they never run in the unit suite. They cover
// what unit tests structurally cannot: actual SQL, actual indexes and
// constraints, and concurrency against a real transaction manager. Several of
// the claims this codebase makes — that a status transition is compare-and-set,
// that document numbers are collision-free — are only true if the database
// behaves as expected, and only a real database can say.
//
//	docker run -d --name pg -e POSTGRES_USER=karlo -e POSTGRES_PASSWORD=karlo \
//	  -e POSTGRES_DB=karlo_business_test -p 5432:5432 postgres:16-alpine
//	migrate -path migrations -database "$BUSINESS_TEST_DSN" up
//	go test -tags=integration ./tests/integration/... -v
package integration

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/repository"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// testDB opens the shared connection, skipping the suite when no database is
// configured. Skipping rather than failing means `go test ./...` stays green on
// a machine with no Postgres, while CI — which sets the variable — still runs
// them.
func testDB(t *testing.T) *gorm.DB {
	t.Helper()

	dsn := os.Getenv("BUSINESS_TEST_DSN")
	if dsn == "" {
		t.Skip("BUSINESS_TEST_DSN is not set; skipping integration tests")
	}

	// This suite TRUNCATEs orders, agreements and invoices. Pointed at the
	// development database it destroys the seed data, and the damage is
	// invisible until someone opens the order list. The database must be one
	// created for testing, so its name has to say so.
	//
	// Naming is a weak check, but it is the only signal available: the test
	// database and the development one are the same server, the same user and
	// the same schema, and nothing else distinguishes them.
	if !strings.Contains(dsn, "_test") {
		t.Fatalf("BUSINESS_TEST_DSN must name a database containing \"_test\": this "+
			"suite truncates orders and agreements, and %q looks like a database "+
			"somebody is using.", redactDSN(dsn))
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:  logger.Default.LogMode(logger.Silent),
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		t.Fatalf("could not connect to %s: %v", dsn, err)
	}

	// Bound the pool exactly as the service does. Without this a concurrency
	// test opens one connection per goroutine and exhausts the server's
	// max_connections, which looks like a product failure but is only the test
	// harness misbehaving. Matching production also makes the test meaningful:
	// the contention it exercises is the contention the service will see.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("could not reach the underlying pool: %v", err)
	}
	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(5)

	return db
}

// resetTables truncates everything between tests.
//
// TRUNCATE ... CASCADE rather than DELETE: it resets the tables and their
// dependants in one statement, and leaves no rows behind to make a later test's
// assertions ambiguous.
// redactDSN strips the password before a DSN reaches a test log.
func redactDSN(dsn string) string {
	if at := strings.LastIndex(dsn, "@"); at != -1 {
		if scheme := strings.Index(dsn, "://"); scheme != -1 && scheme+3 < at {
			return dsn[:scheme+3] + "***" + dsn[at:]
		}
	}
	return dsn
}

func resetTables(t *testing.T, db *gorm.DB) {
	t.Helper()

	err := db.Exec(`
		TRUNCATE TABLE
			order_status_history, shipment_documents, shipments,
			invoice_lines, invoices, ratings,
			orders, agreement_rates, agreements, number_sequences
		RESTART IDENTITY CASCADE
	`).Error
	if err != nil {
		t.Fatalf("could not reset tables: %v", err)
	}
}

// seedOrder inserts an order in a known state and returns it.
//
// It goes through the repository rather than writing the row directly, so a
// seeded order is indistinguishable from one the service created — including
// its opening history row. Bypassing the repository would make history
// assertions in later tests quietly wrong.
func seedOrder(t *testing.T, db *gorm.DB, status string) *models.Order {
	t.Helper()

	order := &models.Order{
		OrderNumber:      "ORD-TEST-" + uuid.NewString()[:8],
		ShipperCompanyID: uuid.New(),
		CreatedByUserID:  uuid.New(),
		OrderKind:        models.OrderKindStandard,
		StatusCode:       status,
		Detail:           models.JSONB{},
	}
	transporter := uuid.New()
	order.TransporterCompanyID = &transporter

	if err := repository.NewOrderRepository(db).Create(ctx(), order); err != nil {
		t.Fatalf("could not seed an order: %v", err)
	}
	return order
}

func ctx() context.Context { return context.Background() }
