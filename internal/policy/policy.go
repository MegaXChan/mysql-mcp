// Package policy contains the SQL authorization boundary used before a query
// reaches database/sql. It deliberately accepts a much smaller language than
// MySQL itself: raw read queries may only be SELECT/UNION statements and every
// potentially stateful construct is rejected after parsing the Vitess AST.
package policy

import (
	"errors"
	"fmt"
	"strings"

	"github.com/MegaXChan/mysql-mcp/internal/schemafilter"
	"vitess.io/vitess/go/vt/sqlparser"
)

// ViolationCode is a stable, machine-readable reason why SQL was rejected.
// Callers should use CodeOf rather than matching human-readable error text.
type ViolationCode string

const (
	CodeInvalidSQL             ViolationCode = "invalid_sql"
	CodeMultipleStatements     ViolationCode = "multiple_statements"
	CodeUnsafeComment          ViolationCode = "unsafe_comment"
	CodeNotReadQuery           ViolationCode = "not_read_query"
	CodeNotCommand             ViolationCode = "not_command"
	CodeNotExplain             ViolationCode = "not_explain"
	CodeExplainAnalyze         ViolationCode = "explain_analyze"
	CodeSelectInto             ViolationCode = "select_into"
	CodeLockingRead            ViolationCode = "locking_read"
	CodeUserVariableAssignment ViolationCode = "user_variable_assignment"
	CodeQualifiedFunction      ViolationCode = "qualified_function"
	CodeDangerousFunction      ViolationCode = "dangerous_function"
	CodeUnapprovedFunction     ViolationCode = "unapproved_function"
	CodeSequenceOperation      ViolationCode = "sequence_operation"
	CodeSchemaNotAllowed       ViolationCode = "schema_not_allowed"
	CodeSchemaUndetermined     ViolationCode = "schema_undetermined"
	CodeStoredProgramDDL       ViolationCode = "stored_program_ddl"
	CodeTemporaryObject        ViolationCode = "temporary_object"
	CodeUnsafeTableOption      ViolationCode = "unsafe_table_option"
)

// PolicyError reports a fail-closed parse or policy decision. SQL text is not
// included so that returning or logging the error cannot disclose query data.
type PolicyError struct {
	Code   ViolationCode
	Detail string
	Cause  error
}

func (e *PolicyError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("SQL rejected: %s", e.Code)
	}
	return fmt.Sprintf("SQL rejected: %s (%s)", e.Code, e.Detail)
}

// Unwrap preserves parser errors for diagnostics without exposing them in the
// default message returned to an MCP client.
func (e *PolicyError) Unwrap() error { return e.Cause }

// CodeOf extracts the stable violation code from err.
func CodeOf(err error) (ViolationCode, bool) {
	var policyErr *PolicyError
	if !errors.As(err, &policyErr) {
		return "", false
	}
	return policyErr.Code, true
}

func reject(code ViolationCode, detail string, cause error) error {
	return &PolicyError{Code: code, Detail: detail, Cause: cause}
}

// StatementClass groups parsed statements by the authority required to run
// them. ValidateReadQuery remains the authoritative read-only decision; Class
// is intended for routing, metrics, and audit records.
type StatementClass string

const (
	ClassRead          StatementClass = "read"
	ClassExplain       StatementClass = "explain"
	ClassWrite         StatementClass = "write"
	ClassDDL           StatementClass = "ddl"
	ClassTransaction   StatementClass = "transaction"
	ClassSession       StatementClass = "session"
	ClassAdmin         StatementClass = "admin"
	ClassStoredProgram StatementClass = "stored_program"
	ClassOther         StatementClass = "other"
)

// Classification contains both the service-level class and the exact Vitess
// statement type. Statement is a fresh AST owned by this result.
type Classification struct {
	Class      StatementClass
	VitessType sqlparser.StatementType
	Statement  sqlparser.Statement
}

// Policy is an immutable, concurrency-safe parser and validator for one MySQL
// datasource. A Policy must be constructed with the server version discovered
// for that datasource, because executable comments and grammar support are
// version-sensitive in MySQL.
type Policy struct {
	parser             *sqlparser.Parser
	mysqlServerVersion string
	mysqlVersionNumber int
}

