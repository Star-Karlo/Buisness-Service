//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/karlo/business-service/internal/archive"
	"github.com/karlo/business-service/internal/models"
)

// memStore stands in for S3: a map, plus a "frozen" switch to act out
// Glacier's thaw cycle.
type memStore struct {
	objects map[string][]byte
	frozen  bool
}

func (m *memStore) PutCold(_ context.Context, key string, body []byte, _ string) (string, error) {
	m.objects[key] = append([]byte(nil), body...)
	return "DEEP_ARCHIVE", nil
}

func (m *memStore) GetCold(_ context.Context, key string) ([]byte, error) {
	if m.frozen {
		return nil, errFrozen
	}
	b, ok := m.objects[key]
	if !ok {
		return nil, errMissing
	}
	return b, nil
}

type stringErr string

func (e stringErr) Error() string { return string(e) }

const (
	errFrozen  = stringErr("frozen")
	errMissing = stringErr("missing")
)

// TestArchiveRoundTrip: a completed order that went cold two years ago
// leaves the hot tables as one bundle, is catalogued, and comes back intact.
func TestArchiveRoundTrip(t *testing.T) {
	db := testDB(t)
	resetTables(t, db)
	db.Exec("TRUNCATE TABLE archive_catalog")

	old := seedOrder(t, db, models.OrderCompleted)
	fresh := seedOrder(t, db, models.OrderCompleted)
	open := seedOrder(t, db, models.OrderApproved)

	// Children that must travel with the order.
	if err := db.Exec(`INSERT INTO order_status_history (order_id, from_status_code, to_status_code, source) VALUES (?, 'approved', 'completed', 'user')`, old.ID).Error; err != nil {
		t.Fatalf("seed history: %v", err)
	}
	if err := db.Exec(`INSERT INTO shipments (order_id, status_code) VALUES (?, 'finished')`, old.ID).Error; err != nil {
		t.Fatalf("seed shipment: %v", err)
	}
	// Push the old order past the retention window; the trigger would
	// otherwise stamp now() on every update.
	if err := db.Exec(`ALTER TABLE orders DISABLE TRIGGER trg_orders_updated_at`).Error; err != nil {
		t.Fatalf("disable trigger: %v", err)
	}
	if err := db.Exec(`UPDATE orders SET updated_at = now() - interval '800 days' WHERE id = ?`, old.ID).Error; err != nil {
		t.Fatalf("age order: %v", err)
	}
	if err := db.Exec(`UPDATE orders SET updated_at = now() - interval '800 days' WHERE id = ?`, open.ID).Error; err != nil {
		t.Fatalf("age open order: %v", err)
	}
	if err := db.Exec(`ALTER TABLE orders ENABLE TRIGGER trg_orders_updated_at`).Error; err != nil {
		t.Fatalf("enable trigger: %v", err)
	}

	store := &memStore{objects: map[string][]byte{}}
	a := archive.New(db, store, archive.Options{RetainFor: 730 * 24 * time.Hour, Batch: 10})

	sum, err := a.Run(ctx())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if sum.Orders != 1 || sum.Failed != 0 {
		t.Fatalf("expected exactly the old completed order archived, got %+v", sum)
	}
	if len(store.objects) != 1 {
		t.Fatalf("expected one object in the store, got %d", len(store.objects))
	}

	var n int64
	db.Table("orders").Where("id = ?", old.ID).Count(&n)
	if n != 0 {
		t.Fatal("archived order still in the hot table")
	}
	db.Table("shipments").Where("order_id = ?", old.ID).Count(&n)
	if n != 0 {
		t.Fatal("archived order's shipment still in the hot table")
	}
	for _, id := range []uuid.UUID{fresh.ID, open.ID} {
		db.Table("orders").Where("id = ?", id).Count(&n)
		if n != 1 {
			t.Fatalf("order %s should not have been archived", id)
		}
	}
	var cat struct {
		SizeBytes int64
		Reference string
	}
	if err := db.Table("archive_catalog").Select("size_bytes, reference").Where("entity = 'order' AND entity_id = ?", old.ID).Scan(&cat).Error; err != nil || cat.SizeBytes == 0 {
		t.Fatalf("catalogue row missing or empty: %v %+v", err, cat)
	}
	if cat.Reference != old.OrderNumber {
		t.Fatalf("catalogue reference %q, want %q", cat.Reference, old.OrderNumber)
	}

	// A second run finds nothing new.
	sum, err = a.Run(ctx())
	if err != nil || sum.Orders != 0 {
		t.Fatalf("second run should be a no-op: %+v %v", sum, err)
	}

	// Restore: frozen first, then readable.
	store.frozen = true
	if err := a.Restore(ctx(), archive.EntityOrder, old.ID); err == nil {
		t.Fatal("restore from a frozen object should report it")
	}
	store.frozen = false
	if err := a.Restore(ctx(), archive.EntityOrder, old.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	var back models.Order
	if err := db.First(&back, "id = ?", old.ID).Error; err != nil {
		t.Fatalf("restored order not found: %v", err)
	}
	if back.OrderNumber != old.OrderNumber || back.StatusCode != models.OrderCompleted {
		t.Fatalf("restored order differs: %+v", back)
	}
	db.Table("shipments").Where("order_id = ?", old.ID).Count(&n)
	if n != 1 {
		t.Fatal("restored order's shipment missing")
	}
	// Two rows: the one the repository wrote on create, and the one seeded.
	db.Table("order_status_history").Where("order_id = ?", old.ID).Count(&n)
	if n != 2 {
		t.Fatalf("restored order's history has %d rows, want 2", n)
	}
	if err := a.Restore(ctx(), archive.EntityOrder, old.ID); err == nil {
		t.Fatal("restoring twice should be refused")
	}
}
