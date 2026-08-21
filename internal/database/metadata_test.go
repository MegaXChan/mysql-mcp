package database

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMetadataServiceListsSchemasAndTables(t *testing.T) {
	// Scenario: an MCP client browses schemas and tables before issuing a query.
	// Risk covered: nullable INFORMATION_SCHEMA estimates are represented as nil
	// rather than fabricated zero values.
	db, mock := newMockDatabase(t)
	service, err := NewMetadataService(db, time.Second)
	if err != nil {
		t.Fatalf("NewMetadataService() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(listSchemasSQL)).WillReturnRows(
		sqlmock.NewRows([]string{"SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME", "DEFAULT_COLLATION_NAME"}).
			AddRow("app", "utf8mb4", "utf8mb4_0900_ai_ci"),
	)
	mock.ExpectQuery(regexp.QuoteMeta(listTablesSQL)).WithArgs("app").WillReturnRows(
		sqlmock.NewRows([]string{
			"TABLE_SCHEMA", "TABLE_NAME", "TABLE_TYPE", "ENGINE", "TABLE_ROWS",
			"DATA_LENGTH", "INDEX_LENGTH", "TABLE_COLLATION", "TABLE_COMMENT",
		}).AddRow("app", "events", "BASE TABLE", "InnoDB", nil, int64(1024), int64(512), "utf8mb4_0900_ai_ci", "event stream"),
	)

	schemas, err := service.ListSchemas(context.Background())
	if err != nil || len(schemas) != 1 || schemas[0].Name != "app" {
		t.Fatalf("ListSchemas() = %#v, %v", schemas, err)
	}
	tables, err := service.ListTables(context.Background(), "app")
	if err != nil {
		t.Fatalf("ListTables() error = %v", err)
	}
	if len(tables) != 1 || tables[0].EstimatedRows != nil || tables[0].DataLength == nil || *tables[0].DataLength != 1024 {
		t.Fatalf("ListTables() = %#v, want nullable estimate and data length", tables)
	}
	assertExpectations(t, mock)
}

func TestMetadataServiceFiltersAllowedSchemasBeforeRowBound(t *testing.T) {
	// Scenario: many visible but disallowed schemas sort before the configured
	// allow-list. Risk covered: applying the row bound before filtering would
	// return an empty/incomplete list even though allowed schemas exist.
	db, mock := newMockDatabase(t)
	service, err := NewMetadataServiceWithMaxRows(db, time.Second, 3)
	if err != nil {
		t.Fatalf("NewMetadataServiceWithMaxRows() error = %v", err)
	}
	statement := listSchemasFilteredPrefix + "?,?)\nORDER BY SCHEMA_NAME"
	mock.ExpectQuery(regexp.QuoteMeta(statement)).WithArgs("z_allowed", "A_allowed").WillReturnRows(
		sqlmock.NewRows([]string{"SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME", "DEFAULT_COLLATION_NAME"}).
			AddRow("A_allowed", "utf8mb4", "utf8mb4_bin").
			AddRow("z_allowed", "utf8mb4", "utf8mb4_bin"),
	)

	schemas, err := service.ListSchemasFiltered(context.Background(), []string{"z_allowed", "A_allowed"})
	if err != nil || len(schemas) != 2 {
		t.Fatalf("ListSchemasFiltered() = %#v, %v; want two allowed schemas", schemas, err)
	}
	assertExpectations(t, mock)
}

func TestMetadataServiceFiltersAllowedSchemaPatternsBeforeRowBound(t *testing.T) {
	// Scenario: the policy combines one exact schema with a *_dev glob while
	// many unrelated schemas are visible. Risk covered: pattern filtering must
	// happen in MySQL before the bounded collector, and the pattern must remain
	// a bound value rather than becoming executable SQL syntax.
	db, mock := newMockDatabase(t)
	service, err := NewMetadataServiceWithMaxRows(db, time.Second, 3)
	if err != nil {
		t.Fatalf("NewMetadataServiceWithMaxRows() error = %v", err)
	}
	statement := listSchemasSelect +
		"WHERE BINARY SCHEMA_NAME IN (?) OR BINARY SCHEMA_NAME LIKE BINARY ? ESCAPE '='\n" +
		"ORDER BY SCHEMA_NAME"
	mock.ExpectQuery(regexp.QuoteMeta(statement)).
		WithArgs("shared", "%=_dev").
		WillReturnRows(
			sqlmock.NewRows([]string{"SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME", "DEFAULT_COLLATION_NAME"}).
				AddRow("analytics_dev", "utf8mb4", "utf8mb4_bin").
				AddRow("shared", "utf8mb4", "utf8mb4_bin"),
		)

	schemas, err := service.ListSchemasAllowed(
		context.Background(), []string{"shared"}, []string{"*_dev"},
	)
	if err != nil || len(schemas) != 2 {
		t.Fatalf("ListSchemasAllowed() = %#v, %v; want pattern and exact matches", schemas, err)
	}
	assertExpectations(t, mock)
}

func TestMetadataServiceDescribesTable(t *testing.T) {
	// Scenario: a table contains required and nullable columns with a literal
	// default. Risk covered: nullability and default metadata remain distinct;
	// NULL defaults are not confused with an empty-string default.
	db, mock := newMockDatabase(t)
	service, err := NewMetadataService(db, time.Second)
	if err != nil {
		t.Fatalf("NewMetadataService() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(describeTableSQL)).WithArgs("app", "users").WillReturnRows(
		sqlmock.NewRows([]string{
			"TABLE_SCHEMA", "TABLE_NAME", "TABLE_TYPE", "ENGINE", "TABLE_ROWS",
			"DATA_LENGTH", "INDEX_LENGTH", "TABLE_COLLATION", "TABLE_COMMENT",
		}).AddRow("app", "users", "BASE TABLE", "InnoDB", int64(5), int64(100), int64(20), "utf8mb4_bin", ""),
	)
	mock.ExpectQuery(regexp.QuoteMeta(listColumnsSQL)).WithArgs("app", "users").WillReturnRows(
		sqlmock.NewRows([]string{
			"COLUMN_NAME", "ORDINAL_POSITION", "COLUMN_DEFAULT", "IS_NULLABLE", "DATA_TYPE",
			"COLUMN_TYPE", "CHARACTER_SET_NAME", "COLLATION_NAME", "COLUMN_KEY", "EXTRA", "COLUMN_COMMENT",
		}).
			AddRow("id", int64(1), nil, "NO", "bigint", "bigint unsigned", nil, nil, "PRI", "auto_increment", "").
			AddRow("name", int64(2), "anonymous", "YES", "varchar", "varchar(64)", "utf8mb4", "utf8mb4_bin", "", "", "display name"),
	)

	description, err := service.DescribeTable(context.Background(), "app", "users")
	if err != nil {
		t.Fatalf("DescribeTable() error = %v", err)
	}
	if len(description.Columns) != 2 || description.Columns[0].Nullable || !description.Columns[1].Nullable {
		t.Fatalf("DescribeTable() columns = %#v", description.Columns)
	}
	if description.Columns[0].Default != nil || description.Columns[1].Default == nil || *description.Columns[1].Default != "anonymous" {
		t.Fatalf("DescribeTable() defaults = %#v", description.Columns)
	}
	assertExpectations(t, mock)
}

func TestMetadataServiceReturnsTypedNotFound(t *testing.T) {
	// Scenario: a requested table disappeared between list and describe calls.
	// Risk covered: adapters can map absence to a useful MCP error without
	// exposing the driver's sql.ErrNoRows wording.
	db, mock := newMockDatabase(t)
	service, err := NewMetadataService(db, time.Second)
	if err != nil {
		t.Fatalf("NewMetadataService() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(describeTableSQL)).WithArgs("app", "missing").
		WillReturnRows(sqlmock.NewRows([]string{
			"TABLE_SCHEMA", "TABLE_NAME", "TABLE_TYPE", "ENGINE", "TABLE_ROWS",
			"DATA_LENGTH", "INDEX_LENGTH", "TABLE_COLLATION", "TABLE_COMMENT",
		}))

	_, err = service.DescribeTable(context.Background(), "app", "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("DescribeTable() error = %v, want ErrNotFound", err)
	}
	assertExpectations(t, mock)
}

func TestMetadataServiceIndexesUseBoundIdentifiers(t *testing.T) {
	// Scenario: a malicious-looking schema name reaches the metadata API.
	// Risk covered: the value is a placeholder argument and never becomes SQL
	// syntax; valid unusual identifiers remain queryable.
	db, mock := newMockDatabase(t)
	service, err := NewMetadataService(db, time.Second)
	if err != nil {
		t.Fatalf("NewMetadataService() error = %v", err)
	}
	schema := "app' OR 1=1 --"
	mock.ExpectQuery(regexp.QuoteMeta(listIndexesSQL)).WithArgs(schema, "users").WillReturnRows(
		sqlmock.NewRows([]string{
			"INDEX_NAME", "NON_UNIQUE", "SEQ_IN_INDEX", "COLUMN_NAME", "COLLATION",
			"CARDINALITY", "SUB_PART", "INDEX_TYPE", "COMMENT", "INDEX_COMMENT",
		}).AddRow("PRIMARY", int64(0), int64(1), "id", "A", int64(10), nil, "BTREE", "", ""),
	)

	indexes, err := service.ListIndexes(context.Background(), schema, "users")
	if err != nil || len(indexes) != 1 || indexes[0].Name != "PRIMARY" || indexes[0].NonUnique {
		t.Fatalf("ListIndexes() = %#v, %v", indexes, err)
	}
	assertExpectations(t, mock)
}

func TestMetadataServiceListsForeignKeyAndCheckConstraints(t *testing.T) {
	// Scenario: KEY_COLUMN_USAGE contains a foreign key while a CHECK constraint
	// has no matching key-column row. Risk covered: the LEFT JOIN preserves both
	// kinds and nullable reference fields are decoded correctly on 5.7/8.0.
	db, mock := newMockDatabase(t)
	service, err := NewMetadataService(db, time.Second)
	if err != nil {
		t.Fatalf("NewMetadataService() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(listConstraintsSQL)).WithArgs("app", "orders").WillReturnRows(
		sqlmock.NewRows([]string{
			"CONSTRAINT_NAME", "CONSTRAINT_TYPE", "COLUMN_NAME", "ORDINAL_POSITION",
			"POSITION_IN_UNIQUE_CONSTRAINT", "REFERENCED_TABLE_SCHEMA", "REFERENCED_TABLE_NAME", "REFERENCED_COLUMN_NAME",
		}).
			AddRow("fk_customer", "FOREIGN KEY", "customer_id", int64(1), int64(1), "app", "customers", "id").
			AddRow("chk_total", "CHECK", nil, nil, nil, nil, nil, nil),
	)

	constraints, err := service.ListConstraints(context.Background(), "app", "orders")
	if err != nil {
		t.Fatalf("ListConstraints() error = %v", err)
	}
	if len(constraints) != 2 || constraints[0].ReferencedTableName == nil || *constraints[0].ReferencedTableName != "customers" {
		t.Fatalf("ListConstraints() = %#v", constraints)
	}
	if constraints[1].ColumnName != nil || constraints[1].OrdinalPosition != nil {
		t.Fatalf("CHECK constraint nullable fields = %#v", constraints[1])
	}
	assertExpectations(t, mock)
}

func TestMetadataServiceRejectsInvalidIdentifierBeforeQuery(t *testing.T) {
	// Scenario: an empty table identifier is supplied.
	// Risk covered: malformed metadata requests are rejected deterministically
	// and do not consume a database connection.
	db, mock := newMockDatabase(t)
	service, err := NewMetadataService(db, time.Second)
	if err != nil {
		t.Fatalf("NewMetadataService() error = %v", err)
	}

	_, err = service.ListConstraints(context.Background(), "app", "")
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ListConstraints() error = %v, want ErrInvalidArgument", err)
	}
	assertExpectations(t, mock)
}

func TestMetadataServiceMapsPermissionFailure(t *testing.T) {
	// Scenario: the monitor account lacks access to INFORMATION_SCHEMA rows.
	// Risk covered: callers receive a stable permission category rather than a
	// server-version-specific text message.
	db, mock := newMockDatabase(t)
	service, err := NewMetadataService(db, time.Second)
	if err != nil {
		t.Fatalf("NewMetadataService() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(listSchemasSQL)).WillReturnError(errors.New("access denied"))

	_, err = service.ListSchemas(context.Background())
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("ListSchemas() error = %v, want ErrPermissionDenied", err)
	}
	assertExpectations(t, mock)
}

func TestMetadataServiceBoundsInformationSchemaRows(t *testing.T) {
	// Scenario: a schema contains more objects than the configured response
	// budget. Risk covered: metadata browsing cannot materialize an unbounded
	// INFORMATION_SCHEMA result before the MCP adapter trims it.
	db, mock := newMockDatabase(t)
	service, err := NewMetadataServiceWithMaxRows(db, time.Second, 2)
	if err != nil {
		t.Fatalf("NewMetadataServiceWithMaxRows() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(listSchemasSQL)).WillReturnRows(
		sqlmock.NewRows([]string{"SCHEMA_NAME", "DEFAULT_CHARACTER_SET_NAME", "DEFAULT_COLLATION_NAME"}).
			AddRow("one", "utf8mb4", "utf8mb4_bin").
			AddRow("two", "utf8mb4", "utf8mb4_bin").
			AddRow("three", "utf8mb4", "utf8mb4_bin"),
	)
	schemas, err := service.ListSchemas(context.Background())
	if err != nil {
		t.Fatalf("ListSchemas() error = %v", err)
	}
	if len(schemas) != 2 || schemas[0].Name != "one" || schemas[1].Name != "two" {
		t.Fatalf("ListSchemas() = %#v, want first two rows", schemas)
	}
	assertExpectations(t, mock)
}

// Compile-time assertion documents the transaction/query-row Scan shape used
// by scanTable without coupling production code to sqlmock-specific types.
var _ tableScanner = (*sql.Row)(nil)
