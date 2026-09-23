//go:build integration

package integration

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"github.com/karlo/business-service/internal/models"
	"github.com/karlo/business-service/internal/repository"
)

// Web-Field's storage, against a real database.
//
// The service logic is gated in Go, but the two things that can only be wrong
// at runtime live here: whether migration 000013's columns exist under the
// names the code writes, and whether the queries the PIC's session depends on
// actually select the row they mean to. A mistyped column in an UpdateFields
// map compiles, passes the unit suite, and fails at a warehouse gate.

func seedShipmentAt(t *testing.T, db *gorm.DB, status string) *models.Shipment {
	t.Helper()

	order := seedOrder(t, db, models.OrderAssigned)
	shipment := &models.Shipment{
		OrderID:    order.ID,
		StatusCode: status,
	}
	if err := db.WithContext(ctx()).Create(shipment).Error; err != nil {
		t.Fatalf("could not seed a shipment: %v", err)
	}
	return shipment
}

// issueHandover writes a handover row as the service would, and returns the
// plaintext code so the test can act as the PIC reading it off the driver's
// phone.
func issueHandover(t *testing.T, db *gorm.DB, shipmentID uuid.UUID, verified bool) (*models.ShipmentHandover, string) {
	t.Helper()

	const code = "428913"
	sum := sha256.Sum256([]byte(code))
	token := "tok-" + uuid.NewString()
	row := &models.ShipmentHandover{
		ShipmentID:    shipmentID,
		Stage:         "unloading",
		PICWhatsApp:   "6281200000000",
		CodeHash:      sum[:],
		CodeRecipient: "driver",
		MaxAttempts:   5,
		SentAt:        time.Now().UTC(),
		ExpiresAt:     time.Now().UTC().Add(10 * time.Minute),
		FieldToken:    &token,
	}
	if verified {
		now := time.Now().UTC()
		row.VerifiedAt = &now
	}
	if err := repository.NewHandoverRepository(db).Issue(ctx(), row); err != nil {
		t.Fatalf("could not issue a handover: %v", err)
	}
	return row, code
}

// LatestForStage must answer after the driver has confirmed the code, which is
// exactly when FindLive stops answering. Web-Field's whole session hangs off
// this difference.
func TestLatestForStageFindsAVerifiedHandover(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	shipment := seedShipmentAt(t, db, models.ShipmentUnloading)
	issued, _ := issueHandover(t, db, shipment.ID, true)
	repo := repository.NewHandoverRepository(db)

	if _, err := repo.FindLive(ctx(), shipment.ID, "unloading"); err == nil {
		t.Error("FindLive answered for a verified handover; the driver's code should no longer be live")
	}

	latest, err := repo.LatestForStage(ctx(), shipment.ID, "unloading")
	if err != nil {
		t.Fatalf("LatestForStage after the driver confirmed: %v", err)
	}
	if latest.ID != issued.ID {
		t.Errorf("LatestForStage returned %s, want the issued row %s", latest.ID, issued.ID)
	}
	if latest.CodeRecipient != "driver" {
		t.Errorf("codeRecipient = %q, want \"driver\"", latest.CodeRecipient)
	}
}

// The PIC's attempts are counted apart from the driver's, so a PIC fumbling
// the digits cannot lock the driver out and vice versa.
func TestFieldAttemptsAreCountedApartFromTheDrivers(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	shipment := seedShipmentAt(t, db, models.ShipmentUnloading)
	issued, _ := issueHandover(t, db, shipment.ID, true)
	repo := repository.NewHandoverRepository(db)

	for i := 0; i < 3; i++ {
		if err := repo.RecordFieldAttempt(ctx(), issued.ID); err != nil {
			t.Fatalf("RecordFieldAttempt: %v", err)
		}
	}
	if err := repo.RecordAttempt(ctx(), issued.ID); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}

	row, err := repo.LatestForStage(ctx(), shipment.ID, "unloading")
	if err != nil {
		t.Fatalf("LatestForStage: %v", err)
	}
	if row.FieldAttempts != 3 {
		t.Errorf("fieldAttempts = %d, want 3", row.FieldAttempts)
	}
	if row.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — the driver's counter must not move with the PIC's", row.Attempts)
	}
}

