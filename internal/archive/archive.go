// Package archive moves aggregates that have gone cold out of Postgres and
// into S3 Glacier Deep Archive, and brings them back on request.
//
// The legacy monolith did this with a Node cron against Mongo: once a day,
// find everything that has aged past its window, compress it, put it in
// Deep Archive, note it in a catalogue, delete. This is the same idea for
// Postgres, with two differences that matter in a relational store and a
// data-lake world:
//
//   - An order is not one document but a tree — items, status history,
//     routes, shipments and their documents and handovers, allowances,
//     ratings — and the tree leaves together or not at all.
//   - The files are Parquet, one per table per day, laid out as Hive
//     partitions (archive/orders/shipments/dt=2026-09-15/…). Athena, Spark
//     or DuckDB can query years of archived orders directly, without a
//     restore, and a column-store compresses a table of near-identical rows
//     far better than one JSON document per order.
//
// Each entity has its own retention: an order is rarely opened a year after
// delivery, an agreement is a contract someone may argue about for longer,
// an invoice sits under tax rules. See Options.Retain.
//
// Order of work in a run is invoices, then orders, then agreements, because
// each holds a RESTRICT foreign key to the next: an order cannot go while an
// invoice line names it, an agreement cannot go while an order prices
// against it. Archiving the referrer first is what lets the referent become
// eligible in the same run.
//
// A run works in chunks: read a chunk of aggregates, write every table's
// rows for that chunk to S3, then for each aggregate insert its catalogue
// row and delete its hot rows in one transaction. A crash after the write
// and before the delete leaves rows in place and a file in S3 that nothing
// points at; the next run archives them again into a fresh file. Nothing is
// ever lost to a half-run, and every catalogue row points at a file that
// exists.
package archive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Entity names the kinds of aggregate the archiver knows.
const (
	EntityOrder     = "order"
	EntityAgreement = "agreement"
	EntityInvoice   = "invoice"
)

// Store is the object store the archiver writes to. Satisfied by *storage.Client.
type Store interface {
	// PutCold writes an object in the cold storage class and returns the
	// storage class actually used.
	PutCold(ctx context.Context, key string, body []byte, contentType string) (string, error)
	// GetCold reads an object back. storage.ErrColdObject means the object
	// exists but is still frozen: a restore has been requested (or was, by
	// this call) and the caller should try again later.
	GetCold(ctx context.Context, key string) ([]byte, error)
}

// Options control one run.
type Options struct {
	// Retain is how long after its last change an aggregate of each entity
	// stays hot. Missing entries fall back to DefaultRetain.
	Retain map[string]time.Duration
	// Batch bounds one run per entity, so a first run against years of
	// backlog is many short runs rather than one that holds locks for an
	// hour.
	Batch int
	// Chunk is how many aggregates share one set of Parquet files.
	Chunk int
	// DryRun reports what would be archived and touches nothing.
	DryRun bool
	// Prefix is the key prefix inside the bucket.
	Prefix string
}

// DefaultRetain is the hot window per entity when nothing else is set.
var DefaultRetain = map[string]time.Duration{
	EntityOrder:     365 * 24 * time.Hour,  // a year after delivery
	EntityAgreement: 730 * 24 * time.Hour,  // two years after expiry
	EntityInvoice:   1095 * 24 * time.Hour, // three years after payment
}

// Archiver runs the archive and restore operations.
type Archiver struct {
	db    *gorm.DB
	store Store
	opts  Options
	now   func() time.Time
}

func New(db *gorm.DB, store Store, opts Options) *Archiver {
	if opts.Retain == nil {
		opts.Retain = map[string]time.Duration{}
	}
	for k, v := range DefaultRetain {
		if opts.Retain[k] <= 0 {
			opts.Retain[k] = v
		}
	}
	if opts.Batch <= 0 {
		opts.Batch = 500
	}
	if opts.Chunk <= 0 {
		opts.Chunk = 200
	}
	if opts.Prefix == "" {
		opts.Prefix = "archive"
	}
	return &Archiver{db: db, store: store, opts: opts, now: time.Now}
}

