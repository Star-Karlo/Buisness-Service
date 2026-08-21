//go:build integration

package integration

import (
	"sort"
	"testing"

	"github.com/karlo/business-service/internal/models"
	"gorm.io/gorm"
)

// TestModelColumnsMatchTheSchema compares what GORM will actually write against
// what the migration actually created.
//
// The bug this guards against was found in the authentication service: GORM
// derives a column name from the Go field name, so `AcceptedTnCAt` became
// `accepted_tn_c_at` while the migration declared `accepted_tnc_at`, and every
// insert failed. Field names here carry the same hazard — `WeightKg`,
// `VolumeM3`, `PPNAmount`, `PPH23Percentage` — so the pair is checked.
func TestModelColumnsMatchTheSchema(t *testing.T) {
	db := testDB(t)

	entities := []struct {
		name  string
		model interface{}
		table string
	}{
		{"Agreement", &models.Agreement{}, "agreements"},
		{"AgreementRate", &models.AgreementRate{}, "agreement_rates"},
		{"Order", &models.Order{}, "orders"},
		{"OrderStatusEntry", &models.OrderStatusEntry{}, "order_status_history"},
		{"Shipment", &models.Shipment{}, "shipments"},
		{"ShipmentDocument", &models.ShipmentDocument{}, "shipment_documents"},
		{"Invoice", &models.Invoice{}, "invoices"},
		{"InvoiceLine", &models.InvoiceLine{}, "invoice_lines"},
		{"Rating", &models.Rating{}, "ratings"},
	}

	for _, entity := range entities {
		t.Run(entity.name, func(t *testing.T) {
			actual, err := actualColumns(db, entity.table)
			if err != nil {
				t.Fatalf("could not read the schema for %s: %v", entity.table, err)
			}
			if len(actual) == 0 {
				t.Fatalf("table %s has no columns; is the migration applied?", entity.table)
			}

			expected, err := modelColumns(db, entity.model)
			if err != nil {
				t.Fatalf("could not parse the model: %v", err)
			}

			var missing []string
			for _, col := range expected {
				if !actual[col] {
					missing = append(missing, col)
				}
			}

			if len(missing) > 0 {
				sort.Strings(missing)
				t.Errorf("%s writes columns that do not exist in %s: %v\n"+
					"Add an explicit `gorm:\"column:...\"` tag, or fix the migration.",
					entity.name, entity.table, missing)
			}
		})
	}
}

func actualColumns(db *gorm.DB, table string) (map[string]bool, error) {
	var names []string
	err := db.Raw(`
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = ?
	`, table).Scan(&names).Error
	if err != nil {
		return nil, err
	}

	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

func modelColumns(db *gorm.DB, model interface{}) ([]string, error) {
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(model); err != nil {
		return nil, err
	}

	var out []string
	for _, field := range stmt.Schema.Fields {
		if field.DBName == "" {
			continue
		}
		out = append(out, field.DBName)
	}
	return out, nil
}