// New constructs a policy for a single datasource. Examples of accepted
// versions include "5.7.44" and "8.0.36". Vitess performs the authoritative
// version validation.
func New(mysqlServerVersion string) (*Policy, error) {
	version := strings.TrimSpace(mysqlServerVersion)
	parser, err := sqlparser.New(sqlparser.Options{MySQLServerVersion: version})
	if err != nil {
		return nil, fmt.Errorf("create SQL parser for MySQL version %q: %w", version, err)
	}
	versionNumber, err := mysqlVersionNumber(version)
	if err != nil {
		return nil, fmt.Errorf("normalize MySQL version %q: %w", version, err)
	}
	return &Policy{
		parser:             parser,
		mysqlServerVersion: version,
		mysqlVersionNumber: versionNumber,
	}, nil
}

// MySQLServerVersion returns the version used to construct the parser.
func (p *Policy) MySQLServerVersion() string { return p.mysqlServerVersion }

// ParseOne parses exactly one non-empty SQL statement. It first rejects MySQL
// executable comments, optimizer hints, and whitespace-sensitive built-in
// spellings that can resolve to stored functions, then uses Vitess's SQL-aware
// splitter so semicolons inside string literals are not mistaken for statement
// boundaries. Vitess's strict parser has the same behavior as Parse for
// SELECT/DML but rejects partially parsed DDL instead of returning an
// incomplete AST, which is essential at this authorization boundary.
func (p *Policy) ParseOne(sql string) (stmt sqlparser.Statement, err error) {
	// Vitess v0.24.2's splitter can panic for a small class of malformed quote
	// sequences (for example a fuzz-discovered short double-quoted input). SQL
	// arrives across a trust boundary, so contain dependency panics and reject
	// the input instead of crashing the MCP process.
	defer func() {
		if recover() != nil {
			stmt = nil
			err = reject(CodeInvalidSQL, "parser rejected malformed input", nil)
		}
	}()

	if p == nil || p.parser == nil {
		return nil, reject(CodeInvalidSQL, "policy is not initialized", nil)
	}

	if kind, found := findUnsafeComment(sql); found {
		return nil, reject(CodeUnsafeComment, kind, nil)
	}
	if name, found := findAmbiguousBuiltinCall(sql); found {
		return nil, reject(CodeUnapprovedFunction, name, nil)
	}

	pieces, err := p.parser.SplitStatementToPieces(sql)
	if err != nil {
		return nil, reject(CodeInvalidSQL, "statement split failed", err)
	}
	if len(pieces) != 1 {
		return nil, reject(CodeMultipleStatements, "exactly one statement is required", nil)
	}

	stmt, err = p.parser.ParseStrictDDL(pieces[0])
	if err != nil {
		return nil, reject(CodeInvalidSQL, "parse failed", err)
	}
	if err := validateFullyParsedDDL(stmt); err != nil {
		return nil, err
	}
	return stmt, nil
}

// validateFullyParsedDDL is a defense-in-depth invariant for both raw parsing
// and APIs that accept an AST from a trusted caller. ParseStrictDDL currently
// marks every accepted DDL/DBDDL root as fully parsed; keeping the explicit
// check prevents a future parser behavior change or manually built partial AST
// from silently weakening table/schema and expression authorization.
func validateFullyParsedDDL(stmt sqlparser.Statement) error {
	switch ddl := stmt.(type) {
	case sqlparser.DDLStatement:
		if !ddl.IsFullyParsed() {
			return reject(CodeInvalidSQL, "DDL was not fully parsed", nil)
		}
	case sqlparser.DBDDLStatement:
		if !ddl.IsFullyParsed() {
			return reject(CodeInvalidSQL, "database DDL was not fully parsed", nil)
		}
	}
	return nil
}

// Classify parses SQL and returns its authority class. Parsing always happens
// before classification; textual prefixes such as "WITH" or "SELECT" are
// never trusted.
func (p *Policy) Classify(sql string) (Classification, error) {
	stmt, err := p.ParseOne(sql)
	if err != nil {
		return Classification{}, err
	}

	vitessType := sqlparser.ASTToStatementType(stmt)
	return Classification{
		Class:      classForVitessType(vitessType),
		VitessType: vitessType,
		Statement:  stmt,
	}, nil
}

