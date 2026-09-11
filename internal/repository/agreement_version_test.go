package repository

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/karlo/business-service/internal/models"
)

func versionTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run agreement versioning against a database")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return db
}

// seedAgreement writes a version 1 and returns it, cleaning up afterwards.
func seedAgreement(t *testing.T, repo *AgreementRepository, db *gorm.DB) *models.Agreement {
	t.Helper()

	shipper, transporter := uuid.New(), uuid.New()
	a := &models.Agreement{
		AgreementNumber:      "TEST-" + uuid.NewString()[:12],
		ShipperCompanyID:     shipper,
		TransporterCompanyID: transporter,
		CreatedByUserID:      uuid.New(),
		StatusCode:           models.AgreementActive,
		ValidFrom:            time.Now().AddDate(0, -1, 0),
		ValidUntil:           time.Now().AddDate(0, 6, 0),
		Detail:               models.JSONB{},
		Rates: []models.AgreementRate{
			{Price: decimal.NewFromInt(5_000_000)},
		},
	}
	if err := repo.Create(context.Background(), a); err != nil {
		t.Fatalf("seed agreement: %v", err)
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM agreement_rates WHERE agreement_id IN (SELECT id FROM agreements WHERE root_agreement_id = ?)", a.ID)
		db.Exec("DELETE FROM agreements WHERE root_agreement_id = ?", a.ID)
	})
	return a
}

// Version 1 is the root of its own lineage. root_agreement_id is NOT NULL and
// is the row's own id, which is why the id is minted before the insert.
func TestCreateEstablishesTheLineageRoot(t *testing.T) {
	db := versionTestDB(t)
	repo := NewAgreementRepository(db)

	a := seedAgreement(t, repo, db)

	if a.Version != 1 {
		t.Errorf("version = %d, want 1", a.Version)
	}
	if a.RootAgreementID != a.ID {
		t.Errorf("root = %s, want the row's own id %s", a.RootAgreementID, a.ID)
	}
}

func TestRevisionIncrementsWithinTheLineage(t *testing.T) {
	db := versionTestDB(t)
	repo := NewAgreementRepository(db)
	ctx := context.Background()

	first := seedAgreement(t, repo, db)

	kind := models.RevisionUpdate
	second := &models.Agreement{
		AgreementNumber:      first.AgreementNumber,
		ShipperCompanyID:     first.ShipperCompanyID,
		TransporterCompanyID: first.TransporterCompanyID,
		CreatedByUserID:      uuid.New(),
		StatusCode:           models.AgreementPendingApproval,
		ValidFrom:            first.ValidFrom,
		ValidUntil:           first.ValidUntil,
		Detail:               models.JSONB{},
		RevisionKind:         &kind,
		Rates:                []models.AgreementRate{{Price: decimal.NewFromInt(5_500_000)}},
	}
	if err := repo.CreateRevision(ctx, second, first.ID); err != nil {
		t.Fatalf("CreateRevision: %v", err)
	}

	if second.Version != 2 {
		t.Errorf("version = %d, want 2", second.Version)
	}
	if second.RootAgreementID != first.ID {
		t.Errorf("root = %s, want %s", second.RootAgreementID, first.ID)
	}
	if second.SupersedesAgreementID == nil || *second.SupersedesAgreementID != first.ID {
		t.Error("the revision should name its predecessor")
	}

	// The predecessor stays live: an amendment under discussion must not stop
	// work being ordered at the agreed price.
	live, err := repo.ActiveVersion(ctx, first.ID)
	if err != nil {
		t.Fatalf("ActiveVersion: %v", err)
	}
	if live.ID != first.ID {
		t.Errorf("live version = %s, want the predecessor %s while approval is pending", live.ID, first.ID)
	}
}

// One undecided revision at a time. Two would leave the approver with no way to
// say "this one, not that one", and whichever was approved second would
// supersede the first silently.
func TestOnlyOneRevisionMayAwaitApproval(t *testing.T) {
	db := versionTestDB(t)
	repo := NewAgreementRepository(db)
	ctx := context.Background()

	first := seedAgreement(t, repo, db)
	kind := models.RevisionUpdate

	build := func(price int64) *models.Agreement {
		return &models.Agreement{
			AgreementNumber:      first.AgreementNumber,
			ShipperCompanyID:     first.ShipperCompanyID,
			TransporterCompanyID: first.TransporterCompanyID,
			CreatedByUserID:      uuid.New(),
			StatusCode:           models.AgreementPendingApproval,
			ValidFrom:            first.ValidFrom,
			ValidUntil:           first.ValidUntil,
			Detail:               models.JSONB{},
			RevisionKind:         &kind,
			Rates:                []models.AgreementRate{{Price: decimal.NewFromInt(price)}},
		}
	}

	if err := repo.CreateRevision(ctx, build(5_500_000), first.ID); err != nil {
		t.Fatalf("first revision: %v", err)
	}
	err := repo.CreateRevision(ctx, build(6_000_000), first.ID)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("second pending revision error = %v, want ErrConflict", err)
	}
}