// Summary is what one run did.
type Summary struct {
	Invoices   int `json:"invoices"`
	Orders     int `json:"orders"`
	Agreements int `json:"agreements"`
	Files      int `json:"files"`
	Bytes      int `json:"bytes"`
	Failed     int `json:"failed"`
}

// Run archives everything eligible, up to the batch size per entity.
func (a *Archiver) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	runID := a.now().UTC().Format("20060102T150405Z")

	for _, e := range []entity{invoiceEntity, orderEntity, agreementEntity} {
		cutoff := a.now().Add(-a.opts.Retain[e.name])
		ids, err := e.candidates(ctx, a.db, cutoff, a.opts.Batch)
		if err != nil {
			return sum, fmt.Errorf("archive: listing %s candidates: %w", e.name, err)
		}
		slog.InfoContext(ctx, "archive candidates", "entity", e.name, "count", len(ids),
			"retain", a.opts.Retain[e.name].String(), "cutoff", cutoff, "dryRun", a.opts.DryRun)

		for start := 0; start < len(ids); start += a.opts.Chunk {
			if err := ctx.Err(); err != nil {
				return sum, err
			}
			end := min(start+a.opts.Chunk, len(ids))
			done, err := a.archiveChunk(ctx, e, ids[start:end], runID, &sum)
			if err != nil {
				// The whole chunk failed before any delete — S3 or the
				// catalogue read. Count them and carry on to the next
				// entity; the rows are still hot and the next run retries.
				sum.Failed += len(ids[start:end])
				slog.ErrorContext(ctx, "archive chunk failed", "entity", e.name, "from", start, "to", end, "error", err)
				continue
			}
			switch e.name {
			case EntityInvoice:
				sum.Invoices += done
			case EntityOrder:
				sum.Orders += done
			case EntityAgreement:
				sum.Agreements += done
			}
		}
	}
	return sum, nil
}

// pendingRow is one aggregate read into memory, waiting for its chunk's
// files to land before it can be committed.
type pendingRow struct {
	id     uuid.UUID
	root   map[string]any
	counts map[string]int
	// alreadyCatalogued means a previous run wrote this aggregate's files
	// and died before deleting; only the delete remains.
	alreadyCatalogued bool
}