func classForVitessType(statementType sqlparser.StatementType) StatementClass {
	switch statementType {
	case sqlparser.StmtSelect, sqlparser.StmtShow:
		return ClassRead
	case sqlparser.StmtExplain:
		return ClassExplain
	case sqlparser.StmtInsert, sqlparser.StmtReplace, sqlparser.StmtUpdate, sqlparser.StmtDelete:
		return ClassWrite
	case sqlparser.StmtDDL:
		return ClassDDL
	case sqlparser.StmtBegin, sqlparser.StmtCommit, sqlparser.StmtRollback,
		sqlparser.StmtSavepoint, sqlparser.StmtSRollback, sqlparser.StmtRelease:
		return ClassTransaction
	case sqlparser.StmtSet, sqlparser.StmtUse, sqlparser.StmtPrepare,
		sqlparser.StmtExecute, sqlparser.StmtDeallocate:
		return ClassSession
	case sqlparser.StmtCallProc:
		return ClassStoredProgram
	case sqlparser.StmtAnalyze, sqlparser.StmtOther, sqlparser.StmtPriv,
		sqlparser.StmtLockTables, sqlparser.StmtUnlockTables, sqlparser.StmtFlush,
		sqlparser.StmtKill, sqlparser.StmtMigration:
		return ClassAdmin
	default:
		return ClassOther
	}
}

// ValidateReadQuery accepts only Select and Union roots. Consequently a CTE
// ending in UPDATE/DELETE remains a write statement and is rejected even
// though its first keyword is WITH.
func (p *Policy) ValidateReadQuery(sql string) (sqlparser.Statement, error) {
	stmt, err := p.ParseOne(sql)
	if err != nil {
		return nil, err
	}
	if !isSelectRoot(stmt) {
		return nil, reject(CodeNotReadQuery, "only SELECT or UNION is allowed", nil)
	}
	if err := validateReadAST(stmt, p.mysqlVersionNumber); err != nil {
		return nil, err
	}
	return stmt, nil
}

// ValidateExplainQuery validates a raw SELECT/UNION that the service will wrap
// in a server-generated EXPLAIN statement. It intentionally does not accept an
// EXPLAIN prefix from the caller.
func (p *Policy) ValidateExplainQuery(sql string) (sqlparser.Statement, error) {
	return p.ValidateReadQuery(sql)
}

// ValidateExplain validates a caller-supplied EXPLAIN statement. EXPLAIN
// ANALYZE is forbidden because MySQL executes the underlying query while
// collecting runtime measurements.
func (p *Policy) ValidateExplain(sql string) (*sqlparser.ExplainStmt, error) {
	stmt, err := p.ParseOne(sql)
	if err != nil {
		return nil, err
	}

	explain, ok := stmt.(*sqlparser.ExplainStmt)
	if !ok {
		return nil, reject(CodeNotExplain, "an EXPLAIN SELECT statement is required", nil)
	}
	if explain.Type == sqlparser.AnalyzeType {
		return nil, reject(CodeExplainAnalyze, "EXPLAIN ANALYZE executes the query", nil)
	}
	if !isSelectRoot(explain.Statement) {
		return nil, reject(CodeNotReadQuery, "EXPLAIN may target only SELECT or UNION", nil)
	}
	if err := validateReadAST(explain.Statement, p.mysqlVersionNumber); err != nil {
		return nil, err
	}
	return explain, nil
}

// ValidateCommand parses and validates one raw DML or DDL statement. It is the
// command counterpart to ValidateReadQuery and must be used instead of plain
// Classify before mysql.execute: schema-qualified/unknown stored functions,
// blocking functions, user-variable assignment, SELECT INTO, locking source
// queries, executable comments, and optimizer hints remain forbidden inside a
// syntactically valid write statement.
//
// Schema authorization is datasource configuration rather than SQL grammar,
// so callers must additionally invoke ValidateCommandForSchemas (or
// ValidateAllowedSchemas) on the returned Statement.
func (p *Policy) ValidateCommand(sql string) (Classification, error) {
	classification, err := p.Classify(sql)
	if err != nil {
		return Classification{}, err
	}
	if err := validateCommandAST(classification.Statement, p.mysqlVersionNumber); err != nil {
		return Classification{}, err
	}
	return classification, nil
}

// ValidateCommandAST applies command expression safety to an already parsed
// AST. It is useful when a trusted caller owns parsing, but it cannot detect
// comments discarded during parsing; untrusted raw SQL should always use the
// Policy.ValidateCommand method above.
func ValidateCommandAST(stmt sqlparser.Statement) error {
	return validateCommandAST(stmt, 0)
}

