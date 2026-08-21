package database

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// max_result_bytes is a conservative combined materialization/wire budget,
	// not merely the sum of SQL value lengths. These fixed allowances cover Go
	// slice/interface structures and the QueryResult JSON envelope; JSON fragment
	// lengths below account for keys, type tags, quoting, and escaping.
	queryResultBaseOverhead   int64 = 128
	queryColumnMemoryOverhead int64 = 64
	queryRowMemoryOverhead    int64 = 32
	queryCellMemoryOverhead   int64 = 64
)

// QueryExecutor executes a single query in a read-only transaction. SQL
// parsing and authorization intentionally belong to the caller; this type
// enforces database-level read-only semantics and output limits.
type QueryExecutor struct {
	db     *sql.DB
	limits Limits
}

// NewQueryExecutor constructs a bounded read-only executor.
func NewQueryExecutor(db *sql.DB, limits Limits) (*QueryExecutor, error) {
	if db == nil {
		return nil, invalid("new query executor", "nil database")
	}
	normalized, err := limits.WithDefaults()
	if err != nil {
		return nil, err
	}
	return &QueryExecutor{db: db, limits: normalized}, nil
}

// Query runs statement with driver-bound args. It never interpolates values
// into SQL and it always attempts Rollback, including success, so a read-only
// transaction cannot be accidentally left open.
func (e *QueryExecutor) Query(ctx context.Context, statement string, args ...any) (result QueryResult, err error) {
	return e.query(ctx, e.limits, statement, args...)
}

// QueryWithMaxRows applies a caller-selected row bound no larger than the
// executor's configured maximum. MCP uses this for request/default limits so
// rows are not needlessly scanned and encoded only to be trimmed later.
func (e *QueryExecutor) QueryWithMaxRows(ctx context.Context, maxRows int, statement string, args ...any) (result QueryResult, err error) {
	if maxRows <= 0 {
		return result, invalid("query", "max rows must be positive")
	}
	if maxRows > e.limits.MaxRows {
		return result, invalid("query", "max rows exceeds executor limit")
	}
	limits := e.limits
	limits.MaxRows = maxRows
	return e.query(ctx, limits, statement, args...)
}

func (e *QueryExecutor) query(ctx context.Context, limits Limits, statement string, args ...any) (result QueryResult, err error) {
	started := time.Now()
	defer func() {
		result.Elapsed = time.Since(started)
		result.ElapsedMillis = result.Elapsed.Milliseconds()
	}()

	if ctx == nil {
		return result, invalid("query", "nil context")
	}
	if strings.TrimSpace(statement) == "" {
		return result, invalid("query", "empty statement")
	}

	queryContext, cancel := context.WithTimeout(ctx, e.limits.QueryTimeout)
	defer cancel()

	tx, err := e.db.BeginTx(queryContext, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, wrapDatabaseError("begin read-only transaction", err)
	}
	defer func() {
		// Rollback returns sql.ErrTxDone after a completed transaction. There is
		// no completed transaction on this path, but ignoring rollback errors is
		// still preferable to replacing the primary query/scan failure.
		_ = tx.Rollback()
	}()

	rows, err := tx.QueryContext(queryContext, statement, args...)
	if err != nil {
		return result, wrapDatabaseError("query", err)
	}
	result, err = scanRows(rows, limits)
	if err != nil {
		return result, wrapDatabaseError("read query result", err)
	}
	return result, nil
}

