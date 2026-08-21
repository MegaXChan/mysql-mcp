package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MegaXChan/mysql-mcp/internal/schemafilter"
)

const (
	listSchemasSelect = `SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME
FROM INFORMATION_SCHEMA.SCHEMATA
`
	listSchemasSQL            = listSchemasSelect + `ORDER BY SCHEMA_NAME`
	listSchemasFilteredPrefix = listSchemasSelect + `WHERE BINARY SCHEMA_NAME IN (`

	listTablesSQL = `SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE, ENGINE, TABLE_ROWS,
       DATA_LENGTH, INDEX_LENGTH, TABLE_COLLATION, TABLE_COMMENT
FROM INFORMATION_SCHEMA.TABLES
WHERE TABLE_SCHEMA = ?
ORDER BY TABLE_NAME`

	describeTableSQL = `SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE, ENGINE, TABLE_ROWS,
       DATA_LENGTH, INDEX_LENGTH, TABLE_COLLATION, TABLE_COMMENT
FROM INFORMATION_SCHEMA.TABLES
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`

	listColumnsSQL = `SELECT COLUMN_NAME, ORDINAL_POSITION, COLUMN_DEFAULT, IS_NULLABLE,
       DATA_TYPE, COLUMN_TYPE, CHARACTER_SET_NAME, COLLATION_NAME,
       COLUMN_KEY, EXTRA, COLUMN_COMMENT
FROM INFORMATION_SCHEMA.COLUMNS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
ORDER BY ORDINAL_POSITION`

	listIndexesSQL = `SELECT INDEX_NAME, NON_UNIQUE, SEQ_IN_INDEX, COLUMN_NAME, COLLATION,
       CARDINALITY, SUB_PART, INDEX_TYPE, COMMENT, INDEX_COMMENT
FROM INFORMATION_SCHEMA.STATISTICS
WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
ORDER BY INDEX_NAME, SEQ_IN_INDEX`

	listConstraintsSQL = `SELECT tc.CONSTRAINT_NAME, tc.CONSTRAINT_TYPE, kcu.COLUMN_NAME,
       kcu.ORDINAL_POSITION, kcu.POSITION_IN_UNIQUE_CONSTRAINT,
       kcu.REFERENCED_TABLE_SCHEMA, kcu.REFERENCED_TABLE_NAME,
       kcu.REFERENCED_COLUMN_NAME
FROM INFORMATION_SCHEMA.TABLE_CONSTRAINTS AS tc
LEFT JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE AS kcu
  ON kcu.CONSTRAINT_SCHEMA = tc.CONSTRAINT_SCHEMA
 AND kcu.TABLE_SCHEMA = tc.TABLE_SCHEMA
 AND kcu.TABLE_NAME = tc.TABLE_NAME
 AND kcu.CONSTRAINT_NAME = tc.CONSTRAINT_NAME
WHERE tc.TABLE_SCHEMA = ? AND tc.TABLE_NAME = ?
ORDER BY tc.CONSTRAINT_NAME, kcu.ORDINAL_POSITION`
)

// MetadataService exposes only parameterized INFORMATION_SCHEMA reads. User
// supplied schema and table names are never used as SQL syntax.
type MetadataService struct {
	db      *sql.DB
	timeout time.Duration
	maxRows int
}

type SchemaInfo struct {
	Name                string `json:"name"`
	DefaultCharacterSet string `json:"default_character_set"`
	DefaultCollation    string `json:"default_collation"`
}

type TableInfo struct {
	Schema         string  `json:"schema"`
	Name           string  `json:"name"`
	Type           string  `json:"type"`
	Engine         *string `json:"engine,omitempty"`
	EstimatedRows  *int64  `json:"estimated_rows,omitempty"`
	DataLength     *int64  `json:"data_length,omitempty"`
	IndexLength    *int64  `json:"index_length,omitempty"`
	TableCollation *string `json:"table_collation,omitempty"`
	Comment        string  `json:"comment,omitempty"`
}

type ColumnInfo struct {
	Name            string  `json:"name"`
	OrdinalPosition int64   `json:"ordinal_position"`
	Default         *string `json:"default,omitempty"`
	Nullable        bool    `json:"nullable"`
	DataType        string  `json:"data_type"`
	ColumnType      string  `json:"column_type"`
	CharacterSet    *string `json:"character_set,omitempty"`
	Collation       *string `json:"collation,omitempty"`
	Key             string  `json:"key,omitempty"`
	Extra           string  `json:"extra,omitempty"`
	Comment         string  `json:"comment,omitempty"`
}