// validateCommandAST applies command validation with the supplied datasource
// version. A zero version is the conservative cross-version profile used by
// the exported AST-only API, whose caller has provided no server version.
func validateCommandAST(stmt sqlparser.Statement, mysqlVersion int) error {
	if stmt == nil {
		return reject(CodeInvalidSQL, "statement AST is nil", nil)
	}
	if err := validateFullyParsedDDL(stmt); err != nil {
		return err
	}
	class := classForVitessType(sqlparser.ASTToStatementType(stmt))
	if class != ClassWrite && class != ClassDDL {
		return reject(CodeNotCommand, "only DML or DDL is allowed", nil)
	}

	// Stored-program lifecycle belongs to the dedicated function/routine
	// capability. Allowing it through generic DDL would bypass the configured
	// allowlist, SQL SECURITY checks, and effect classification.
	switch stmt.(type) {
	case *sqlparser.CreateProcedure, *sqlparser.DropProcedure:
		return reject(CodeStoredProgramDDL, "stored-program DDL is not accepted by mysql.execute", nil)
	}

	// Temporary tables live for the lifetime of a physical connection. With a
	// connection pool they can leak state into an unrelated later MCP request,
	// so generic execute never creates or drops session-scoped objects.
	if ddl, ok := stmt.(sqlparser.DDLStatement); ok && ddl.IsTemporary() {
		return reject(CodeTemporaryObject, "temporary-object DDL is not accepted by mysql.execute", nil)
	}

	return sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		switch query := node.(type) {
		case *sqlparser.Select:
			if query.Into != nil {
				return false, reject(CodeSelectInto, "SELECT INTO is not allowed inside a command", nil)
			}
			if query.Lock != sqlparser.NoLock {
				return false, reject(CodeLockingRead, "locking SELECT is not allowed inside a command", nil)
			}
		case *sqlparser.Union:
			if query.Into != nil {
				return false, reject(CodeSelectInto, "UNION INTO is not allowed inside a command", nil)
			}
			if query.Lock != sqlparser.NoLock {
				return false, reject(CodeLockingRead, "locking UNION is not allowed inside a command", nil)
			}
		}
		if err := validateDDLResourceNode(node); err != nil {
			return false, err
		}
		if err := validateSafeExpressionNode(node, mysqlVersion); err != nil {
			return false, err
		}
		return true, nil
	}, stmt)
}

// validateDDLResourceNode prevents generic table DDL from reaching network or
// filesystem resources. Explicit engines are restricted to InnoDB; this also
// excludes FEDERATED and MERGE, whose CONNECTION and UNION options can access
// resources outside the authorized table target. Path, tablespace, and engine
// attribute options are rejected even if paired with InnoDB.
func validateDDLResourceNode(node sqlparser.SQLNode) error {
	switch option := node.(type) {
	case sqlparser.TableOptions:
		for _, tableOption := range option {
			if err := validateTableOption(tableOption); err != nil {
				return err
			}
		}
	case *sqlparser.PartitionDefinitionOptions:
		if option.DataDirectory != nil {
			return reject(CodeUnsafeTableOption, "partition data directory", nil)
		}
		if option.IndexDirectory != nil {
			return reject(CodeUnsafeTableOption, "partition index directory", nil)
		}
		if option.TableSpace != "" {
			return reject(CodeUnsafeTableOption, "partition tablespace", nil)
		}
		if option.Engine != nil {
			return validateStorageEngine(option.Engine.Name)
		}
	case *sqlparser.SubPartitionDefinitionOptions:
		if option.DataDirectory != nil {
			return reject(CodeUnsafeTableOption, "subpartition data directory", nil)
		}
		if option.IndexDirectory != nil {
			return reject(CodeUnsafeTableOption, "subpartition index directory", nil)
		}
		if option.TableSpace != "" {
			return reject(CodeUnsafeTableOption, "subpartition tablespace", nil)
		}
		if option.Engine != nil {
			return validateStorageEngine(option.Engine.Name)
		}
	}
	return nil
}

func validateTableOption(option *sqlparser.TableOption) error {
	if option == nil {
		return reject(CodeUnsafeTableOption, "nil table option", nil)
	}
	name := strings.ToLower(strings.TrimSpace(option.Name))
	switch name {
	case "engine":
		return validateStorageEngine(option.String)
	case "connection", "data directory", "index directory", "union",
		"insert_method", "tablespace", "engine_attribute", "secondary_engine_attribute":
		return reject(CodeUnsafeTableOption, name, nil)
	default:
		return nil
	}
}

