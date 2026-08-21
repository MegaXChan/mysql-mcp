package database

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestQueryExecutorEncodesMySQLValuesAndRollsBack(t *testing.T) {
	// Scenario: a successful SELECT returns every common driver value shape.
	// Risk covered: JSON clients must not lose BIGINT/DECIMAL precision, binary
	// data must not be mistaken for UTF-8, and even success must end in rollback.
	db, mock := newMockDatabase(t)
	executor, err := NewQueryExecutor(db, Limits{})
	if err != nil {
		t.Fatalf("NewQueryExecutor() error = %v", err)
	}

	when := time.Date(2026, time.August, 21, 9, 8, 7, 123, time.FixedZone("CST", 8*60*60))
	columns := []*sqlmock.Column{
		sqlmock.NewColumn("id").OfType("BIGINT", int64(0)).Nullable(false),
		sqlmock.NewColumn("amount").OfType("DECIMAL", []byte{}).Nullable(false),
		sqlmock.NewColumn("created_at").OfType("DATETIME", time.Time{}).Nullable(false),
		sqlmock.NewColumn("payload").OfType("BLOB", []byte{}).Nullable(true),
		sqlmock.NewColumn("nickname").OfType("VARCHAR", "").Nullable(true),
		sqlmock.NewColumn("missing").OfType("VARCHAR", "").Nullable(true),
	}
	rows := sqlmock.NewRowsWithColumnDefinition(columns...).
		AddRow(int64(9_007_199_254_740_993), []byte("123.4500"), when, []byte{0, 1, 2}, "alice", nil)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT * FROM values_table WHERE id = ?")).
		WithArgs(int64(7)).
		WillReturnRows(rows)
	mock.ExpectRollback()

	result, err := executor.Query(context.Background(), "SELECT * FROM values_table WHERE id = ?", int64(7))
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if len(result.Rows) != 1 || len(result.Rows[0]) != 6 {
		t.Fatalf("Query() rows = %#v, want one six-cell row", result.Rows)
	}
	assertCell(t, result.Rows[0][0], CellInteger, "9007199254740993")
	assertCell(t, result.Rows[0][1], CellDecimal, "123.4500")
	assertCell(t, result.Rows[0][2], CellTime, when.Format(time.RFC3339Nano))
	assertCell(t, result.Rows[0][3], CellBytes, base64.StdEncoding.EncodeToString([]byte{0, 1, 2}))
	assertCell(t, result.Rows[0][4], CellString, "alice")
	if result.Rows[0][5].Type != CellNull || result.Rows[0][5].Value != nil {
		t.Fatalf("NULL cell = %#v, want explicit null", result.Rows[0][5])
	}
	if result.Rows[0][3].Encoding != "base64" {
		t.Fatalf("binary encoding = %q, want base64", result.Rows[0][3].Encoding)
	}
	assertExpectations(t, mock)
}

func TestQueryExecutorTruncatesRows(t *testing.T) {
	// Scenario: MySQL has more rows than the configured response budget.
	// Risk covered: one additional row is detected without materializing it and
	// the caller receives a machine-readable, non-error truncation signal.
	db, mock := newMockDatabase(t)
	executor, err := NewQueryExecutor(db, Limits{MaxRows: 1})
	if err != nil {
		t.Fatalf("NewQueryExecutor() error = %v", err)
	}
	rows := sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("id").OfType("BIGINT", int64(0))).
		AddRow(int64(1)).
		AddRow(int64(2))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id FROM items").WillReturnRows(rows)
	mock.ExpectRollback()

	result, err := executor.Query(context.Background(), "SELECT id FROM items")
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if !result.Truncated || result.Reason != TruncatedMaxRows || len(result.Rows) != 1 {
		t.Fatalf("Query() = %#v, want one row truncated by max_rows", result)
	}
	assertExpectations(t, mock)
}