type TableDescription struct {
	Table   TableInfo    `json:"table"`
	Columns []ColumnInfo `json:"columns"`
}

type IndexInfo struct {
	Name         string  `json:"name"`
	NonUnique    bool    `json:"non_unique"`
	SeqInIndex   int64   `json:"seq_in_index"`
	ColumnName   *string `json:"column_name,omitempty"`
	Collation    *string `json:"collation,omitempty"`
	Cardinality  *int64  `json:"cardinality,omitempty"`
	SubPart      *int64  `json:"sub_part,omitempty"`
	IndexType    string  `json:"index_type"`
	Comment      string  `json:"comment,omitempty"`
	IndexComment string  `json:"index_comment,omitempty"`
}

type ConstraintInfo struct {
	Name                       string  `json:"name"`
	Type                       string  `json:"type"`
	ColumnName                 *string `json:"column_name,omitempty"`
	OrdinalPosition            *int64  `json:"ordinal_position,omitempty"`
	PositionInUniqueConstraint *int64  `json:"position_in_unique_constraint,omitempty"`
	ReferencedTableSchema      *string `json:"referenced_table_schema,omitempty"`
	ReferencedTableName        *string `json:"referenced_table_name,omitempty"`
	ReferencedColumnName       *string `json:"referenced_column_name,omitempty"`
}

func NewMetadataService(db *sql.DB, timeout time.Duration) (*MetadataService, error) {
	return NewMetadataServiceWithMaxRows(db, timeout, defaultMaxRows)
}

// NewMetadataServiceWithMaxRows constructs a metadata reader with an explicit
// row bound. The registry requests one look-ahead row so the MCP adapter can
// report truncation without materializing an unbounded INFORMATION_SCHEMA set.
func NewMetadataServiceWithMaxRows(db *sql.DB, timeout time.Duration, maxRows int) (*MetadataService, error) {
	if db == nil {
		return nil, invalid("new metadata service", "nil database")
	}
	if timeout < 0 {
		return nil, invalid("new metadata service", "negative timeout")
	}
	if timeout == 0 {
		timeout = defaultQueryTimeout
	}
	if maxRows <= 0 {
		return nil, invalid("new metadata service", "max rows must be positive")
	}
	return &MetadataService{db: db, timeout: timeout, maxRows: maxRows}, nil
}

func (s *MetadataService) ListSchemas(ctx context.Context) ([]SchemaInfo, error) {
	return s.listSchemas(ctx, listSchemasSQL, nil)
}

// ListSchemasFiltered applies the configured schema allow-list in MySQL before
// the row bound. Filtering only after a bounded unfiltered query could omit an
// allowed schema that sorts after many disallowed schemas.
func (s *MetadataService) ListSchemasFiltered(ctx context.Context, names []string) ([]SchemaInfo, error) {
	return s.ListSchemasAllowed(ctx, names, nil)
}

// ListSchemasAllowed applies exact names and anchored glob patterns in MySQL
// before the row bound. Pattern values are converted to LIKE data and passed
// as query parameters; they are never interpolated into SQL syntax. BINARY
// keeps authorization case-sensitive on every supported MySQL host.
func (s *MetadataService) ListSchemasAllowed(
	ctx context.Context,
	names []string,
	patterns []string,
) ([]SchemaInfo, error) {
	if !schemafilter.Restricted(names, patterns) {
		return s.ListSchemas(ctx)
	}

	clauses := make([]string, 0, 1+len(patterns))
	args := make([]any, 0, len(names)+len(patterns))
	if len(names) > 0 {
		placeholders := make([]string, len(names))
		for index, name := range names {
			if err := validateIdentifier("schema", name); err != nil {
				return nil, err
			}
			args = append(args, name)
			placeholders[index] = "?"
		}
		clauses = append(clauses, "BINARY SCHEMA_NAME IN ("+strings.Join(placeholders, ",")+")")
	}
	for index, pattern := range patterns {
		if err := schemafilter.Validate(pattern); err != nil {
			return nil, invalid("list schemas", fmt.Sprintf("invalid schema pattern at index %d: %v", index, err))
		}
		clauses = append(clauses, "BINARY SCHEMA_NAME LIKE BINARY ? ESCAPE '='")
		args = append(args, schemafilter.ToSQLLike(pattern))
	}

	statement := listSchemasSelect + "WHERE " + strings.Join(clauses, " OR ") + "\nORDER BY SCHEMA_NAME"
	return s.listSchemas(ctx, statement, args)
}

