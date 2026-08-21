package database

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestFunctionServiceListsOnlyExplicitPolicies(t *testing.T) {
	// Scenario: two stored functions are configured but the client lists one
	// schema. Risk covered: INFORMATION_SCHEMA is constrained by bound allow-list
	// values, and native/loadable UDFs or unconfigured routines cannot leak.
	db, mock := newMockDatabase(t)
	policies := []FunctionPolicy{
		{Schema: "app", Name: "discount", Effect: FunctionEffectRead},
		{Schema: "ops", Name: "rebuild", Effect: FunctionEffectWrite},
	}
	service, err := NewFunctionService(db, db, policies, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}
	mock.ExpectQuery("SELECT ROUTINE_SCHEMA, ROUTINE_NAME").WithArgs("app", "discount").
		WillReturnRows(functionInfoRows("app", "discount", "INVOKER", "READS SQL DATA"))

	functions, err := service.List(context.Background(), "app")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(functions) != 1 || functions[0].Name != "discount" || functions[0].ConfiguredEffect != FunctionEffectRead {
		t.Fatalf("List() = %#v", functions)
	}
	assertExpectations(t, mock)
}

func TestFunctionRoutineNamesAreCaseInsensitiveWithinExactSchema(t *testing.T) {
	// MySQL routine names are case-insensitive, but database-name case
	// sensitivity depends on the host. List/Describe/Call must therefore accept
	// routine-case differences without authorizing a case-different schema.
	db, mock := newMockDatabase(t)
	service, err := NewFunctionService(db, nil, []FunctionPolicy{{
		Schema: "app", Name: "Discount", Effect: FunctionEffectRead,
	}}, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}

	mock.ExpectQuery("SELECT ROUTINE_SCHEMA, ROUTINE_NAME").WithArgs("app", "Discount").
		WillReturnRows(functionInfoRows("app", "discount", "INVOKER", "READS SQL DATA"))
	listed, err := service.List(context.Background(), "app")
	if err != nil || len(listed) != 1 {
		t.Fatalf("List() = %#v, %v; want case-insensitive routine match", listed, err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(describeFunctionSQL)).WithArgs("app", "discount").
		WillReturnRows(functionInfoRows("app", "Discount", "INVOKER", "READS SQL DATA"))
	mock.ExpectQuery(regexp.QuoteMeta(functionParametersSQL)).WithArgs("app", "discount").
		WillReturnRows(functionParameterRows(0))
	if _, err := service.Describe(context.Background(), "app", "discount"); err != nil {
		t.Fatalf("Describe() error = %v", err)
	}

	mock.ExpectBegin()
	expectFunctionDescription(mock, "app", "DISCOUNT", "INVOKER", "READS SQL DATA", 0)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `app`.`DISCOUNT`() AS `result`")).
		WillReturnRows(sqlmock.NewRowsWithColumnDefinition(
			sqlmock.NewColumn("result").OfType("BIGINT", int64(0)),
		).AddRow(int64(1)))
	mock.ExpectRollback()
	if _, err := service.Call(context.Background(), "app", "DISCOUNT", nil); err != nil {
		t.Fatalf("Call() error = %v", err)
	}

	if _, err := service.Describe(context.Background(), "APP", "discount"); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("case-different schema error = %v, want ErrPolicyDenied", err)
	}
	assertExpectations(t, mock)
}

func TestFunctionServiceDescribesReturnAndInputParameters(t *testing.T) {
	// Scenario: a function returns DECIMAL and accepts two input parameters.
	// Risk covered: ordinal zero is modeled as the return type and is excluded
	// from the argument count used by Call.
	db, mock := newMockDatabase(t)
	service, err := NewFunctionService(db, nil, []FunctionPolicy{{
		Schema: "app", Name: "discount", Effect: FunctionEffectRead,
	}}, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(describeFunctionSQL)).WithArgs("app", "discount").
		WillReturnRows(functionInfoRows("app", "discount", "INVOKER", "READS SQL DATA"))
	mock.ExpectQuery(regexp.QuoteMeta(functionParametersSQL)).WithArgs("app", "discount").
		WillReturnRows(functionParameterRows(2))

	description, err := service.Describe(context.Background(), "app", "discount")
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if description.Return == nil || description.Return.OrdinalPosition != 0 || len(description.Parameters) != 2 {
		t.Fatalf("Describe() = %#v", description)
	}
	assertExpectations(t, mock)
}