func validateStorageEngine(engine string) error {
	if strings.EqualFold(strings.TrimSpace(engine), "innodb") {
		return nil
	}
	return reject(CodeUnsafeTableOption, "only the InnoDB storage engine is allowed", nil)
}

func isSelectRoot(stmt sqlparser.Statement) bool {
	switch stmt.(type) {
	case *sqlparser.Select, *sqlparser.Union:
		return true
	default:
		return false
	}
}

func validateReadAST(stmt sqlparser.Statement, mysqlVersion int) error {
	return sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		switch expr := node.(type) {
		case *sqlparser.Select:
			if expr.Into != nil {
				return false, reject(CodeSelectInto, "SELECT INTO is not read-only", nil)
			}
			if expr.Lock != sqlparser.NoLock {
				return false, reject(CodeLockingRead, "locking SELECT is not read-only", nil)
			}
		case *sqlparser.Union:
			if expr.Into != nil {
				return false, reject(CodeSelectInto, "UNION INTO is not read-only", nil)
			}
			if expr.Lock != sqlparser.NoLock {
				return false, reject(CodeLockingRead, "locking UNION is not read-only", nil)
			}
		}
		if err := validateSafeExpressionNode(node, mysqlVersion); err != nil {
			return false, err
		}
		return true, nil
	}, stmt)
}

func validateSafeExpressionNode(node sqlparser.SQLNode, mysqlVersion int) error {
	if name, minimum := minimumVersionForBuiltinNode(node); minimum != 0 && mysqlVersion < minimum {
		return reject(CodeUnapprovedFunction, name, nil)
	}
	switch expr := node.(type) {
	case *sqlparser.AssignmentExpr:
		return reject(CodeUserVariableAssignment, "assignment expressions change session state", nil)
	case *sqlparser.FuncExpr:
		if !expr.Qualifier.IsEmpty() {
			return reject(CodeQualifiedFunction, "stored or schema-qualified functions require mysql.function.call", nil)
		}
		name := strings.ToLower(expr.Name.String())
		if isDangerousFunction(name) {
			return reject(CodeDangerousFunction, name, nil)
		}
		if !isSafeBuiltinFunction(name, mysqlVersion) {
			return reject(CodeUnapprovedFunction, name, nil)
		}
	case *sqlparser.LockingFunc:
		return reject(CodeDangerousFunction, "advisory lock function", nil)
	case *sqlparser.GTIDFuncExpr:
		if expr.Type == sqlparser.WaitForExecutedGTIDSetType ||
			expr.Type == sqlparser.WaitUntilSQLThreadAfterGTIDSType {
			return reject(CodeDangerousFunction, "GTID wait function", nil)
		}
	case *sqlparser.Nextval:
		return reject(CodeSequenceOperation, "NEXT VALUE changes sequence state", nil)
	}
	return nil
}

// ValidateAllowedSchemas verifies every physical table reference against exact
// names and optional glob patterns. An unqualified table belongs to defaultDB
// for this decision. Empty exact and pattern lists disable this additional
// restriction; any configured restriction with an empty defaultDB rejects
// unqualified physical tables because their destination cannot be proven safe.
//
// CTE identifiers and derived-table aliases are not physical table references.
// CTE scopes are tracked recursively so a name used by a nested CTE does not
// accidentally exempt a physical table with the same name outside that scope.
func ValidateAllowedSchemas(
	stmt sqlparser.Statement,
	defaultDB string,
	allowed []string,
	patternGroups ...[]string,
) error {
	patterns := flattenSchemaPatterns(patternGroups)
	if !schemafilter.Restricted(allowed, patterns) {
		return nil
	}
	if stmt == nil {
		return reject(CodeInvalidSQL, "statement AST is nil", nil)
	}

	allowedSchemas := schemaRestrictions{exact: allowed, patterns: patterns}
	if err := validateFullyParsedDDL(stmt); err != nil {
		return err
	}
	if err := validateDirectCommandSchemas(stmt, defaultDB, allowedSchemas); err != nil {
		return err
	}
	return validateSchemaScope(stmt, nil, defaultDB, allowedSchemas)
}