// Exactly one live version per lineage. Approving must retire the predecessor
// in the same transaction, or an order placed in the gap could be priced
// against either.
func TestApprovalSupersedesThePredecessor(t *testing.T) {
	db := versionTestDB(t)
	repo := NewAgreementRepository(db)
	ctx := context.Background()

	first := seedAgreement(t, repo, db)
	kind := models.RevisionUpdate
	second := &models.Agreement{
		AgreementNumber:      first.AgreementNumber,
		ShipperCompanyID:     first.ShipperCompanyID,
		TransporterCompanyID: first.TransporterCompanyID,
		CreatedByUserID:      uuid.New(),
		StatusCode:           models.AgreementPendingApproval,
		ValidFrom:            first.ValidFrom,
		ValidUntil:           first.ValidUntil,
		Detail:               models.JSONB{},
		RevisionKind:         &kind,
		Rates:                []models.AgreementRate{{Price: decimal.NewFromInt(5_500_000)}},
	}
	if err := repo.CreateRevision(ctx, second, first.ID); err != nil {
		t.Fatalf("CreateRevision: %v", err)
	}

	if _, err := repo.Approve(ctx, second.ID, uuid.New(), "agreed with shipper"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	live, err := repo.ActiveVersion(ctx, first.ID)
	if err != nil {
		t.Fatalf("ActiveVersion: %v", err)
	}
	if live.ID != second.ID {
		t.Errorf("live version = %s, want the approved revision %s", live.ID, second.ID)
	}

	var retired models.Agreement
	if err := db.First(&retired, "id = ?", first.ID).Error; err != nil {
		t.Fatalf("reload predecessor: %v", err)
	}
	if retired.StatusCode != models.AgreementSuperseded {
		t.Errorf("predecessor status = %q, want superseded", retired.StatusCode)
	}
	if retired.SupersededAt == nil {
		t.Error("predecessor should carry supersededAt, which is what dates the handover")
	}
}

func TestApprovingSomethingNotPendingIsAConflict(t *testing.T) {
	db := versionTestDB(t)
	repo := NewAgreementRepository(db)

	first := seedAgreement(t, repo, db)

	_, err := repo.Approve(context.Background(), first.ID, uuid.New(), "")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("approving an active version error = %v, want ErrConflict", err)
	}
}

// The view is the PRD's "view price history", and it must show every version's
// lines rather than only the live one.
func TestPriceHistorySpansEveryVersion(t *testing.T) {
	db := versionTestDB(t)
	repo := NewAgreementRepository(db)
	ctx := context.Background()

	first := seedAgreement(t, repo, db)
	kind := models.RevisionUpdate
	second := &models.Agreement{
		AgreementNumber:      first.AgreementNumber,
		ShipperCompanyID:     first.ShipperCompanyID,
		TransporterCompanyID: first.TransporterCompanyID,
		CreatedByUserID:      uuid.New(),
		StatusCode:           models.AgreementPendingApproval,
		ValidFrom:            first.ValidFrom,
		ValidUntil:           first.ValidUntil,
		Detail:               models.JSONB{},
		RevisionKind:         &kind,
		Rates:                []models.AgreementRate{{Price: decimal.NewFromInt(5_500_000)}},
	}
	if err := repo.CreateRevision(ctx, second, first.ID); err != nil {
		t.Fatalf("CreateRevision: %v", err)
	}

	lines, err := repo.PriceHistory(ctx, first.TransporterCompanyID, first.ID)
	if err != nil {
		t.Fatalf("PriceHistory: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d price lines, want one per version", len(lines))
	}
	// Newest first.
	if lines[0].Version != 2 || lines[1].Version != 1 {
		t.Errorf("versions = %d, %d; want 2 then 1", lines[0].Version, lines[1].Version)
	}
}

// The view carries no company column, so authority is checked against the
// lineage. Without it any caller who guessed a root id could read another
// company's negotiated prices.
func TestPriceHistoryRefusesAnotherCompany(t *testing.T) {
	db := versionTestDB(t)
	repo := NewAgreementRepository(db)

	first := seedAgreement(t, repo, db)

	_, err := repo.PriceHistory(context.Background(), uuid.New(), first.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("outsider PriceHistory error = %v, want ErrNotFound", err)
	}
}

// Every model's columns must exist on its table.
//
// This exists because they briefly did not. The versioning fields were added to
// Agreement with a text edit whose anchor — the shipper/transporter id pair —
// appears identically on Invoice, so Invoice silently gained a dozen columns
// its table does not have. Nothing failed at compile time; GORM would have put
// them in the INSERT column list and every invoice write would have failed at
// runtime, on a path no unit test exercises.
//
// Asking the database directly is the only check that catches it, because the
// mistake is precisely that the Go type and the schema disagree.
func TestEveryModelColumnExistsOnItsTable(t *testing.T) {
	db := versionTestDB(t)

	for _, subject := range []struct {
		table string
		model any
	}{
		{"agreements", &models.Agreement{}},
		{"agreement_rates", &models.AgreementRate{}},
		{"orders", &models.Order{}},
		{"order_items", &models.OrderItem{}},
		{"shipments", &models.Shipment{}},
		{"invoices", &models.Invoice{}},
		{"invoice_lines", &models.InvoiceLine{}},
		{"order_allowances", &models.OrderAllowance{}},
		{"order_routes", &models.OrderRoute{}},
		{"route_cache", &models.RouteCacheEntry{}},
		{"shipment_handovers", &models.ShipmentHandover{}},
	} {
		stmt := &gorm.Statement{DB: db}
		if err := stmt.Parse(subject.model); err != nil {
			t.Errorf("%s: parse model: %v", subject.table, err)
			continue
		}

		var actual []string
		if err := db.Raw(
			`SELECT column_name FROM information_schema.columns
			 WHERE table_schema = 'public' AND table_name = ?`,
			subject.table,
		).Scan(&actual).Error; err != nil {
			t.Fatalf("%s: read schema: %v", subject.table, err)
		}

		present := make(map[string]bool, len(actual))
		for _, c := range actual {
			present[c] = true
		}

		for _, field := range stmt.Schema.Fields {
			// Fields with no column are associations or computed values.
			if field.DBName == "" || !field.Readable || !field.Creatable {
				continue
			}
			if !present[field.DBName] {
				t.Errorf("%s.%s is on the Go model but not on the table",
					subject.table, field.DBName)
			}
		}
	}
}