func TestFunctionParameterMetadataExcludesSameNamedProcedure(t *testing.T) {
	// MySQL permits a procedure and function to share a schema/name. PARAMETERS
	// contains both routine types, so omitting ROUTINE_TYPE would merge their
	// ordinal rows and make a legitimate allow-listed function uncallable (or
	// validate the wrong signature).
	db, mock := newMockDatabase(t)
	service, err := NewFunctionService(db, nil, []FunctionPolicy{{
		Schema: "app", Name: "calculate", Effect: FunctionEffectRead,
	}}, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(describeFunctionSQL)).WithArgs("app", "calculate").
		WillReturnRows(functionInfoRows("app", "calculate", "INVOKER", "READS SQL DATA"))
	const functionOnlyParametersSQL = `SELECT ORDINAL_POSITION, PARAMETER_MODE, PARAMETER_NAME,
       DATA_TYPE, DTD_IDENTIFIER, CHARACTER_SET_NAME, COLLATION_NAME
FROM INFORMATION_SCHEMA.PARAMETERS
WHERE SPECIFIC_SCHEMA = ? AND SPECIFIC_NAME = ? AND ROUTINE_TYPE = 'FUNCTION'
ORDER BY ORDINAL_POSITION`
	mock.ExpectQuery(regexp.QuoteMeta(functionOnlyParametersSQL)).WithArgs("app", "calculate").
		WillReturnRows(functionParameterRows(1))

	if _, err := service.Describe(context.Background(), "app", "calculate"); err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	assertExpectations(t, mock)
}

func TestFunctionServiceCallsReadFunctionInReadOnlyTransaction(t *testing.T) {
	// Scenario: an allowed INVOKER function is called with two scalar arguments.
	// Risk covered: metadata is validated in the same read-only transaction,
	// values remain placeholders, and success ends in rollback.
	db, mock := newMockDatabase(t)
	service, err := NewFunctionService(db, nil, []FunctionPolicy{{
		Schema: "app", Name: "discount", Effect: FunctionEffectRead,
	}}, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}
	mock.ExpectBegin()
	expectFunctionDescription(mock, "app", "discount", "INVOKER", "READS SQL DATA", 2)
	callSQL := "SELECT `app`.`discount`(?,?) AS `result`"
	mock.ExpectQuery(regexp.QuoteMeta(callSQL)).WithArgs("gold", "123.45").
		WillReturnRows(sqlmock.NewRowsWithColumnDefinition(
			sqlmock.NewColumn("result").OfType("DECIMAL", []byte{}),
		).AddRow([]byte("12.3450")))
	mock.ExpectRollback()

	result, err := service.Call(context.Background(), "app", "discount", []any{"gold", "123.45"})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("Call() rows = %#v", result.Rows)
	}
	assertCell(t, result.Rows[0][0], CellDecimal, "12.3450")
	assertExpectations(t, mock)
}

func TestFunctionServiceCallsWriteFunctionOnWriterAndCommits(t *testing.T) {
	// Scenario: an explicitly write-classified function mutates state.
	// Risk covered: validation and invocation both use the writer pool, the
	// transaction is not read-only, and successful work is committed.
	reader, readerMock := newMockDatabase(t)
	writer, writerMock := newMockDatabase(t)
	service, err := NewFunctionService(reader, writer, []FunctionPolicy{{
		Schema: "ops", Name: "rebuild_counter", Effect: FunctionEffectWrite, AllowDefiner: true,
	}}, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}
	writerMock.ExpectBegin()
	expectFunctionDescription(writerMock, "ops", "rebuild_counter", "DEFINER", "MODIFIES SQL DATA", 1)
	writerMock.ExpectQuery(regexp.QuoteMeta("SELECT `ops`.`rebuild_counter`(?) AS `result`")).WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRowsWithColumnDefinition(
			sqlmock.NewColumn("result").OfType("BIGINT", int64(0)),
		).AddRow(int64(1)))
	writerMock.ExpectCommit()

	result, err := service.Call(context.Background(), "ops", "rebuild_counter", []any{int64(7)})
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	assertCell(t, result.Rows[0][0], CellInteger, "1")
	assertExpectations(t, writerMock)
	assertExpectations(t, readerMock)
}

