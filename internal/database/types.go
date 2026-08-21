// Package database contains the MySQL-facing service layer.
//
// The package deliberately has no dependency on MCP or on the application's
// configuration model. Callers are responsible for SQL classification and
// authorization; this package is responsible for safe database execution,
// bounded result encoding, metadata access, and fixed monitoring queries.
package database

import (
	"fmt"
	"time"
)

const (
	defaultQueryTimeout   = 15 * time.Second
	defaultMaxRows        = 1_000
	defaultMaxColumns     = 256
	defaultMaxCellBytes   = 1 << 20 // 1 MiB
	defaultMaxResultBytes = 4 << 20 // 4 MiB
)

// Limits bounds both the time spent querying MySQL and the amount of data
// materialized in memory. A zero field selects the conservative default. A
// negative value is invalid, because silently turning a negative limit into an
// unlimited query would be unsafe.
type Limits struct {
	QueryTimeout   time.Duration `json:"query_timeout" yaml:"query_timeout"`
	MaxRows        int           `json:"max_rows" yaml:"max_rows"`
	MaxColumns     int           `json:"max_columns" yaml:"max_columns"`
	MaxCellBytes   int           `json:"max_cell_bytes" yaml:"max_cell_bytes"`
	MaxResultBytes int64         `json:"max_result_bytes" yaml:"max_result_bytes"`
}

// WithDefaults validates l and fills zero-valued fields with safe defaults.
func (l Limits) WithDefaults() (Limits, error) {
	if l.QueryTimeout < 0 {
		return Limits{}, fmt.Errorf("query timeout: %w", ErrInvalidArgument)
	}
	if l.MaxRows < 0 {
		return Limits{}, fmt.Errorf("max rows: %w", ErrInvalidArgument)
	}
	if l.MaxColumns < 0 {
		return Limits{}, fmt.Errorf("max columns: %w", ErrInvalidArgument)
	}
	if l.MaxCellBytes < 0 {
		return Limits{}, fmt.Errorf("max cell bytes: %w", ErrInvalidArgument)
	}
	if l.MaxResultBytes < 0 {
		return Limits{}, fmt.Errorf("max result bytes: %w", ErrInvalidArgument)
	}

	if l.QueryTimeout == 0 {
		l.QueryTimeout = defaultQueryTimeout
	}
	if l.MaxRows == 0 {
		l.MaxRows = defaultMaxRows
	}
	if l.MaxColumns == 0 {
		l.MaxColumns = defaultMaxColumns
	}
	if l.MaxCellBytes == 0 {
		l.MaxCellBytes = defaultMaxCellBytes
	}
	if l.MaxResultBytes == 0 {
		l.MaxResultBytes = defaultMaxResultBytes
	}
	return l, nil
}

// CellType describes the lossless, JSON-friendly representation of a MySQL
// value. Integers and decimals are strings so JavaScript clients cannot lose
// precision. Binary values are base64 strings.
type CellType string

const (
	CellNull    CellType = "null"
	CellBoolean CellType = "boolean"
	CellInteger CellType = "integer"
	CellDecimal CellType = "decimal"
	CellFloat   CellType = "float"
	CellString  CellType = "string"
	CellTime    CellType = "time"
	CellBytes   CellType = "bytes"
)

// Cell is a stable wire representation of a database value.
type Cell struct {
	Type     CellType `json:"type"`
	Value    any      `json:"value"`
	Encoding string   `json:"encoding,omitempty"`
}

// Column describes one returned column. Nullable is nil when the driver does
// not expose nullability metadata.
type Column struct {
	Name         string `json:"name"`
	DatabaseType string `json:"database_type,omitempty"`
	Nullable     *bool  `json:"nullable,omitempty"`
}

// TruncationReason is machine-readable so an MCP adapter can distinguish an
// intentionally bounded result from an incomplete result caused by an error.
type TruncationReason string

const (
	TruncatedMaxRows        TruncationReason = "max_rows"
	TruncatedMaxColumns     TruncationReason = "max_columns"
	TruncatedMaxCellBytes   TruncationReason = "max_cell_bytes"
	TruncatedMaxResultBytes TruncationReason = "max_result_bytes"
)

// QueryResult is returned by read queries, fixed monitoring queries, and
// stored-function calls.
type QueryResult struct {
	Columns       []Column         `json:"columns"`
	Rows          [][]Cell         `json:"rows"`
	Truncated     bool             `json:"truncated"`
	Reason        TruncationReason `json:"truncate_reason,omitempty"`
	Elapsed       time.Duration    `json:"-"`
	ElapsedMillis int64            `json:"elapsed_ms"`
}

func (r *QueryResult) markTruncated(reason TruncationReason) {
	r.Truncated = true
	// A column cap changes the shape but does not stop row iteration. If a later
	// row/cell/byte cap actually stops iteration, report that stronger reason.
	if r.Reason == "" || (r.Reason == TruncatedMaxColumns && reason != TruncatedMaxColumns) {
		r.Reason = reason
	}
}