// scanRows materializes a bounded result. It is shared by query execution and
// stored-function calls, which need different transaction commit semantics.
func scanRows(rows *sql.Rows, limits Limits) (QueryResult, error) {
	result := QueryResult{Columns: []Column{}, Rows: [][]Cell{}}
	if rows == nil {
		return result, invalid("scan rows", "nil rows")
	}

	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		_ = rows.Close()
		return result, err
	}
	columnCount := len(columnTypes)
	visibleColumns := columnCount
	if visibleColumns > limits.MaxColumns {
		visibleColumns = limits.MaxColumns
		result.markTruncated(TruncatedMaxColumns)
	}

	resultBytes := queryResultBaseOverhead
	if resultBytes > limits.MaxResultBytes {
		result.markTruncated(TruncatedMaxResultBytes)
		if err := rows.Close(); err != nil {
			return result, err
		}
		return result, nil
	}
	result.Columns = make([]Column, 0, visibleColumns)
	for _, columnType := range columnTypes[:visibleColumns] {
		column := Column{
			Name:         columnType.Name(),
			DatabaseType: strings.ToUpper(columnType.DatabaseTypeName()),
		}
		if nullable, ok := columnType.Nullable(); ok {
			column.Nullable = &nullable
		}
		encodedColumn, marshalErr := json.Marshal(column)
		if marshalErr != nil {
			_ = rows.Close()
			return result, marshalErr
		}
		columnBytes := int64(len(encodedColumn)) + queryColumnMemoryOverhead
		if exceedsResultBudget(resultBytes, columnBytes, limits.MaxResultBytes) {
			result.markTruncated(TruncatedMaxResultBytes)
			if err := rows.Close(); err != nil {
				return result, err
			}
			return result, nil
		}
		resultBytes += columnBytes
		result.Columns = append(result.Columns, column)
	}
	if columnCount > limits.MaxColumns {
		// A database/sql driver must materialize every physical value before Scan.
		// Closing here avoids allocating unbounded hidden cells merely to return a
		// visible prefix of an over-wide result.
		if err := rows.Close(); err != nil {
			return result, err
		}
		return result, nil
	}

	// Scan must receive a destination for every physical column.
	values := make([]any, columnCount)
	destinations := make([]any, columnCount)
	for i := range values {
		destinations[i] = &values[i]
	}

	stop := false
	for rows.Next() {
		if len(result.Rows) >= limits.MaxRows {
			result.markTruncated(TruncatedMaxRows)
			break
		}
		if err := rows.Scan(destinations...); err != nil {
			_ = rows.Close()
			return result, err
		}

		encodedRow := make([]Cell, 0, visibleColumns)
		rowBytes := queryRowMemoryOverhead + 2 // JSON array brackets.
		for i := 0; i < visibleColumns; i++ {
			cell, sourceBytes, _, err := encodeCell(values[i], columnTypes[i].DatabaseTypeName())
			if err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("column %q: %w", columnTypes[i].Name(), err)
			}
			if sourceBytes > int64(limits.MaxCellBytes) {
				result.markTruncated(TruncatedMaxCellBytes)
				stop = true
				break
			}
			encodedCell, marshalErr := json.Marshal(cell)
			if marshalErr != nil {
				_ = rows.Close()
				return result, fmt.Errorf("encode column %q for result budget: %w", columnTypes[i].Name(), marshalErr)
			}
			cellBytes := int64(len(encodedCell)) + queryCellMemoryOverhead + 1 // comma allowance.
			if exceedsResultBudget(resultBytes, rowBytes+cellBytes, limits.MaxResultBytes) {
				result.markTruncated(TruncatedMaxResultBytes)
				stop = true
				break
			}
			rowBytes += cellBytes
			encodedRow = append(encodedRow, cell)
		}
		if stop {
			break
		}
		resultBytes += rowBytes
		result.Rows = append(result.Rows, encodedRow)
	}

	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	return result, nil
}

func exceedsResultBudget(current, additional, maximum int64) bool {
	return additional < 0 || current > maximum || additional > maximum-current
}

func encodeCell(value any, databaseType string) (Cell, int64, int64, error) {
	if value == nil {
		return Cell{Type: CellNull, Value: nil}, 0, 4, nil
	}

	typeName := normalizeDatabaseType(databaseType)
	switch typed := value.(type) {
	case bool:
		encoded := strconv.FormatBool(typed)
		return Cell{Type: CellBoolean, Value: typed}, int64(len(encoded)), int64(len(encoded)), nil
	case int:
		return integerCell(strconv.FormatInt(int64(typed), 10))
	case int8:
		return integerCell(strconv.FormatInt(int64(typed), 10))
	case int16:
		return integerCell(strconv.FormatInt(int64(typed), 10))
	case int32:
		return integerCell(strconv.FormatInt(int64(typed), 10))
	case int64:
		return integerCell(strconv.FormatInt(typed, 10))
	case uint:
		return integerCell(strconv.FormatUint(uint64(typed), 10))
	case uint8:
		return integerCell(strconv.FormatUint(uint64(typed), 10))
	case uint16:
		return integerCell(strconv.FormatUint(uint64(typed), 10))
	case uint32:
		return integerCell(strconv.FormatUint(uint64(typed), 10))
	case uint64:
		return integerCell(strconv.FormatUint(typed, 10))
	case float32:
		value := strconv.FormatFloat(float64(typed), 'g', -1, 32)
		return Cell{Type: CellFloat, Value: value}, int64(len(value)), int64(len(value)), nil
	case float64:
		value := strconv.FormatFloat(typed, 'g', -1, 64)
		return Cell{Type: CellFloat, Value: value}, int64(len(value)), int64(len(value)), nil
	case time.Time:
		value := typed.Format(time.RFC3339Nano)
		return Cell{Type: CellTime, Value: value}, int64(len(value)), int64(len(value)), nil
	case string:
		return encodeStringLike(typed, typeName)
	case []byte:
		return encodeBytesLike(typed, typeName)
	case sql.RawBytes:
		return encodeBytesLike([]byte(typed), typeName)
	default:
		return Cell{}, 0, 0, fmt.Errorf("unsupported driver value %T", value)
	}
}