func TestQueryExecutorAppliesPerCallRowLimitBeforeEncoding(t *testing.T) {
	// The executor-wide maximum is an upper policy bound, while an MCP request
	// may ask for fewer rows. The smaller limit must reach scanRows directly so
	// extra rows are not encoded and then discarded by the adapter.
	db, mock := newMockDatabase(t)
	executor, err := NewQueryExecutor(db, Limits{MaxRows: 3})
	if err != nil {
		t.Fatalf("NewQueryExecutor() error = %v", err)
	}
	rows := sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("id").OfType("BIGINT", int64(0))).
		AddRow(int64(1)).
		AddRow(int64(2)).
		AddRow(int64(3))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT id FROM items").WillReturnRows(rows)
	mock.ExpectRollback()

	result, err := executor.QueryWithMaxRows(context.Background(), 1, "SELECT id FROM items")
	if err != nil {
		t.Fatalf("QueryWithMaxRows() error = %v", err)
	}
	if len(result.Rows) != 1 || !result.Truncated || result.Reason != TruncatedMaxRows {
		t.Fatalf("QueryWithMaxRows() = %#v, want one row and max_rows truncation", result)
	}
	assertExpectations(t, mock)
}

func TestQueryExecutorRejectsInvalidPerCallRowLimitBeforeDatabase(t *testing.T) {
	db, mock := newMockDatabase(t)
	executor, err := NewQueryExecutor(db, Limits{MaxRows: 3})
	if err != nil {
		t.Fatalf("NewQueryExecutor() error = %v", err)
	}
	for _, limit := range []int{0, -1, 4} {
		if _, err := executor.QueryWithMaxRows(context.Background(), limit, "SELECT 1"); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("QueryWithMaxRows(%d) error = %v, want ErrInvalidArgument", limit, err)
		}
	}
	assertExpectations(t, mock)
}

func TestQueryExecutorStopsBeforeScanningOverWideRows(t *testing.T) {
	// Scenario: a query returns more columns than the response allows.
	// Risk covered: the executor exposes the allowed metadata prefix but closes
	// before a driver can materialize unbounded values for hidden columns.
	db, mock := newMockDatabase(t)
	executor, err := NewQueryExecutor(db, Limits{MaxColumns: 1})
	if err != nil {
		t.Fatalf("NewQueryExecutor() error = %v", err)
	}
	rows := sqlmock.NewRowsWithColumnDefinition(
		sqlmock.NewColumn("first").OfType("VARCHAR", ""),
		sqlmock.NewColumn("second").OfType("VARCHAR", ""),
	).AddRow("visible", "hidden")
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT first, second FROM items").WillReturnRows(rows)
	mock.ExpectRollback()

	result, err := executor.Query(context.Background(), "SELECT first, second FROM items")
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if result.Reason != TruncatedMaxColumns || len(result.Columns) != 1 || len(result.Rows) != 0 {
		t.Fatalf("Query() = %#v, want first column metadata, no rows, and max_columns reason", result)
	}
	assertExpectations(t, mock)
}

func TestQueryExecutorTruncatesOversizedCellWithoutReturningPartialRow(t *testing.T) {
	// Scenario: one text value is larger than MaxCellBytes.
	// Risk covered: partial rows are never returned, which keeps the column/row
	// shape valid and prevents a single cell from exhausting process memory.
	db, mock := newMockDatabase(t)
	executor, err := NewQueryExecutor(db, Limits{MaxCellBytes: 3})
	if err != nil {
		t.Fatalf("NewQueryExecutor() error = %v", err)
	}
	rows := sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("value").OfType("VARCHAR", "")).
		AddRow("four")
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value FROM items").WillReturnRows(rows)
	mock.ExpectRollback()

	result, err := executor.Query(context.Background(), "SELECT value FROM items")
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if result.Reason != TruncatedMaxCellBytes || len(result.Rows) != 0 {
		t.Fatalf("Query() = %#v, want no partial row and max_cell_bytes reason", result)
	}
	assertExpectations(t, mock)
}