func TestFunctionServiceQuotesAllowListedIdentifiers(t *testing.T) {
	// Scenario: explicitly allow-listed identifiers contain SQL-looking text.
	// Risk covered: even policy-controlled identifiers are quoted and doubled,
	// while argument values remain bound.
	db, mock := newMockDatabase(t)
	schema := "app`archive"
	name := "fn`) FROM mysql.user --"
	service, err := NewFunctionService(db, nil, []FunctionPolicy{{
		Schema: schema, Name: name, Effect: FunctionEffectRead,
	}}, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}
	mock.ExpectBegin()
	expectFunctionDescription(mock, schema, name, "INVOKER", "NO SQL", 0)
	safeSQL := "SELECT `app``archive`.`fn``) FROM mysql.user --`() AS `result`"
	mock.ExpectQuery(regexp.QuoteMeta(safeSQL)).
		WillReturnRows(sqlmock.NewRowsWithColumnDefinition(
			sqlmock.NewColumn("result").OfType("BIGINT", int64(0)),
		).AddRow(int64(9)))
	mock.ExpectRollback()

	if _, err := service.Call(context.Background(), schema, name, nil); err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	assertExpectations(t, mock)
}

func TestFunctionServiceRejectsUnlistedFunctionBeforeDatabaseAccess(t *testing.T) {
	// Scenario: a client attempts to inject a different function name.
	// Risk covered: only exact schema/name policy keys are accepted and no
	// INFORMATION_SCHEMA or function query is issued for a denied name.
	db, mock := newMockDatabase(t)
	service, err := NewFunctionService(db, nil, []FunctionPolicy{{
		Schema: "app", Name: "safe", Effect: FunctionEffectRead,
	}}, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}

	_, err = service.Call(context.Background(), "app", "safe`; SELECT secret FROM users; --", nil)
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Call() error = %v, want ErrPolicyDenied", err)
	}
	assertExpectations(t, mock)
}

func TestFunctionServiceRejectsNonScalarArgument(t *testing.T) {
	// Scenario: a JSON object reaches the function-argument decoder.
	// Risk covered: maps/slices cannot be interpreted as SQL expressions or
	// passed to an unsupported driver conversion path.
	db, mock := newMockDatabase(t)
	service, err := NewFunctionService(db, nil, []FunctionPolicy{{
		Schema: "app", Name: "safe", Effect: FunctionEffectRead,
	}}, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}

	_, err = service.Call(context.Background(), "app", "safe", []any{map[string]any{"raw": "NOW()"}})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Call() error = %v, want ErrInvalidArgument", err)
	}
	assertExpectations(t, mock)
}

func TestFunctionServiceRejectsArgumentCountMismatchAndRollsBack(t *testing.T) {
	// Scenario: metadata declares two inputs but the client supplies one.
	// Risk covered: the mismatch is detected before invoking the routine and the
	// validation transaction is closed.
	db, mock := newMockDatabase(t)
	service, err := NewFunctionService(db, nil, []FunctionPolicy{{
		Schema: "app", Name: "discount", Effect: FunctionEffectRead,
	}}, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}
	mock.ExpectBegin()
	expectFunctionDescription(mock, "app", "discount", "INVOKER", "READS SQL DATA", 2)
	mock.ExpectRollback()

	_, err = service.Call(context.Background(), "app", "discount", []any{"gold"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Call() error = %v, want ErrInvalidArgument", err)
	}
	assertExpectations(t, mock)
}

func TestFunctionServiceRejectsDefinerWithoutExplicitPermission(t *testing.T) {
	// Scenario: a function runs with its creator's privileges, but the policy did
	// not opt in. Risk covered: privilege escalation through SQL SECURITY DEFINER
	// is denied even though EXECUTE might otherwise succeed.
	db, mock := newMockDatabase(t)
	service, err := NewFunctionService(db, nil, []FunctionPolicy{{
		Schema: "app", Name: "privileged", Effect: FunctionEffectRead,
	}}, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}
	mock.ExpectBegin()
	expectFunctionDescription(mock, "app", "privileged", "DEFINER", "READS SQL DATA", 0)
	mock.ExpectRollback()

	_, err = service.Call(context.Background(), "app", "privileged", nil)
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Call() error = %v, want ErrPolicyDenied", err)
	}
	assertExpectations(t, mock)
}