// MarkFieldVerified opens the session, and repeating it is harmless: a PIC who
// closes the page and comes back with the same code must get in again.
func TestMarkFieldVerifiedIsRepeatable(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	shipment := seedShipmentAt(t, db, models.ShipmentUnloading)
	issued, _ := issueHandover(t, db, shipment.ID, true)
	repo := repository.NewHandoverRepository(db)

	user := uuid.New()
	if err := repo.MarkFieldVerified(ctx(), issued.ID, &user); err != nil {
		t.Fatalf("MarkFieldVerified: %v", err)
	}
	first, _ := repo.LatestForStage(ctx(), shipment.ID, "unloading")
	if first.FieldVerifiedAt == nil {
		t.Fatal("fieldVerifiedAt is still null after verifying")
	}
	if first.FieldPICUserID == nil || *first.FieldPICUserID != user {
		t.Errorf("fieldPicUserId = %v, want the signed-in PIC %s", first.FieldPICUserID, user)
	}

	// Again, anonymously this time: the session reopens and the attribution
	// already recorded is not erased.
	if err := repo.MarkFieldVerified(ctx(), issued.ID, nil); err != nil {
		t.Fatalf("MarkFieldVerified a second time: %v", err)
	}
	second, _ := repo.LatestForStage(ctx(), shipment.ID, "unloading")
	if second.FieldPICUserID == nil || *second.FieldPICUserID != user {
		t.Errorf("fieldPicUserId = %v after an anonymous reopen, want it kept as %s", second.FieldPICUserID, user)
	}
}

// The token is unique across shipments: it is a credential, and two deliveries
// sharing one would hand each PIC the other's order sheet.
func TestFieldTokenIsUnique(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	a := seedShipmentAt(t, db, models.ShipmentUnloading)
	b := seedShipmentAt(t, db, models.ShipmentUnloading)
	issued, _ := issueHandover(t, db, a.ID, true)

	clash := &models.ShipmentHandover{
		ShipmentID:  b.ID,
		Stage:       "unloading",
		PICWhatsApp: "6281200000001",
		CodeHash:    []byte("irrelevant"),
		MaxAttempts: 5,
		SentAt:      time.Now().UTC(),
		ExpiresAt:   time.Now().UTC().Add(time.Minute),
		FieldToken:  issued.FieldToken,
	}
	if err := db.WithContext(ctx()).Create(clash).Error; err == nil {
		t.Error("a second handover took an existing field token; uq_shipment_handovers_field_token is missing")
	}
}

// FindByFieldToken is how every call after the code resolves its delivery.
func TestFindByFieldTokenResolvesTheSession(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	shipment := seedShipmentAt(t, db, models.ShipmentUnloading)
	issued, _ := issueHandover(t, db, shipment.ID, true)
	repo := repository.NewHandoverRepository(db)

	row, err := repo.FindByFieldToken(ctx(), *issued.FieldToken)
	if err != nil {
		t.Fatalf("FindByFieldToken: %v", err)
	}
	if row.ShipmentID != shipment.ID {
		t.Errorf("token resolved to shipment %s, want %s", row.ShipmentID, shipment.ID)
	}
	if _, err := repo.FindByFieldToken(ctx(), "tok-does-not-exist"); err == nil {
		t.Error("an unknown token resolved to a session")
	}
}