func TestQueryExecutorTruncatesAtTotalEncodedBytes(t *testing.T) {
	// Scenario: individual cells fit, but their combined encoded values do not.
	// Risk covered: the aggregate memory/result budget is enforced at row
	// boundaries, independently of the per-cell limit.
	db, mock := newMockDatabase(t)
	executor, err := NewQueryExecutor(db, Limits{MaxResultBytes: 430})
	if err != nil {
		t.Fatalf("NewQueryExecutor() error = %v", err)
	}
	rows := sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("value").OfType("VARCHAR", "")).
		AddRow("123").
		AddRow("456")
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value FROM items").WillReturnRows(rows)
	mock.ExpectRollback()

	result, err := executor.Query(context.Background(), "SELECT value FROM items")
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if result.Reason != TruncatedMaxResultBytes || len(result.Rows) != 1 {
		t.Fatalf("Query() = %#v, want first row and max_result_bytes reason", result)
	}
	assertExpectations(t, mock)
}

func TestQueryExecutorCountsNullAndStructureOverhead(t *testing.T) {
	// NULL has no source payload, but each returned Cell/row and its JSON fields
	// still consume memory and response bytes. A value-only counter would allow
	// this entire result through a tiny max_result_bytes budget.
	db, mock := newMockDatabase(t)
	executor, err := NewQueryExecutor(db, Limits{MaxRows: 100, MaxResultBytes: 500})
	if err != nil {
		t.Fatalf("NewQueryExecutor() error = %v", err)
	}
	rows := sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn("optional_value").OfType("VARCHAR", "").Nullable(true))
	for range 20 {
		rows.AddRow(nil)
	}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT optional_value FROM items").WillReturnRows(rows)
	mock.ExpectRollback()

	result, err := executor.Query(context.Background(), "SELECT optional_value FROM items")
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if !result.Truncated || result.Reason != TruncatedMaxResultBytes || len(result.Rows) >= 20 {
		t.Fatalf("Query() = %#v, want structural-overhead truncation", result)
	}
	assertExpectations(t, mock)
}

func TestQueryExecutorRollsBackAfterQueryFailure(t *testing.T) {
	// Scenario: MySQL rejects a query after the transaction has begun.
	// Risk covered: the connection is returned without an open transaction and
	// account details in permission errors are redacted.
	db, mock := newMockDatabase(t)
	executor, err := NewQueryExecutor(db, Limits{})
	if err != nil {
		t.Fatalf("NewQueryExecutor() error = %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT secret FROM restricted").
		WillReturnError(errors.New("SELECT command denied to user 'mcp'@'10.0.0.9'"))
	mock.ExpectRollback()

	_, err = executor.Query(context.Background(), "SELECT secret FROM restricted")
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("Query() error = %v, want ErrPermissionDenied", err)
	}
	if regexp.MustCompile(`'mcp'@'10\.0\.0\.9'`).MatchString(err.Error()) {
		t.Fatalf("Query() error leaked database account: %v", err)
	}
	assertExpectations(t, mock)
}

func TestQueryExecutorHonorsTimeout(t *testing.T) {
	// Scenario: the driver does not return before QueryTimeout.
	// Risk covered: service deadlines cancel in-flight work instead of tying up a
	// datasource pool indefinitely.
	db, mock := newMockDatabase(t)
	executor, err := NewQueryExecutor(db, Limits{QueryTimeout: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("NewQueryExecutor() error = %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT SLEEP").WillDelayFor(50 * time.Millisecond).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(1))

	_, err = executor.Query(context.Background(), "SELECT SLEEP(1)")
	if err == nil {
		t.Fatal("Query() error = nil, want deadline/cancellation error")
	}
	assertExpectations(t, mock)
}

func TestLimitsRejectNegativeValues(t *testing.T) {
	// Scenario: configuration supplies a negative limit.
	// Risk covered: an invalid value cannot be interpreted as unlimited access.
	_, err := (Limits{MaxRows: -1}).WithDefaults()
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("WithDefaults() error = %v, want ErrInvalidArgument", err)
	}
}

func newMockDatabase(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

func assertCell(t *testing.T, cell Cell, wantType CellType, wantValue any) {
	t.Helper()
	if cell.Type != wantType || cell.Value != wantValue {
		t.Fatalf("cell = %#v, want type %q value %#v", cell, wantType, wantValue)
	}
}

func assertExpectations(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}