func TestFunctionServiceRejectsReadPolicyForDeclaredWriter(t *testing.T) {
	// Scenario: configuration calls a routine read-only while metadata declares
	// MODIFIES SQL DATA. Risk covered: obvious policy/metadata drift is rejected;
	// advisory metadata is never used to weaken a read policy.
	db, mock := newMockDatabase(t)
	service, err := NewFunctionService(db, nil, []FunctionPolicy{{
		Schema: "app", Name: "mutate", Effect: FunctionEffectRead, AllowDefiner: true,
	}}, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}
	mock.ExpectBegin()
	expectFunctionDescription(mock, "app", "mutate", "DEFINER", "MODIFIES SQL DATA", 0)
	mock.ExpectRollback()

	_, err = service.Call(context.Background(), "app", "mutate", nil)
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Call() error = %v, want ErrPolicyDenied", err)
	}
	assertExpectations(t, mock)
}

func TestFunctionServiceSanitizesExecutePermissionError(t *testing.T) {
	// Scenario: MySQL returns an EXECUTE denial including account and host.
	// Risk covered: adapters receive a typed permission error without leaking
	// credentials or account topology.
	db, mock := newMockDatabase(t)
	service, err := NewFunctionService(db, nil, []FunctionPolicy{{
		Schema: "app", Name: "safe", Effect: FunctionEffectRead,
	}}, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}
	mock.ExpectBegin()
	expectFunctionDescription(mock, "app", "safe", "INVOKER", "READS SQL DATA", 0)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `app`.`safe`() AS `result`")).
		WillReturnError(errors.New("execute command denied to user 'mcp_user'@'10.2.3.4' for routine 'app.safe'"))
	mock.ExpectRollback()

	_, err = service.Call(context.Background(), "app", "safe", nil)
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("Call() error = %v, want ErrPermissionDenied", err)
	}
	if strings.Contains(err.Error(), "mcp_user") || strings.Contains(err.Error(), "10.2.3.4") {
		t.Fatalf("Call() leaked account information: %v", err)
	}
	assertExpectations(t, mock)
}

func TestFunctionServiceRejectsLoadableUDFByPolicyAndMetadataModel(t *testing.T) {
	// Scenario: a client names a native/loadable UDF such as sys_exec.
	// Risk covered: no policy entry means denial, and the service never consults
	// mysql.func or generates a direct call for loadable UDFs.
	db, mock := newMockDatabase(t)
	service, err := NewFunctionService(db, nil, nil, Limits{})
	if err != nil {
		t.Fatalf("NewFunctionService() error = %v", err)
	}
	_, err = service.Call(context.Background(), "mysql", "sys_exec", []any{"id"})
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("Call() error = %v, want ErrPolicyDenied", err)
	}
	assertExpectations(t, mock)
}

func expectFunctionDescription(
	mock sqlmock.Sqlmock,
	schema, name, security, access string,
	inputCount int,
) {
	mock.ExpectQuery(regexp.QuoteMeta(describeFunctionSQL)).WithArgs(schema, name).
		WillReturnRows(functionInfoRows(schema, name, security, access))
	mock.ExpectQuery(regexp.QuoteMeta(functionParametersSQL)).WithArgs(schema, name).
		WillReturnRows(functionParameterRows(inputCount))
}

func functionInfoRows(schema, name, security, access string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"ROUTINE_SCHEMA", "ROUTINE_NAME", "DATA_TYPE", "DTD_IDENTIFIER",
		"IS_DETERMINISTIC", "SQL_DATA_ACCESS", "SECURITY_TYPE", "ROUTINE_COMMENT",
	}).AddRow(schema, name, "decimal", "decimal(20,4)", "YES", access, security, "")
}

func functionParameterRows(inputCount int) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"ORDINAL_POSITION", "PARAMETER_MODE", "PARAMETER_NAME", "DATA_TYPE",
		"DTD_IDENTIFIER", "CHARACTER_SET_NAME", "COLLATION_NAME",
	}).AddRow(int64(0), nil, nil, "decimal", "decimal(20,4)", nil, nil)
	for i := 1; i <= inputCount; i++ {
		rows.AddRow(int64(i), "IN", "arg", "varchar", "varchar(255)", "utf8mb4", "utf8mb4_bin")
	}
	return rows
}
