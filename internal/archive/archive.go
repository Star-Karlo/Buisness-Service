// Package archive moves aggregates that have gone cold out of Postgres and
// into S3 Glacier Deep Archive, and brings them back on request.
//
// The legacy monolith did this with a Node cron against Mongo: once a day,
// find everything that turned 730 days old today, gzip it, put it in Deep
// Archive, note it in a catalogue, optionally delete. This is the same idea
// for Postgres, with the one difference that matters in a relational store:
// an order is not one document but a tree — items, status history, routes,
// shipments and their documents and handovers, allowances, ratings — and the
// tree leaves together or not at all.
//
// Order of work in a run is invoices, then orders, then agreements, because
// each holds a RESTRICT foreign key to the next: an order cannot go while an
// invoice line names it, an agreement cannot go while an order prices
// against it. Archiving the referrer first is what lets the referent become
// eligible in the same run.
//
// Every aggregate is written before anything is deleted, and deleted in one
// transaction. A crash between the two leaves an object in S3 and the rows
// in place; the next run finds the catalogue row already present and only
// deletes. Nothing is ever lost to a half-run.
package archive

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	// RetainFor is how long after its last change an aggregate stays hot.
	// The legacy system used 730 days; a finished order is rarely opened
	// after a quarter, but disputes and audits reach back two years.
	RetainFor time.Duration
	// Batch bounds one run, so a first run against years of backlog is many
	// short runs rather than one that holds locks for an hour.
	Batch int
	// DryRun reports what would be archived and touches nothing.
	DryRun bool
	// Prefix is the key prefix inside the bucket.
	Prefix string
}

// Archiver runs the archive and restore operations.
type Archiver struct {
	db    *gorm.DB
	store Store
	opts  Options
	now   func() time.Time
}

func New(db *gorm.DB, store Store, opts Options) *Archiver {
	if opts.RetainFor <= 0 {
		opts.RetainFor = 730 * 24 * time.Hour
	}
	if opts.Batch <= 0 {
		opts.Batch = 500
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
	Bytes      int `json:"bytes"`
	Failed     int `json:"failed"`
}

// Run archives everything eligible, up to the batch size per entity.
func (a *Archiver) Run(ctx context.Context) (Summary, error) {
	var sum Summary
	cutoff := a.now().Add(-a.opts.RetainFor)

	for _, e := range []entity{invoiceEntity, orderEntity, agreementEntity} {
		ids, err := e.candidates(ctx, a.db, cutoff, a.opts.Batch)
		if err != nil {
			return sum, fmt.Errorf("archive: listing %s candidates: %w", e.name, err)
		}
		slog.InfoContext(ctx, "archive candidates", "entity", e.name, "count", len(ids), "cutoff", cutoff, "dryRun", a.opts.DryRun)
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return sum, err
			}
			n, err := a.archiveOne(ctx, e, id)
			if err != nil {
				sum.Failed++
				slog.ErrorContext(ctx, "archive failed", "entity", e.name, "id", id, "error", err)
				continue
			}
			sum.Bytes += n
			switch e.name {
			case EntityInvoice:
				sum.Invoices++
			case EntityOrder:
				sum.Orders++
			case EntityAgreement:
				sum.Agreements++
			}
		}
	}
	return sum, nil
}

// bundle is what one archived aggregate looks like on disk: the root row and
// each dependent table's rows, as the database returned them. JSON rather
// than a binary format so it is readable with nothing but gunzip, decades
// from now, by whoever has to answer a question about it.
type bundle struct {
	Entity     string                      `json:"entity"`
	ID         uuid.UUID                   `json:"id"`
	ArchivedAt time.Time                   `json:"archivedAt"`
	Tables     map[string][]map[string]any `json:"tables"`
}