func (a *Archiver) archiveChunk(ctx context.Context, e entity, ids []uuid.UUID, runID string, sum *Summary) (int, error) {
	date := a.now().UTC().Format("2006-01-02")
	// One buffer of rows per table, across every aggregate in the chunk.
	perTable := make(map[string][]map[string]any, len(e.tables))
	pending := make([]pendingRow, 0, len(ids))

	for _, id := range ids {
		var existing catalogRow
		err := a.db.WithContext(ctx).Where("entity = ? AND entity_id = ? AND restored_at IS NULL", e.name, id).First(&existing).Error
		if err == nil {
			pending = append(pending, pendingRow{id: id, alreadyCatalogued: true})
			continue
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, err
		}

		p := pendingRow{id: id, counts: map[string]int{}}
		for _, t := range e.tables {
			rows, err := readRows(ctx, a.db, t, id)
			if err != nil {
				return 0, fmt.Errorf("reading %s: %w", t.name, err)
			}
			for _, r := range rows {
				r[colRoot] = id.String()
				r[colEntity] = e.name
			}
			perTable[t.name] = append(perTable[t.name], rows...)
			p.counts[t.name] = len(rows)
			if t.name == e.tables[0].name {
				if len(rows) != 1 {
					return 0, fmt.Errorf("expected one %s row for %s, found %d", e.name, id, len(rows))
				}
				p.root = rows[0]
			}
		}
		pending = append(pending, p)
	}

	// Write the chunk's files: one per table that has rows.
	// archive/orders/shipments/dt=2026-09-15/20260915T190000Z.parquet
	keyFor := func(table string) string {
		return fmt.Sprintf("%s/%ss/%s/dt=%s/%s.parquet", a.opts.Prefix, e.name, table, date, runID)
	}
	rootSHA := ""
	files := 0
	for _, t := range e.tables {
		rows := perTable[t.name]
		if len(rows) == 0 {
			continue
		}
		ts, err := schemaFor(ctx, a.db, t.name)
		if err != nil {
			return 0, fmt.Errorf("schema of %s: %w", t.name, err)
		}
		data, err := ts.encode(rows)
		if err != nil {
			return 0, fmt.Errorf("encoding %s: %w", t.name, err)
		}
		if t.name == e.tables[0].name {
			sha := sha256.Sum256(data)
			rootSHA = hex.EncodeToString(sha[:])
		}
		if a.opts.DryRun {
			slog.InfoContext(ctx, "would write", "key", keyFor(t.name), "rows", len(rows), "bytes", len(data))
			sum.Bytes += len(data)
			files++
			continue
		}
		class, err := a.store.PutCold(ctx, keyFor(t.name), data, "application/vnd.apache.parquet")
		if err != nil {
			return 0, fmt.Errorf("writing %s: %w", keyFor(t.name), err)
		}
		slog.InfoContext(ctx, "archive file written", "key", keyFor(t.name), "rows", len(rows), "bytes", len(data), "class", class)
		sum.Bytes += len(data)
		files++
	}
	sum.Files += files

	if a.opts.DryRun {
		for _, p := range pending {
			slog.InfoContext(ctx, "would archive", "entity", e.name, "id", p.id, "rows", p.counts)
		}
		return len(pending), nil
	}

	// Commit each aggregate: catalogue row and hot-row deletion in one
	// transaction, so the database either says "archived, gone" or says
	// nothing happened.
	done := 0
	for _, p := range pending {
		var err error
		if p.alreadyCatalogued {
			err = a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return deleteHotTx(tx, e, p.id) })
		} else {
			countsJSON, _ := json.Marshal(p.counts)
			row := catalogRow{
				Entity:       e.name,
				EntityID:     p.id,
				CompanyID:    uuidField(p.root, e.companyColumn),
				Reference:    stringField(p.root, e.referenceColumn),
				S3Key:        keyFor("{table}"),
				StorageClass: "DEEP_ARCHIVE",
				SizeBytes:    0,
				SHA256:       rootSHA,
				RowCounts:    string(countsJSON),
				ArchivedAt:   a.now(),
			}
			if ts, ok := p.root["updated_at"].(time.Time); ok {
				row.SourceUpdatedAt = &ts
			}
			err = a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := tx.Create(&row).Error; err != nil {
					return err
				}
				return deleteHotTx(tx, e, p.id)
			})
		}
		if err != nil {
			sum.Failed++
			slog.ErrorContext(ctx, "archive commit failed", "entity", e.name, "id", p.id, "error", err)
			continue
		}
		done++
	}
	slog.InfoContext(ctx, "archived", "entity", e.name, "count", done, "files", files, "run", runID)
	return done, nil
}

// deleteHotTx deletes the root row; every dependent table cascades from it.
// Tables that RESTRICT (invoice_lines → orders, orders → agreements) are
// what the candidate queries exclude, so the delete does not trip on them.
func deleteHotTx(tx *gorm.DB, e entity, id uuid.UUID) error {
	res := tx.Exec(fmt.Sprintf("DELETE FROM %s WHERE id = ?", e.tables[0].name), id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("expected to delete one %s row, deleted %d", e.name, res.RowsAffected)
	}
	return nil
}