func (s *MetadataService) listSchemas(ctx context.Context, statement string, args []any) ([]SchemaInfo, error) {
	queryContext, cancel, err := serviceContext(ctx, s.timeout, "list schemas")
	if err != nil {
		return nil, err
	}
	defer cancel()

	rows, err := s.db.QueryContext(queryContext, statement, args...)
	if err != nil {
		return nil, wrapDatabaseError("list schemas", err)
	}
	defer rows.Close()

	result := make([]SchemaInfo, 0)
	for rows.Next() {
		if len(result) >= s.maxRows {
			break
		}
		var schema SchemaInfo
		if err := rows.Scan(&schema.Name, &schema.DefaultCharacterSet, &schema.DefaultCollation); err != nil {
			return nil, wrapDatabaseError("scan schema metadata", err)
		}
		result = append(result, schema)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDatabaseError("iterate schema metadata", err)
	}
	return result, nil
}

func (s *MetadataService) ListTables(ctx context.Context, schema string) ([]TableInfo, error) {
	if err := validateIdentifier("schema", schema); err != nil {
		return nil, err
	}
	queryContext, cancel, err := serviceContext(ctx, s.timeout, "list tables")
	if err != nil {
		return nil, err
	}
	defer cancel()

	rows, err := s.db.QueryContext(queryContext, listTablesSQL, schema)
	if err != nil {
		return nil, wrapDatabaseError("list tables", err)
	}
	defer rows.Close()

	tables := make([]TableInfo, 0)
	for rows.Next() {
		if len(tables) >= s.maxRows {
			break
		}
		table, err := scanTable(rows)
		if err != nil {
			return nil, wrapDatabaseError("scan table metadata", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDatabaseError("iterate table metadata", err)
	}
	return tables, nil
}

func (s *MetadataService) DescribeTable(ctx context.Context, schema, table string) (TableDescription, error) {
	if err := validateIdentifier("schema", schema); err != nil {
		return TableDescription{}, err
	}
	if err := validateIdentifier("table", table); err != nil {
		return TableDescription{}, err
	}
	queryContext, cancel, err := serviceContext(ctx, s.timeout, "describe table")
	if err != nil {
		return TableDescription{}, err
	}
	defer cancel()

	info, err := scanTable(s.db.QueryRowContext(queryContext, describeTableSQL, schema, table))
	if errors.Is(err, sql.ErrNoRows) {
		return TableDescription{}, notFound("describe table", fmt.Sprintf("%s.%s", schema, table))
	}
	if err != nil {
		return TableDescription{}, wrapDatabaseError("read table metadata", err)
	}

	rows, err := s.db.QueryContext(queryContext, listColumnsSQL, schema, table)
	if err != nil {
		return TableDescription{}, wrapDatabaseError("list table columns", err)
	}
	defer rows.Close()

	columns := make([]ColumnInfo, 0)
	for rows.Next() {
		if len(columns) >= s.maxRows {
			break
		}
		var (
			column                           ColumnInfo
			defaultValue, charset, collation sql.NullString
			nullable                         string
		)
		if err := rows.Scan(
			&column.Name, &column.OrdinalPosition, &defaultValue, &nullable,
			&column.DataType, &column.ColumnType, &charset, &collation,
			&column.Key, &column.Extra, &column.Comment,
		); err != nil {
			return TableDescription{}, wrapDatabaseError("scan column metadata", err)
		}
		column.Default = stringPointer(defaultValue)
		column.CharacterSet = stringPointer(charset)
		column.Collation = stringPointer(collation)
		column.Nullable = strings.EqualFold(nullable, "YES")
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return TableDescription{}, wrapDatabaseError("iterate column metadata", err)
	}
	return TableDescription{Table: info, Columns: columns}, nil
}

func (s *MetadataService) ListIndexes(ctx context.Context, schema, table string) ([]IndexInfo, error) {
	if err := validateIdentifiers(schema, table); err != nil {
		return nil, err
	}
	queryContext, cancel, err := serviceContext(ctx, s.timeout, "list indexes")
	if err != nil {
		return nil, err
	}
	defer cancel()

	rows, err := s.db.QueryContext(queryContext, listIndexesSQL, schema, table)
	if err != nil {
		return nil, wrapDatabaseError("list indexes", err)
	}
	defer rows.Close()

	indexes := make([]IndexInfo, 0)
	for rows.Next() {
		if len(indexes) >= s.maxRows {
			break
		}
		var (
			index                 IndexInfo
			nonUnique             int64
			columnName, collation sql.NullString
			cardinality, subPart  sql.NullInt64
		)
		if err := rows.Scan(
			&index.Name, &nonUnique, &index.SeqInIndex, &columnName, &collation,
			&cardinality, &subPart, &index.IndexType, &index.Comment, &index.IndexComment,
		); err != nil {
			return nil, wrapDatabaseError("scan index metadata", err)
		}
		index.NonUnique = nonUnique != 0
		index.ColumnName = stringPointer(columnName)
		index.Collation = stringPointer(collation)
		index.Cardinality = int64Pointer(cardinality)
		index.SubPart = int64Pointer(subPart)
		indexes = append(indexes, index)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDatabaseError("iterate index metadata", err)
	}
	return indexes, nil
}

func (s *MetadataService) ListConstraints(ctx context.Context, schema, table string) ([]ConstraintInfo, error) {
	if err := validateIdentifiers(schema, table); err != nil {
		return nil, err
	}
	queryContext, cancel, err := serviceContext(ctx, s.timeout, "list constraints")
	if err != nil {
		return nil, err
	}
	defer cancel()

	rows, err := s.db.QueryContext(queryContext, listConstraintsSQL, schema, table)
	if err != nil {
		return nil, wrapDatabaseError("list constraints", err)
	}
	defer rows.Close()

	constraints := make([]ConstraintInfo, 0)
	for rows.Next() {
		if len(constraints) >= s.maxRows {
			break
		}
		var (
			constraint                                            ConstraintInfo
			columnName, referencedSchema, referencedTable, refCol sql.NullString
			ordinal, uniquePosition                               sql.NullInt64
		)
		if err := rows.Scan(
			&constraint.Name, &constraint.Type, &columnName, &ordinal, &uniquePosition,
			&referencedSchema, &referencedTable, &refCol,
		); err != nil {
			return nil, wrapDatabaseError("scan constraint metadata", err)
		}
		constraint.ColumnName = stringPointer(columnName)
		constraint.OrdinalPosition = int64Pointer(ordinal)
		constraint.PositionInUniqueConstraint = int64Pointer(uniquePosition)
		constraint.ReferencedTableSchema = stringPointer(referencedSchema)
		constraint.ReferencedTableName = stringPointer(referencedTable)
		constraint.ReferencedColumnName = stringPointer(refCol)
		constraints = append(constraints, constraint)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDatabaseError("iterate constraint metadata", err)
	}
	return constraints, nil
}

type tableScanner interface {
	Scan(dest ...any) error
}

func scanTable(scanner tableScanner) (TableInfo, error) {
	var (
		table                         TableInfo
		engine, collation             sql.NullString
		rows, dataLength, indexLength sql.NullInt64
	)
	if err := scanner.Scan(
		&table.Schema, &table.Name, &table.Type, &engine, &rows,
		&dataLength, &indexLength, &collation, &table.Comment,
	); err != nil {
		return TableInfo{}, err
	}
	table.Engine = stringPointer(engine)
	table.EstimatedRows = int64Pointer(rows)
	table.DataLength = int64Pointer(dataLength)
	table.IndexLength = int64Pointer(indexLength)
	table.TableCollation = stringPointer(collation)
	return table, nil
}

func stringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	copy := value.String
	return &copy
}

func int64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	copy := value.Int64
	return &copy
}