func (a *Archiver) archiveOne(ctx context.Context, e entity, id uuid.UUID) (int, error) {
	// Idempotency: a catalogue row means the object is already in S3, and a
	// previous run died before deleting. Only the delete remains.
	var existing catalogRow
	err := a.db.WithContext(ctx).Where("entity = ? AND entity_id = ? AND restored_at IS NULL", e.name, id).First(&existing).Error
	if err == nil {
		if a.opts.DryRun {
			return 0, nil
		}
		return 0, a.deleteHot(ctx, e, id)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, err
	}

	b := bundle{Entity: e.name, ID: id, ArchivedAt: a.now(), Tables: map[string][]map[string]any{}}
	counts := map[string]int{}
	for _, t := range e.tables {
		rows, err := readRows(ctx, a.db, t, id)
		if err != nil {
			return 0, fmt.Errorf("reading %s: %w", t.name, err)
		}
		b.Tables[t.name] = rows
		counts[t.name] = len(rows)
	}
	root := b.Tables[e.tables[0].name]
	if len(root) != 1 {
		return 0, fmt.Errorf("expected one %s row, found %d", e.name, len(root))
	}

	raw, err := json.Marshal(b)
	if err != nil {
		return 0, err
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	sum := sha256.Sum256(buf.Bytes())

	when := b.ArchivedAt
	if ts, ok := root[0]["updated_at"].(time.Time); ok {
		when = ts
	}
	key := fmt.Sprintf("%s/%s/%04d/%02d/%s.json.gz", a.opts.Prefix, e.name+"s", when.Year(), when.Month(), id)

	if a.opts.DryRun {
		slog.InfoContext(ctx, "would archive", "entity", e.name, "id", id, "key", key, "bytes", buf.Len(), "rows", counts)
		return buf.Len(), nil
	}

	class, err := a.store.PutCold(ctx, key, buf.Bytes(), "application/gzip")
	if err != nil {
		return 0, fmt.Errorf("writing %s: %w", key, err)
	}

	countsJSON, _ := json.Marshal(counts)
	row := catalogRow{
		Entity:       e.name,
		EntityID:     id,
		CompanyID:    uuidField(root[0], e.companyColumn),
		Reference:    stringField(root[0], e.referenceColumn),
		S3Key:        key,
		StorageClass: class,
		SizeBytes:    int64(buf.Len()),
		SHA256:       hex.EncodeToString(sum[:]),
		RowCounts:    string(countsJSON),
		ArchivedAt:   b.ArchivedAt,
	}
	if ts, ok := root[0]["updated_at"].(time.Time); ok {
		row.SourceUpdatedAt = &ts
	}

	// Catalogue row and hot-row deletion in one transaction: either the
	// database says "archived, gone" or it says nothing happened.
	err = a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return deleteHotTx(tx, e, id)
	})
	if err != nil {
		return 0, fmt.Errorf("committing %s: %w", key, err)
	}
	slog.InfoContext(ctx, "archived", "entity", e.name, "id", id, "key", key, "bytes", buf.Len(), "class", class)
	return buf.Len(), nil
}

func (a *Archiver) deleteHot(ctx context.Context, e entity, id uuid.UUID) error {
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return deleteHotTx(tx, e, id)
	})
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
// thaw the object and returns ErrColdObject; a call hours later finds it
// readable and inserts the rows. The catalogue row stays, marked restored,
// so the same aggregate is not archived again the next night — it will be,
// once it goes cold again past the retention window, under a fresh key.
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

	raw, err := a.store.GetCold(ctx, row.S3Key)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != row.SHA256 {
		return fmt.Errorf("archive: %s does not match its recorded checksum", row.S3Key)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	var b bundle
	if err := json.NewDecoder(gz).Decode(&b); err != nil {
		return fmt.Errorf("archive: decoding %s: %w", row.S3Key, err)
	}

	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Parents before children, which is the order the tables are listed in.
		for _, t := range e.tables {
			for _, r := range b.Tables[t.name] {
				if err := tx.Table(t.name).Create(r).Error; err != nil {
					return fmt.Errorf("restoring %s: %w", t.name, err)
				}
			}
		}
		now := a.now()
		return tx.Model(&catalogRow{}).Where("id = ?", row.ID).Update("restored_at", now).Error
	})
}

type catalogRow struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey;default:gen_random_uuid()"`
	Entity          string
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