// ValidateCommandForSchemas is the command-oriented spelling of
// ValidateAllowedSchemas. It covers SELECT plus DML, DDL, database DDL, and
// supported administrative ASTs; callers may use it for a generic execute
// tool after applying that tool's separate authority/read-only policy.
func ValidateCommandForSchemas(
	stmt sqlparser.Statement,
	defaultDB string,
	allowed []string,
	patternGroups ...[]string,
) error {
	return ValidateAllowedSchemas(stmt, defaultDB, allowed, patternGroups...)
}

// ValidateReadQueryForSchemas combines the two checks in the required order:
// establish that SQL is a safe SELECT first, then restrict its table schemas.
func (p *Policy) ValidateReadQueryForSchemas(
	sql, defaultDB string,
	allowed []string,
	patternGroups ...[]string,
) (sqlparser.Statement, error) {
	stmt, err := p.ValidateReadQuery(sql)
	if err != nil {
		return nil, err
	}
	if err := ValidateAllowedSchemas(stmt, defaultDB, allowed, patternGroups...); err != nil {
		return nil, err
	}
	return stmt, nil
}

// schemaRestrictions is immutable for one validation pass. Slices originate
// from validated configuration and are read only while walking the SQL AST.
type schemaRestrictions struct {
	exact    []string
	patterns []string
}

func flattenSchemaPatterns(groups [][]string) []string {
	if len(groups) == 0 {
		return nil
	}
	if len(groups) == 1 {
		return groups[0]
	}
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	patterns := make([]string, 0, total)
	for _, group := range groups {
		patterns = append(patterns, group...)
	}
	return patterns
}

func validateSchemaScope(
	root sqlparser.SQLNode,
	inheritedCTEs map[string]struct{},
	defaultDB string,
	allowedSchemas schemaRestrictions,
) error {
	visibleCTEs := cloneNames(inheritedCTEs)
	with := withClause(root)
	if with != nil {
		// Adding every name before validating bodies correctly supports recursive
		// CTEs. Invalid forward references in non-recursive CTEs still fail at
		// MySQL and cannot cause a physical cross-schema access.
		for _, cte := range with.CTEs {
			visibleCTEs[strings.ToLower(cte.ID.String())] = struct{}{}
		}
		for _, cte := range with.CTEs {
			if err := validateSchemaScope(cte.Subquery, visibleCTEs, defaultDB, allowedSchemas); err != nil {
				return err
			}
		}
	}

	first := true
	return sqlparser.Walk(func(node sqlparser.SQLNode) (bool, error) {
		if first {
			first = false
			return true, nil
		}

		switch typed := node.(type) {
		case *sqlparser.CommonTableExpr:
			// CTE bodies were validated above with their proper lexical scope.
			return false, nil
		case *sqlparser.Select:
			return false, validateSchemaScope(typed, visibleCTEs, defaultDB, allowedSchemas)
		case *sqlparser.Union:
			return false, validateSchemaScope(typed, visibleCTEs, defaultDB, allowedSchemas)
		case *sqlparser.ValuesStatement:
			return false, validateSchemaScope(typed, visibleCTEs, defaultDB, allowedSchemas)
		case *sqlparser.Update:
			return false, validateSchemaScope(typed, visibleCTEs, defaultDB, allowedSchemas)
		case *sqlparser.Delete:
			return false, validateSchemaScope(typed, visibleCTEs, defaultDB, allowedSchemas)
		case sqlparser.DDLStatement:
			return false, validateSchemaScope(typed, visibleCTEs, defaultDB, allowedSchemas)
		case sqlparser.DBDDLStatement:
			return false, validateSchemaScope(typed, visibleCTEs, defaultDB, allowedSchemas)
		case *sqlparser.AliasedTableExpr:
			table, physical := typed.Expr.(sqlparser.TableName)
			if !physical {
				// A derived table is traversed until its nested Select/Union is
				// encountered and validated recursively above.
				return true, nil
			}

			return false, validatePhysicalTable(table, visibleCTEs, true, defaultDB, allowedSchemas)
		case *sqlparser.ReferenceDefinition:
			// Foreign keys can reference a table in a different database even
			// though the CREATE/ALTER target itself is allowed.
			return false, validatePhysicalTable(typed.ReferencedTable, nil, false, defaultDB, allowedSchemas)
		case *sqlparser.PartitionSpec:
			// ALTER TABLE ... EXCHANGE PARTITION names a second physical table.
			if typed.TableName.NonEmpty() {
				return false, validatePhysicalTable(typed.TableName, nil, false, defaultDB, allowedSchemas)
			}
		case *sqlparser.AutoIncSpec:
			if typed.Sequence.NonEmpty() {
				return false, validatePhysicalTable(typed.Sequence, nil, false, defaultDB, allowedSchemas)
			}
		case sqlparser.TableOptions:
			// MERGE's UNION option names additional physical tables. Generic
			// command validation rejects the capability entirely, while this
			// schema check remains defense in depth for separately parsed ASTs.
			for _, option := range typed {
				if option == nil {
					continue
				}
				for _, table := range option.Tables {
					if err := validatePhysicalTable(table, nil, false, defaultDB, allowedSchemas); err != nil {
						return false, err
					}
				}
			}
			return false, nil
		}
		return true, nil
	}, root)
}