func serviceContext(ctx context.Context, timeout time.Duration, op string) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, invalid(op, "nil context")
	}
	queryContext, cancel := context.WithTimeout(ctx, timeout)
	return queryContext, cancel, nil
}

func validateIdentifiers(schema, table string) error {
	if err := validateIdentifier("schema", schema); err != nil {
		return err
	}
	return validateIdentifier("table", table)
}

func validateIdentifier(kind, identifier string) error {
	if identifier == "" {
		return invalid("validate identifier", kind+" is empty")
	}
	if !utf8.ValidString(identifier) {
		return invalid("validate identifier", kind+" is not valid UTF-8")
	}
	if utf8.RuneCountInString(identifier) > 64 {
		return invalid("validate identifier", kind+" exceeds MySQL's 64-character limit")
	}
	if strings.IndexByte(identifier, 0) >= 0 {
		return invalid("validate identifier", kind+" contains NUL")
	}
	return nil
}

// quoteIdentifier is used only where MySQL syntax requires an identifier (a
// stored-function invocation). Metadata filters use placeholders instead.
func quoteIdentifier(identifier string) (string, error) {
	if err := validateIdentifier("identifier", identifier); err != nil {
		return "", err
	}
	return "`" + strings.ReplaceAll(identifier, "`", "``") + "`", nil
}
