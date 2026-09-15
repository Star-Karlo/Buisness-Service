package archive

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// table is one table in an aggregate and how its rows are found from the
// root id. The root is first and is the one deleted; the rest cascade.
type table struct {
	name string
	// where selects the rows for one root id; "?" is bound to the id.
	where string
}

type entity struct {
	name            string
	companyColumn   string
	referenceColumn string
	tables          []table
	// candidates lists root ids that have gone cold and are safe to delete.
	candidates func(ctx context.Context, db *gorm.DB, cutoff time.Time, limit int) ([]uuid.UUID, error)
}

// The three aggregates, in the order a run processes them.

var invoiceEntity = entity{
	name:            EntityInvoice,
	companyColumn:   "shipper_company_id",
	referenceColumn: "invoice_number",
	tables: []table{
		{"invoices", "id = ?"},
		{"invoice_lines", "invoice_id = ?"},
	},
	// Paid or cancelled, untouched for the retention window.
	candidates: func(ctx context.Context, db *gorm.DB, cutoff time.Time, limit int) ([]uuid.UUID, error) {
		return ids(ctx, db, `
			SELECT id FROM invoices
			WHERE status_code IN ('paid', 'cancelled')
			  AND updated_at < ?
			ORDER BY updated_at
			LIMIT ?`, cutoff, limit)
	},
}

var orderEntity = entity{
	name:            EntityOrder,
	companyColumn:   "shipper_company_id",
	referenceColumn: "order_number",
	tables: []table{
		{"orders", "id = ?"},
		{"order_items", "order_id = ?"},
		{"order_status_history", "order_id = ?"},
		{"order_routes", "order_id = ?"},
		{"order_allowances", "order_id = ?"},
		{"order_allowance_history", "order_id = ?"},
		{"ratings", "order_id = ?"},
		{"shipments", "order_id = ?"},
		{"shipment_documents", "shipment_id IN (SELECT id FROM shipments WHERE order_id = ?)"},
		{"shipment_handovers", "shipment_id IN (SELECT id FROM shipments WHERE order_id = ?)"},
	},
	// Terminal, cold, and not named by any invoice line still in the hot
	// database (invoice_lines RESTRICTs the delete). Child orders leave
	// with or after their parent: a parent whose children are still hot is
	// held back, and a child's parent_order_id is SET NULL by the schema if
	// the parent went first — so parents wait for their children.
	candidates: func(ctx context.Context, db *gorm.DB, cutoff time.Time, limit int) ([]uuid.UUID, error) {
		return ids(ctx, db, `
			SELECT o.id FROM orders o
			WHERE o.status_code IN ('completed', 'cancelled', 'rejected')
			  AND o.updated_at < ?
			  AND NOT EXISTS (SELECT 1 FROM invoice_lines il WHERE il.order_id = o.id)
			  AND NOT EXISTS (SELECT 1 FROM orders c WHERE c.parent_order_id = o.id)
			ORDER BY o.updated_at
			LIMIT ?`, cutoff, limit)
	},
}

var agreementEntity = entity{
	name:            EntityAgreement,
	companyColumn:   "shipper_company_id",
	referenceColumn: "agreement_number",
	tables: []table{
		{"agreements", "id = ?"},
		{"agreement_rates", "agreement_id = ?"},
		{"agreement_customers", "agreement_id = ?"},
	},
	// Terminal and cold, with no hot order priced against it and no other
	// version pointing at it (root_agreement_id / supersedes_agreement_id
	// RESTRICT). Versions therefore leave newest first, and the root last.
	candidates: func(ctx context.Context, db *gorm.DB, cutoff time.Time, limit int) ([]uuid.UUID, error) {
		return ids(ctx, db, `
			SELECT a.id FROM agreements a
			WHERE a.status_code IN ('expired', 'superseded', 'cancelled', 'rejected')
			  AND a.updated_at < ?
			  AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.agreement_id = a.id)
			  AND NOT EXISTS (SELECT 1 FROM agreements b WHERE b.id <> a.id AND (b.root_agreement_id = a.id OR b.supersedes_agreement_id = a.id))
			ORDER BY a.updated_at
			LIMIT ?`, cutoff, limit)
	},
}

var entities = map[string]entity{
	EntityInvoice:   invoiceEntity,
	EntityOrder:     orderEntity,
	EntityAgreement: agreementEntity,
}

func ids(ctx context.Context, db *gorm.DB, sql string, args ...any) ([]uuid.UUID, error) {
	var out []uuid.UUID
	err := db.WithContext(ctx).Raw(sql, args...).Scan(&out).Error
	return out, err
}

func readRows(ctx context.Context, db *gorm.DB, t table, id uuid.UUID) ([]map[string]any, error) {
	if strings.ContainsAny(t.name, " ;") {
		return nil, fmt.Errorf("bad table name %q", t.name)
	}
	var rows []map[string]any
	err := db.WithContext(ctx).Table(t.name).Where(t.where, id).Find(&rows).Error
	if rows == nil {
		rows = []map[string]any{}
	}
	return rows, err
}