// Restore brings one aggregate back into the hot database.
//
// Deep Archive is not readable on demand: the first call asks Glacier to
// thaw the day's files and returns storage.ErrColdObject; a call hours
// later finds them readable, picks this aggregate's rows out of each, and
// inserts them parents-first. The catalogue row stays, marked restored, so
// the aggregate is not archived again the same night — it will be, once it
// goes cold again, into a fresh day's files.
func (a *Archiver) Restore(ctx context.Context, entityName string, id uuid.UUID) error {
	e, ok := entities[entityName]
	if !ok {
		return fmt.Errorf("archive: unknown entity %q", entityName)
	}
	var row catalogRow
	if err := a.db.WithContext(ctx).Where("entity = ? AND entity_id = ?", e.name, id).First(&row).Error; err != nil {
		return fmt.Errorf("archive: %s %s is not in the catalogue: %w", e.name, id, err)
	}
	if row.RestoredAt != nil {
		return fmt.Errorf("archive: %s %s was already restored at %s", e.name, id, row.RestoredAt)
	}

	var counts map[string]int
	_ = json.Unmarshal([]byte(row.RowCounts), &counts)

	// Ask for every file first, so one thaw request covers them all rather
	// than one per call hours apart.
	perTable := map[string][]map[string]any{}
	var cold error
	for _, t := range e.tables {
		if counts[t.name] == 0 {
			continue
		}
		key := strings.ReplaceAll(row.S3Key, "{table}", t.name)
		data, err := a.store.GetCold(ctx, key)
		if err != nil {
			if cold == nil {
				cold = err
			}
			continue
		}
		if t.name == e.tables[0].name && row.SHA256 != "" {
			sha := sha256.Sum256(data)
			if hex.EncodeToString(sha[:]) != row.SHA256 {
				return fmt.Errorf("archive: %s does not match its recorded checksum", key)
			}
		}
		rows, err := decode(data, id.String())
		if err != nil {
			return fmt.Errorf("archive: decoding %s: %w", key, err)
		}
		if len(rows) != counts[t.name] {
			return fmt.Errorf("archive: %s holds %d rows for %s, catalogue says %d", key, len(rows), id, counts[t.name])
		}
		perTable[t.name] = rows
	}
	if cold != nil {
		return cold
	}

	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Parents before children, which is the order the tables are listed in.
		for _, t := range e.tables {
			for _, r := range perTable[t.name] {
				cols := columnsOf(r)
				vals := make([]any, 0, len(cols))
				for _, c := range cols {
					vals = append(vals, r[c])
				}
				if err := insertRow(tx, t.name, cols, vals); err != nil {
					return fmt.Errorf("restoring %s: %w", t.name, err)
				}
			}
		}
		return tx.Model(&catalogRow{}).Where("id = ?", row.ID).Update("restored_at", a.now()).Error
	})
}

// insertRow writes one row by explicit column list. Values are the Parquet
// scalars; Postgres casts the strings into uuid, timestamptz, numeric and
// jsonb from the column's declared type.
func insertRow(tx *gorm.DB, table string, cols []string, vals []any) error {
	quoted := make([]string, len(cols))
	marks := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = `"` + c + `"`
		marks[i] = "?"
	}
	sql := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`, table, strings.Join(quoted, ", "), strings.Join(marks, ", "))
	return tx.Exec(sql, vals...).Error
}

type catalogRow struct {
	ID              uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	Entity          string     `gorm:"column:entity"`
	EntityID        uuid.UUID  `gorm:"type:uuid"`
	CompanyID       *uuid.UUID `gorm:"type:uuid"`
	Reference       *string
	S3Key           string `gorm:"column:s3_key"`
	StorageClass    string
	SizeBytes       int64
	SHA256          string `gorm:"column:sha256"`
	RowCounts       string `gorm:"type:jsonb"`
	SourceUpdatedAt *time.Time
	ArchivedAt      time.Time
	RestoredAt      *time.Time
}

func (catalogRow) TableName() string { return "archive_catalog" }

func uuidField(row map[string]any, col string) *uuid.UUID {
	if col == "" {
		return nil
	}
	switch v := row[col].(type) {
	case string:
		if u, err := uuid.Parse(v); err == nil {
			return &u
		}
	case [16]byte:
		u := uuid.UUID(v)
		return &u
	case []byte:
		if u, err := uuid.ParseBytes(v); err == nil {
			return &u
		}
	}
	return nil
}

func stringField(row map[string]any, col string) *string {
	if col == "" {
		return nil
	}
	if s, ok := row[col].(string); ok && s != "" {
		return &s
	}
	return nil
}
