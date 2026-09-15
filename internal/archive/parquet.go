package archive

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/parquet-go/parquet-go"
	"gorm.io/gorm"
)

// Column typing for the Parquet files.
//
// Every column is optional, and the types are the few Parquet has that map
// onto Postgres without loss: booleans and integers as themselves, floats
// as doubles, and everything else as a string — uuid, text, timestamps
// (RFC 3339 with nanoseconds), numeric (as its decimal text, never a float
// that rounds an invoice), jsonb, dates. A string column casts back into any
// of those on restore, and Athena can CAST it for a query.
//
// The two leading underscore columns are the archiver's own: _root_id is
// the aggregate every row belongs to (a shipment_document row carries its
// order id here, which its own columns do not), and _entity says which.
const (
	colRoot   = "_root_id"
	colEntity = "_entity"
)

type colKind int

const (
	kindString colKind = iota
	kindBool
	kindInt
	kindDouble
)

type tableSchema struct {
	names []string // column names in schema (alphabetical) order
	kinds map[string]colKind
	pq    *parquet.Schema
}

// schemaFor reads the table's column types from the catalogue so the file
// carries the database's idea of a column, not the first row's. A column
// that is null in every row of a batch would otherwise have no type at all.
func schemaFor(ctx context.Context, db *gorm.DB, table string) (*tableSchema, error) {
	var cols []struct {
		ColumnName string `gorm:"column:column_name"`
		DataType   string `gorm:"column:data_type"`
	}
	err := db.WithContext(ctx).Raw(`
		SELECT column_name, data_type FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = ?`, table).Scan(&cols).Error
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("table %q has no columns", table)
	}

	kinds := map[string]colKind{colRoot: kindString, colEntity: kindString}
	group := parquet.Group{
		colRoot:   parquet.Optional(parquet.String()),
		colEntity: parquet.Optional(parquet.String()),
	}
	for _, c := range cols {
		var k colKind
		var node parquet.Node
		switch c.DataType {
		case "boolean":
			k, node = kindBool, parquet.Optional(parquet.Leaf(parquet.BooleanType))
		case "smallint", "integer", "bigint":
			k, node = kindInt, parquet.Optional(parquet.Int(64))
		case "real", "double precision":
			k, node = kindDouble, parquet.Optional(parquet.Leaf(parquet.DoubleType))
		default:
			k, node = kindString, parquet.Optional(parquet.String())
		}
		kinds[c.ColumnName] = k
		group[c.ColumnName] = node
	}

	s := parquet.NewSchema(table, group)
	names := make([]string, 0, len(s.Columns()))
	for _, path := range s.Columns() {
		names = append(names, path[0])
	}
	return &tableSchema{names: names, kinds: kinds, pq: s}, nil
}

// encode writes rows as one Parquet file. Rows are maps as the database
// returned them; values are coerced to the column's kind.
func (ts *tableSchema) encode(rows []map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	w := parquet.NewWriter(&buf, ts.pq, parquet.Compression(&parquet.Zstd))
	batch := make([]parquet.Row, 0, 256)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := w.WriteRows(batch)
		batch = batch[:0]
		return err
	}
	for _, m := range rows {
		row := make(parquet.Row, 0, len(ts.names))
		for i, name := range ts.names {
			v, err := coerce(m[name], ts.kinds[name])
			if err != nil {
				return nil, fmt.Errorf("column %s: %w", name, err)
			}
			if v == nil {
				row = append(row, parquet.NullValue().Level(0, 0, i))
			} else {
				row = append(row, parquet.ValueOf(v).Level(0, 1, i))
			}
		}
		batch = append(batch, row)
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// coerce turns a database value into the Go value the column's Parquet
// type accepts, or nil for SQL NULL.
func coerce(v any, k colKind) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch k {
	case kindBool:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("expected bool, got %T", v)
		}
		return b, nil
	case kindInt:
		switch n := v.(type) {
		case int64:
			return n, nil
		case int32:
			return int64(n), nil
		case int:
			return int64(n), nil
		case int16:
			return int64(n), nil
		}
		return nil, fmt.Errorf("expected integer, got %T", v)
	case kindDouble:
		switch f := v.(type) {
		case float64:
			return f, nil
		case float32:
			return float64(f), nil
		}
		return nil, fmt.Errorf("expected float, got %T", v)
	default:
		switch s := v.(type) {
		case string:
			return s, nil
		case []byte:
			return string(s), nil
		case time.Time:
			return s.UTC().Format(time.RFC3339Nano), nil
		case fmt.Stringer:
			return s.String(), nil
		default:
			return fmt.Sprint(v), nil
		}
	}
}

// decode reads a Parquet file back into rows keyed by column name, keeping
// only rows whose _root_id matches when rootID is non-empty. Values come
// back as string / bool / int64 / float64, which Postgres casts on insert.
func decode(data []byte, rootID string) ([]map[string]any, error) {
	r := parquet.NewReader(bytes.NewReader(data))
	defer func() { _ = r.Close() }()
	cols := r.Schema().Columns()
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c[0]
	}

	var out []map[string]any
	buf := make([]parquet.Row, 256)
	for {
		n, err := r.ReadRows(buf)
		for _, row := range buf[:n] {
			m := make(map[string]any, len(names))
			for _, v := range row {
				name := names[v.Column()]
				if v.IsNull() {
					m[name] = nil
					continue
				}
				switch v.Kind() {
				case parquet.Boolean:
					m[name] = v.Boolean()
				case parquet.Int32:
					m[name] = int64(v.Int32())
				case parquet.Int64:
					m[name] = v.Int64()
				case parquet.Double:
					m[name] = v.Double()
				case parquet.Float:
					m[name] = float64(v.Float())
				default:
					m[name] = v.String()
				}
			}
			if rootID == "" || m[colRoot] == rootID {
				out = append(out, m)
			}
		}
		if err != nil {
			break
		}
	}
	return out, nil
}

// columnsOf lists a row's user columns (everything but the archiver's own),
// sorted, for a stable INSERT.
func columnsOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != colRoot && k != colEntity {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