func validateDirectCommandSchemas(
	stmt sqlparser.Statement,
	defaultDB string,
	allowedSchemas schemaRestrictions,
) error {
	if ddl, ok := stmt.(sqlparser.DDLStatement); ok {
		for _, table := range ddl.AffectedTables() {
			if err := validatePhysicalTable(table, nil, false, defaultDB, allowedSchemas); err != nil {
				return err
			}
		}
		if like := ddl.GetOptLike(); like != nil {
			if err := validatePhysicalTable(like.LikeTable, nil, false, defaultDB, allowedSchemas); err != nil {
				return err
			}
		}
	}

	if databaseDDL, ok := stmt.(sqlparser.DBDDLStatement); ok {
		if err := validateSchemaName(databaseDDL.GetDatabaseName(), allowedSchemas); err != nil {
			return err
		}
	}

	switch command := stmt.(type) {
	case *sqlparser.Use:
		return validateSchemaName(command.DBName.String(), allowedSchemas)
	case *sqlparser.Analyze:
		return validatePhysicalTable(command.Table, nil, false, defaultDB, allowedSchemas)
	case *sqlparser.Flush:
		for _, table := range command.TableNames {
			if err := validatePhysicalTable(table, nil, false, defaultDB, allowedSchemas); err != nil {
				return err
			}
		}
		return nil
	case *sqlparser.CallProc:
		return validatePhysicalTable(command.Name, nil, false, defaultDB, allowedSchemas)
	case *sqlparser.OtherAdmin, *sqlparser.Load, *sqlparser.PrepareStmt, *sqlparser.ExecuteStmt:
		// These Vitess nodes do not retain enough table information to prove
		// that an arbitrary command stays within the configured schemas.
		return reject(CodeSchemaUndetermined, "statement AST does not expose all referenced schemas", nil)
	default:
		return nil
	}
}

func validatePhysicalTable(
	table sqlparser.TableName,
	visibleCTEs map[string]struct{},
	allowVirtualDual bool,
	defaultDB string,
	allowedSchemas schemaRestrictions,
) error {
	if table.Qualifier.IsEmpty() {
		// Vitess represents SELECT expressions without a FROM clause as reading
		// the MySQL virtual DUAL table. DDL/DML targets named dual remain physical.
		if allowVirtualDual && strings.EqualFold(table.Name.String(), "dual") {
			return nil
		}
		if _, isCTE := visibleCTEs[strings.ToLower(table.Name.String())]; isCTE {
			return nil
		}
		return validateSchemaName(defaultDB, allowedSchemas)
	}
	return validateSchemaName(table.Qualifier.String(), allowedSchemas)
}

func validateSchemaName(schema string, allowedSchemas schemaRestrictions) error {
	if schemafilter.Allows(schema, allowedSchemas.exact, allowedSchemas.patterns) {
		return nil
	}
	detail := schema
	if detail == "" {
		detail = "unqualified table without a default database"
	}
	return reject(CodeSchemaNotAllowed, detail, nil)
}

func withClause(node sqlparser.SQLNode) *sqlparser.With {
	switch typed := node.(type) {
	case *sqlparser.Select:
		return typed.With
	case *sqlparser.Union:
		return typed.With
	case *sqlparser.ValuesStatement:
		return typed.With
	case *sqlparser.Update:
		return typed.With
	case *sqlparser.Delete:
		return typed.With
	default:
		return nil
	}
}

func cloneNames(source map[string]struct{}) map[string]struct{} {
	cloned := make(map[string]struct{}, len(source)+2)
	for name := range source {
		cloned[name] = struct{}{}
	}
	return cloned
}