func integerCell(value string) (Cell, int64, int64, error) {
	size := int64(len(value))
	return Cell{Type: CellInteger, Value: value}, size, size, nil
}

func encodeStringLike(value, typeName string) (Cell, int64, int64, error) {
	size := int64(len(value))
	switch {
	case isIntegerType(typeName):
		return Cell{Type: CellInteger, Value: value}, size, size, nil
	case isDecimalType(typeName):
		return Cell{Type: CellDecimal, Value: value}, size, size, nil
	case isFloatType(typeName):
		return Cell{Type: CellFloat, Value: value}, size, size, nil
	case isTemporalType(typeName):
		return Cell{Type: CellTime, Value: value}, size, size, nil
	default:
		return Cell{Type: CellString, Value: value}, size, size, nil
	}
}

func encodeBytesLike(value []byte, typeName string) (Cell, int64, int64, error) {
	sourceSize := int64(len(value))
	if isIntegerType(typeName) {
		text := string(value)
		return Cell{Type: CellInteger, Value: text}, sourceSize, int64(len(text)), nil
	}
	if isDecimalType(typeName) {
		text := string(value)
		return Cell{Type: CellDecimal, Value: text}, sourceSize, int64(len(text)), nil
	}
	if isFloatType(typeName) {
		text := string(value)
		return Cell{Type: CellFloat, Value: text}, sourceSize, int64(len(text)), nil
	}
	if isTemporalType(typeName) {
		text := string(value)
		return Cell{Type: CellTime, Value: text}, sourceSize, int64(len(text)), nil
	}
	if isTextType(typeName) && utf8.Valid(value) {
		text := string(value)
		return Cell{Type: CellString, Value: text}, sourceSize, int64(len(text)), nil
	}
	encoded := base64.StdEncoding.EncodeToString(value)
	return Cell{Type: CellBytes, Value: encoded, Encoding: "base64"}, sourceSize, int64(len(encoded)), nil
}

func normalizeDatabaseType(databaseType string) string {
	typeName := strings.ToUpper(strings.TrimSpace(databaseType))
	if index := strings.IndexByte(typeName, '('); index >= 0 {
		typeName = typeName[:index]
	}
	return typeName
}

func isIntegerType(typeName string) bool {
	typeName = normalizeDatabaseType(typeName)
	typeName = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(typeName, "UNSIGNED "), " UNSIGNED"))
	switch typeName {
	case "TINYINT", "SMALLINT", "MEDIUMINT", "INT", "INTEGER", "BIGINT", "YEAR", "SERIAL":
		return true
	default:
		return false
	}
}

func isDecimalType(typeName string) bool {
	typeName = normalizeDatabaseType(typeName)
	return typeName == "DECIMAL" || typeName == "NUMERIC" || typeName == "NEWDECIMAL"
}

func isFloatType(typeName string) bool {
	typeName = normalizeDatabaseType(typeName)
	return typeName == "FLOAT" || typeName == "DOUBLE" || typeName == "REAL"
}

func isTemporalType(typeName string) bool {
	typeName = normalizeDatabaseType(typeName)
	return typeName == "DATE" || typeName == "DATETIME" || typeName == "TIMESTAMP" || typeName == "TIME"
}

func isTextType(typeName string) bool {
	typeName = normalizeDatabaseType(typeName)
	switch typeName {
	case "CHAR", "VARCHAR", "TINYTEXT", "TEXT", "MEDIUMTEXT", "LONGTEXT", "ENUM", "SET", "JSON":
		return true
	default:
		return false
	}
}