// The audit figures and the finalised manifest must land in migration 000013's
// columns under the names the service writes. A mistyped column here is the
// failure mode no Go test can catch.
func TestAuditFiguresAndManifestRoundTrip(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	shipment := seedShipmentAt(t, db, models.ShipmentUnloading)
	repo := repository.NewShipmentRepository(db)

	err := repo.UpdateFields(ctx(), shipment.ID, map[string]interface{}{
		"unloading_cargo_matches":     false,
		"unloading_cargo_note":        "2 koli rusak",
		"unloading_cargo_checked_at":  time.Now().UTC(),
		"unloading_cargo_checked_via": "field",
		"unloading_audit_weight_kg":   decimal.NewFromFloat(1234.5),
		"unloading_audit_volume_m3":   decimal.NewFromFloat(12.25),
		"unloading_audit_quantity":    decimal.NewFromInt(98),
		"manifest_finalized_at":       time.Now().UTC(),
		"manifest_finalized_by":       "Budi (PIC gudang)",
		"manifest_finalized_note":     "selesai dihitung 17:40",
	})
	if err != nil {
		t.Fatalf("writing the audit and the manifest: %v", err)
	}

	got, err := repo.FindByID(ctx(), shipment.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if got.UnloadingAuditWeightKg == nil || !got.UnloadingAuditWeightKg.Equal(decimal.NewFromFloat(1234.5)) {
		t.Errorf("unloadingAuditWeightKg = %v, want 1234.5", got.UnloadingAuditWeightKg)
	}
	if got.UnloadingAuditVolumeM3 == nil || !got.UnloadingAuditVolumeM3.Equal(decimal.NewFromFloat(12.25)) {
		t.Errorf("unloadingAuditVolumeM3 = %v, want 12.25", got.UnloadingAuditVolumeM3)
	}
	if got.UnloadingAuditQuantity == nil || !got.UnloadingAuditQuantity.Equal(decimal.NewFromInt(98)) {
		t.Errorf("unloadingAuditQuantity = %v, want 98", got.UnloadingAuditQuantity)
	}
	if got.ManifestFinalizedAt == nil {
		t.Error("manifestFinalizedAt is null after finalising")
	}
	if got.ManifestFinalizedBy == nil || *got.ManifestFinalizedBy != "Budi (PIC gudang)" {
		t.Errorf("manifestFinalizedBy = %v, want the PIC's name", got.ManifestFinalizedBy)
	}
	if got.UnloadingCargoCheckedVia == nil || *got.UnloadingCargoCheckedVia != "field" {
		t.Errorf("unloadingCargoCheckedVia = %v, want \"field\"", got.UnloadingCargoCheckedVia)
	}
}

// The inbox lists what is arriving at a company's sites, from either side of
// the order, and nothing that has not reached the unloading leg yet.
func TestListInboundForCompanyCoversBothSidesOfTheOrder(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)

	company := uuid.New()
	orders := repository.NewOrderRepository(db)
	shipments := repository.NewShipmentRepository(db)

	seed := func(status string, shipper, transporter uuid.UUID) uuid.UUID {
		order := &models.Order{
			OrderNumber:      "ORD-TEST-" + uuid.NewString()[:8],
			ShipperCompanyID: shipper,
			CreatedByUserID:  uuid.New(),
			OrderKind:        models.OrderKindStandard,
			StatusCode:       models.OrderAssigned,
			Detail:           models.JSONB{},
		}
		order.TransporterCompanyID = &transporter
		if err := orders.Create(ctx(), order); err != nil {
			t.Fatalf("seeding an order: %v", err)
		}
		shipment := &models.Shipment{OrderID: order.ID, StatusCode: status}
		if err := db.WithContext(ctx()).Create(shipment).Error; err != nil {
			t.Fatalf("seeding a shipment: %v", err)
		}
		return order.ID
	}

	asShipper := seed(models.ShipmentAtUnloading, company, uuid.New())
	asTransporter := seed(models.ShipmentUnloading, uuid.New(), company)
	seed(models.ShipmentLoading, company, uuid.New())        // not on the unloading leg
	seed(models.ShipmentAtUnloading, uuid.New(), uuid.New()) // somebody else's

	gotShipments, gotOrders, err := shipments.ListInboundForCompany(ctx(), company)
	if err != nil {
		t.Fatalf("ListInboundForCompany: %v", err)
	}
	if len(gotShipments) != 2 || len(gotOrders) != 2 {
		t.Fatalf("inbox returned %d shipments / %d orders, want 2 and 2", len(gotShipments), len(gotOrders))
	}
	seen := map[uuid.UUID]bool{}
	for i := range gotOrders {
		seen[gotOrders[i].ID] = true
		if gotShipments[i].OrderID != gotOrders[i].ID {
			t.Errorf("row %d pairs shipment of order %s with order %s; the slices are out of step",
				i, gotShipments[i].OrderID, gotOrders[i].ID)
		}
	}
	if !seen[asShipper] || !seen[asTransporter] {
		t.Errorf("inbox missed a side of the order: shipper %v, transporter %v", seen[asShipper], seen[asTransporter])
	}
}
