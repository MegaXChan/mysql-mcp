package policy

import (
	"errors"
	"testing"

	"vitess.io/vitess/go/vt/sqlparser"
)

// TestNewPolicyVersion verifies that every datasource gets a parser configured
// for its actual MySQL family. The invalid-version case protects startup from
// silently falling back to a grammar that may classify executable comments
// differently from the target server.
func TestNewPolicyVersion(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		version string
		wantErr bool
	}{
		{name: "MySQL 5.7", version: "5.7.44"},
		{name: "MySQL 8.0", version: "8.0.36"},
		{name: "version suffix accepted by Vitess", version: "8.0.36-commercial"},
		{name: "invalid version fails closed", version: "not-a-version", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			configured, err := New(test.version)
			if test.wantErr {
				if err == nil {
					t.Fatal("New() succeeded for an invalid version")
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if configured.MySQLServerVersion() != test.version {
				t.Fatalf("MySQLServerVersion() = %q, want %q", configured.MySQLServerVersion(), test.version)
			}
		})
	}
}

// TestClassify proves classification is based on the parsed root node instead
// of the first keyword. In particular, WITH ... UPDATE must be a write even
// though a textual prefix check could mistake the CTE for a read query.
func TestClassify(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, test := range []struct {
		name string
		sql  string
		want StatementClass
	}{
		{name: "select", sql: "SELECT 1", want: ClassRead},
		{name: "union", sql: "SELECT 1 UNION SELECT 2", want: ClassRead},
		{name: "show", sql: "SHOW TABLES", want: ClassRead},
		{name: "explain", sql: "EXPLAIN SELECT 1", want: ClassExplain},
		{name: "insert", sql: "INSERT INTO t(id) VALUES (1)", want: ClassWrite},
		{name: "update", sql: "UPDATE t SET id = 2", want: ClassWrite},
		{name: "delete", sql: "DELETE FROM t", want: ClassWrite},
		{name: "CTE update", sql: "WITH cte AS (SELECT 1) UPDATE t SET id = 2", want: ClassWrite},
		{name: "DDL", sql: "CREATE TABLE t(id INT)", want: ClassDDL},
		{name: "transaction", sql: "START TRANSACTION", want: ClassTransaction},
		{name: "session", sql: "SET @answer = 42", want: ClassSession},
		{name: "stored procedure", sql: "CALL app.rebuild_cache()", want: ClassStoredProgram},
		{name: "admin", sql: "ANALYZE TABLE t", want: ClassAdmin},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := configured.Classify(test.sql)
			if err != nil {
				t.Fatalf("Classify() error = %v", err)
			}
			if got.Class != test.want {
				t.Fatalf("Classify().Class = %q, want %q (Vitess type %v)", got.Class, test.want, got.VitessType)
			}
			if got.Statement == nil {
				t.Fatal("Classify().Statement is nil")
			}
		})
	}
}

// TestParseOneRejectsPartialDDL reproduces the permissive behavior of
// sqlparser.Parser.Parse that previously crossed this package's authorization
// boundary. Vitess accepts each malformed statement as a partial DDL AST and
// drops the unparsed suffix; policy.ParseOne must instead fail closed so later
// expression and schema checks never authorize an incomplete representation.
func TestParseOneRejectsPartialDDL(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, test := range []struct {
		name string
		sql  string
	}{
		{name: "partial create table", sql: "CREATE TABLE orders(id INT, payload GARBAGE)"},
		{name: "partial alter table", sql: "ALTER TABLE orders ZZZZ ZZZZ ZZZZ"},
		{name: "partial create database", sql: "CREATE DATABASE app CHARACTER SET * UNPARSABLE"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// This assertion locks down the upstream condition behind the
			// regression: ordinary Parse reports success despite retaining only
			// part of the submitted DDL.
			partial, err := configured.parser.Parse(test.sql)
			if err != nil {
				t.Fatalf("permissive Parser.Parse() error = %v", err)
			}
			switch ddl := partial.(type) {
			case sqlparser.DDLStatement:
				if ddl.IsFullyParsed() {
					t.Fatal("permissive Parser.Parse() unexpectedly returned fully parsed DDL")
				}
			case sqlparser.DBDDLStatement:
				if ddl.IsFullyParsed() {
					t.Fatal("permissive Parser.Parse() unexpectedly returned fully parsed database DDL")
				}
			default:
				t.Fatalf("permissive Parser.Parse() type = %T, want DDL or DBDDL", partial)
			}
			// AST-taking policy entry points must also reject the partial tree in
			// case a trusted integration parses SQL outside Policy.ParseOne.
			requireViolationCode(t, ValidateCommandAST(partial), CodeInvalidSQL)
			requireViolationCode(t, ValidateAllowedSchemas(partial, "app", []string{"app"}), CodeInvalidSQL)

			_, err = configured.ParseOne(test.sql)
			requireViolationCode(t, err, CodeInvalidSQL)
		})
	}
}

// TestParseOneAcceptsFullyParsedDDL ensures the strict trust boundary retains
// ordinary supported table and database lifecycle operations. The explicit
// IsFullyParsed assertion also documents the invariant relied on by command
// expression and schema authorization.
func TestParseOneAcceptsFullyParsedDDL(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, sql := range []string{
		"CREATE TABLE app.orders(id INT)",
		"ALTER TABLE app.orders ADD COLUMN note VARCHAR(32)",
		"DROP TABLE app.orders",
		"CREATE DATABASE app_archive DEFAULT CHARACTER SET utf8mb4",
		"DROP DATABASE app_archive",
	} {
		stmt, err := configured.ParseOne(sql)
		if err != nil {
			t.Errorf("ParseOne(%q) error = %v", sql, err)
			continue
		}
		switch ddl := stmt.(type) {
		case sqlparser.DDLStatement:
			if !ddl.IsFullyParsed() {
				t.Errorf("ParseOne(%q) returned partial DDL", sql)
			}
		case sqlparser.DBDDLStatement:
			if !ddl.IsFullyParsed() {
				t.Errorf("ParseOne(%q) returned partial database DDL", sql)
			}
		default:
			t.Errorf("ParseOne(%q) type = %T, want DDL or DBDDL", sql, stmt)
		}
	}
}

// TestValidateReadQueryAllowed covers the intentionally small read language.
// The semicolon-in-string and quoted-comment cases are regression guards for
// lexical prechecks accidentally splitting or rejecting harmless data.
func TestValidateReadQueryAllowed(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, test := range []struct {
		name string
		sql  string
	}{
		{name: "literal", sql: "SELECT 1"},
		{name: "one trailing delimiter", sql: "SELECT 1;"},
		{name: "semicolon in string", sql: "SELECT 'first;second' AS value"},
		{name: "CTE select", sql: "WITH cte AS (SELECT 1 AS id) SELECT id FROM cte"},
		{name: "union", sql: "SELECT 1 UNION ALL SELECT 2"},
		{name: "subquery", sql: "SELECT id FROM t WHERE id IN (SELECT id FROM allowed_ids)"},
		{name: "safe builtins", sql: "SELECT LOWER(name), ABS(score), COALESCE(alias, name) FROM users"},
		{name: "aggregate AST node", sql: "SELECT COUNT(*), SUM(amount) FROM orders"},
		{name: "ordinary comment", sql: "SELECT /* application trace */ 1"},
		{name: "hint marker in string", sql: "SELECT '/*+ MAX_EXECUTION_TIME(1) */'"},
		{name: "executable marker in string", sql: "SELECT '/*!80000 SLEEP(10) */'"},
		{name: "hint marker in quoted identifier", sql: "SELECT `/*+ harmless identifier */` FROM t"},
		{name: "read existing user variable", sql: "SELECT @existing_value"},
		{name: "pure GTID operation", sql: "SELECT GTID_SUBSET('a:1', 'a:1-2')"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			stmt, err := configured.ValidateReadQuery(test.sql)
			if err != nil {
				t.Fatalf("ValidateReadQuery() error = %v", err)
			}
			switch stmt.(type) {
			case *sqlparser.Select, *sqlparser.Union:
			default:
				t.Fatalf("ValidateReadQuery() returned unexpected AST %T", stmt)
			}
		})
	}
}

// TestValidateReadQueryRejected exercises each fail-closed branch. The table
// includes side effects hidden behind valid SELECT syntax because checking only
// the AST root would otherwise authorize them.
func TestValidateReadQueryRejected(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, test := range []struct {
		name     string
		sql      string
		wantCode ViolationCode
	}{
		{name: "empty", sql: "", wantCode: CodeMultipleStatements},
		{name: "whitespace only", sql: " \n\t", wantCode: CodeInvalidSQL},
		{name: "malformed", sql: "SELECT FROM", wantCode: CodeInvalidSQL},
		{name: "two selects", sql: "SELECT 1; SELECT 2", wantCode: CodeMultipleStatements},
		{name: "write root", sql: "UPDATE t SET id = 2", wantCode: CodeNotReadQuery},
		{name: "CTE write root", sql: "WITH cte AS (SELECT 1) UPDATE t SET id = 2", wantCode: CodeNotReadQuery},
		{name: "outfile", sql: "SELECT * FROM t INTO OUTFILE '/tmp/result'", wantCode: CodeSelectInto},
		{name: "dumpfile", sql: "SELECT payload FROM t INTO DUMPFILE '/tmp/result'", wantCode: CodeSelectInto},
		{name: "user variable into", sql: "SELECT id INTO @captured FROM t", wantCode: CodeSelectInto},
		{name: "for update", sql: "SELECT * FROM t FOR UPDATE", wantCode: CodeLockingRead},
		{name: "for share", sql: "SELECT * FROM t FOR SHARE", wantCode: CodeLockingRead},
		{name: "legacy share lock", sql: "SELECT * FROM t LOCK IN SHARE MODE", wantCode: CodeLockingRead},
		{name: "assignment", sql: "SELECT @counter := 1", wantCode: CodeUserVariableAssignment},
		{name: "sleep", sql: "SELECT SLEEP(10)", wantCode: CodeDangerousFunction},
		{name: "benchmark", sql: "SELECT BENCHMARK(1000, SHA2('x', 256))", wantCode: CodeDangerousFunction},
		{name: "read file", sql: "SELECT LOAD_FILE('/etc/passwd')", wantCode: CodeDangerousFunction},
		{name: "replication wait", sql: "SELECT MASTER_POS_WAIT('binlog.1', 1)", wantCode: CodeDangerousFunction},
		{name: "advisory lock", sql: "SELECT GET_LOCK('resource', 30)", wantCode: CodeDangerousFunction},
		{name: "release advisory lock", sql: "SELECT RELEASE_LOCK('resource')", wantCode: CodeDangerousFunction},
		{name: "GTID wait", sql: "SELECT WAIT_FOR_EXECUTED_GTID_SET('a:1', 30)", wantCode: CodeDangerousFunction},
		{name: "schema function", sql: "SELECT app.calculate_discount(1)", wantCode: CodeQualifiedFunction},
		{name: "unknown function", sql: "SELECT calculate_discount(1)", wantCode: CodeUnapprovedFunction},
		{name: "optimizer hint", sql: "SELECT /*+ MAX_EXECUTION_TIME(100) */ 1", wantCode: CodeUnsafeComment},
		{name: "executable option comment", sql: "SELECT /*!80000 SQL_NO_CACHE */ 1", wantCode: CodeUnsafeComment},
		{name: "executable statement comment", sql: "/*!50700 SELECT SLEEP(10) */", wantCode: CodeUnsafeComment},
		{name: "Vitess sequence operation", sql: "SELECT NEXT VALUE FOR seq", wantCode: CodeSequenceOperation},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := configured.ValidateReadQuery(test.sql)
			requireViolationCode(t, err, test.wantCode)
		})
	}
}

// TestVersionAwareBuiltinFunctions verifies that a function is considered a
// built-in only after the exact MySQL release that introduced it. This closes
// a stored-function bypass: on an older server an unknown unqualified name can
// resolve to a same-named routine in the current database, even when Vitess
// represents that name as a built-in or a dedicated expression node.
func TestVersionAwareBuiltinFunctions(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		version  string
		sql      string
		wantCode ViolationCode
	}{
		{name: "ANY_VALUE before 5.7.5", version: "5.7.4", sql: "SELECT ANY_VALUE(id) FROM orders", wantCode: CodeUnapprovedFunction},
		{name: "ANY_VALUE at 5.7.5", version: "5.7.5", sql: "SELECT ANY_VALUE(id) FROM orders"},
		{name: "core JSON before 5.7.8", version: "5.7.7", sql: "SELECT JSON_VALID('{}')", wantCode: CodeUnapprovedFunction},
		{name: "core JSON at 5.7.8", version: "5.7.8", sql: "SELECT JSON_VALID('{}')"},
		{name: "JSON pretty before 5.7.22", version: "5.7.21", sql: "SELECT JSON_PRETTY('{}')", wantCode: CodeUnapprovedFunction},
		{name: "JSON pretty at 5.7.22", version: "5.7.22", sql: "SELECT JSON_PRETTY('{}')"},
		{name: "current role on 5.7", version: "5.7.44", sql: "SELECT CURRENT_ROLE()", wantCode: CodeUnapprovedFunction},
		{name: "current role on 8.0", version: "8.0.0", sql: "SELECT CURRENT_ROLE()"},
		{name: "regexp function before 8.0.4", version: "8.0.3", sql: "SELECT REGEXP_LIKE('abc', '^a')", wantCode: CodeUnapprovedFunction},
		{name: "regexp function at 8.0.4", version: "8.0.4", sql: "SELECT REGEXP_LIKE('abc', '^a')"},
		{name: "performance schema function before 8.0.16", version: "8.0.15", sql: "SELECT FORMAT_BYTES(1024)", wantCode: CodeUnapprovedFunction},
		{name: "performance schema function at 8.0.16", version: "8.0.16", sql: "SELECT FORMAT_BYTES(1024)"},
		{name: "advanced JSON before 8.0.17", version: "8.0.16", sql: "SELECT JSON_OVERLAPS('[1]', '[1]')", wantCode: CodeUnapprovedFunction},
		{name: "advanced JSON at 8.0.17", version: "8.0.17", sql: "SELECT JSON_OVERLAPS('[1]', '[1]')"},
		{name: "JSON value before 8.0.21", version: "8.0.20", sql: "SELECT JSON_VALUE('{\"id\": 1}', '$.id')", wantCode: CodeUnapprovedFunction},
		{name: "JSON value at 8.0.21", version: "8.0.21", sql: "SELECT JSON_VALUE('{\"id\": 1}', '$.id')"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			configured := newTestPolicy(t, test.version)
			_, err := configured.ValidateReadQuery(test.sql)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("ValidateReadQuery() error = %v", err)
				}
				return
			}
			requireViolationCode(t, err, test.wantCode)
		})
	}
}

// TestVersionAwareBuiltinFunctionsAcrossEntryPoints makes the datasource
// version effective for every raw SQL capability, not just mysql.query.
// CURRENT_ROLE() is absent in MySQL 5.7 and therefore must be rejected in read,
// explain, and command expressions, while the same inputs are safe on 8.x.
func TestVersionAwareBuiltinFunctionsAcrossEntryPoints(t *testing.T) {
	t.Parallel()
	mysql57 := newTestPolicy(t, "5.7.44")
	mysql80 := newTestPolicy(t, "8.0.36")

	for _, test := range []struct {
		name     string
		validate func(*Policy) error
	}{
		{
			name: "read",
			validate: func(configured *Policy) error {
				_, err := configured.ValidateReadQuery("SELECT CURRENT_ROLE()")
				return err
			},
		},
		{
			name: "explain",
			validate: func(configured *Policy) error {
				_, err := configured.ValidateExplain("EXPLAIN SELECT CURRENT_ROLE()")
				return err
			},
		},
		{
			name: "command",
			validate: func(configured *Policy) error {
				_, err := configured.ValidateCommand("UPDATE app.orders SET role_name=CURRENT_ROLE()")
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			requireViolationCode(t, test.validate(mysql57), CodeUnapprovedFunction)
			if err := test.validate(mysql80); err != nil {
				t.Fatalf("MySQL 8 validation error = %v", err)
			}
		})
	}
}

// TestVersionAwareBuiltinsRejectQualifiedCalls ensures version allowances
// never authorize a schema-qualified routine. Such a call is always a stored
// function and must go through mysql.function.call, even when its final name
// matches a built-in available on that server version.
func TestVersionAwareBuiltinsRejectQualifiedCalls(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"5.7.44", "8.0.36"} {
		configured := newTestPolicy(t, version)
		for _, sql := range []string{
			"SELECT app.current_role()",
			"SELECT app.json_valid('{}')",
			"SELECT app.adddate('2026-01-01', 1)",
		} {
			_, err := configured.ValidateReadQuery(sql)
			requireViolationCode(t, err, CodeQualifiedFunction)
		}
	}
}

// TestWhitespaceSensitiveBuiltinsRejectStoredRoutineSpelling verifies the raw
// SQL boundary preserves a MySQL lexer distinction that is absent from the
// Vitess AST. With IGNORE_SPACE disabled, ADDDATE (...) and COUNT (*) can enter
// generic function resolution as identifiers; the immediate ADDDATE(...) and
// COUNT(*) spellings remain unambiguous built-ins.
func TestWhitespaceSensitiveBuiltinsRejectStoredRoutineSpelling(t *testing.T) {
	t.Parallel()
	proof := newTestPolicy(t, "8.0.36")
	immediate, err := proof.parser.Parse("SELECT ADDDATE('2026-01-01', 1)")
	if err != nil {
		t.Fatalf("parse immediate built-in spelling: %v", err)
	}
	separated, err := proof.parser.Parse("SELECT ADDDATE ('2026-01-01', 1)")
	if err != nil {
		t.Fatalf("parse separated built-in spelling: %v", err)
	}
	if got, want := sqlparser.String(separated), sqlparser.String(immediate); got != want {
		t.Fatalf("Vitess AST spellings differ: separated = %q, immediate = %q", got, want)
	}

	for _, version := range []string{"5.7.44", "8.0.36"} {
		configured := newTestPolicy(t, version)
		t.Run(version, func(t *testing.T) {
			t.Parallel()

			for _, test := range []struct {
				name     string
				validate func() error
			}{
				{
					name: "read adddate",
					validate: func() error {
						_, err := configured.ValidateReadQuery("SELECT ADDDATE ('2026-01-01', 1)")
						return err
					},
				},
				{
					name: "explain aggregate",
					validate: func() error {
						_, err := configured.ValidateExplain("EXPLAIN SELECT COUNT (*) FROM orders")
						return err
					},
				},
				{
					name: "command adddate",
					validate: func() error {
						_, err := configured.ValidateCommand("UPDATE app.orders SET due_at=ADDDATE (due_at, 1)")
						return err
					},
				},
				{
					name: "quoted function identifier",
					validate: func() error {
						_, err := configured.ValidateReadQuery("SELECT `ADDDATE`('2026-01-01', 1)")
						return err
					},
				},
			} {
				t.Run(test.name, func(t *testing.T) {
					t.Parallel()
					requireViolationCode(t, test.validate(), CodeUnapprovedFunction)
				})
			}

			if _, err := configured.ValidateReadQuery("SELECT ADDDATE('2026-01-01', 1), COUNT(*) FROM orders"); err != nil {
				t.Fatalf("immediate built-in validation error = %v", err)
			}
		})
	}
}

// TestValidateExplain separates static plans from EXPLAIN ANALYZE. The latter
// executes the statement and is therefore unsafe even when its child is a
// syntactic SELECT.
func TestValidateExplain(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, sql := range []string{
		"EXPLAIN SELECT * FROM t",
		"EXPLAIN FORMAT=JSON SELECT * FROM t",
		"EXPLAIN SELECT 1 UNION SELECT 2",
	} {
		if _, err := configured.ValidateExplain(sql); err != nil {
			t.Errorf("ValidateExplain(%q) error = %v", sql, err)
		}
	}

	for _, test := range []struct {
		name     string
		sql      string
		wantCode ViolationCode
	}{
		{name: "raw select is not explain", sql: "SELECT 1", wantCode: CodeNotExplain},
		{name: "analyze executes select", sql: "EXPLAIN ANALYZE SELECT * FROM t", wantCode: CodeExplainAnalyze},
		{name: "write target", sql: "EXPLAIN UPDATE t SET id = 1", wantCode: CodeNotReadQuery},
		{name: "table shorthand", sql: "EXPLAIN t", wantCode: CodeNotExplain},
		{name: "unsafe child function", sql: "EXPLAIN SELECT SLEEP(1)", wantCode: CodeDangerousFunction},
		{name: "unsafe child hint", sql: "EXPLAIN SELECT /*+ SET_VAR(sort_buffer_size=1) */ 1", wantCode: CodeUnsafeComment},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := configured.ValidateExplain(test.sql)
			requireViolationCode(t, err, test.wantCode)
		})
	}
}

// TestValidateExplainQuery covers the service-owned EXPLAIN path: callers pass
// only the SELECT body and the service generates the EXPLAIN keyword itself.
func TestValidateExplainQuery(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")
	if _, err := configured.ValidateExplainQuery("SELECT * FROM t"); err != nil {
		t.Fatalf("ValidateExplainQuery() error = %v", err)
	}
	_, err := configured.ValidateExplainQuery("DELETE FROM t")
	requireViolationCode(t, err, CodeNotReadQuery)
}

// TestValidateAllowedSchemas covers physical tables, default-database
// resolution, subqueries, derived tables, and nested CTE scopes. The final
// shadowing case prevents a nested CTE name from globally exempting an actual
// table with the same name elsewhere in the statement.
func TestValidateAllowedSchemas(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, test := range []struct {
		name      string
		sql       string
		defaultDB string
		allowed   []string
		wantCode  ViolationCode
	}{
		{name: "empty allowlist has no restriction", sql: "SELECT * FROM external.orders", allowed: nil},
		{name: "literal select uses virtual dual", sql: "SELECT 1", defaultDB: "blocked", allowed: []string{"app"}},
		{name: "explicit allowed schema", sql: "SELECT * FROM app.orders", defaultDB: "other", allowed: []string{"app"}},
		{name: "unqualified uses allowed default", sql: "SELECT * FROM orders", defaultDB: "app", allowed: []string{"app"}},
		{name: "exact padded default and allowlist match", sql: "SELECT * FROM orders", defaultDB: " secret ", allowed: []string{" secret "}},
		{name: "multiple allowed schemas", sql: "SELECT * FROM app.orders JOIN audit.events USING(id)", allowed: []string{"app", "audit"}},
		{name: "allowed schema in scalar subquery", sql: "SELECT (SELECT id FROM audit.events LIMIT 1) FROM app.orders", allowed: []string{"app", "audit"}},
		{name: "CTE is not a physical table", sql: "WITH recent AS (SELECT * FROM app.orders) SELECT * FROM recent", defaultDB: "blocked", allowed: []string{"app"}},
		{name: "recursive CTE is not a physical table", sql: "WITH RECURSIVE nums AS (SELECT 1 AS n UNION ALL SELECT n + 1 FROM nums WHERE n < 3) SELECT * FROM nums", defaultDB: "blocked", allowed: []string{"app"}},
		{name: "derived table checks child", sql: "SELECT * FROM (SELECT * FROM app.orders) AS recent", defaultDB: "blocked", allowed: []string{"app"}},
		{name: "explicit denied schema", sql: "SELECT * FROM secret.orders", defaultDB: "app", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "unqualified denied default", sql: "SELECT * FROM orders", defaultDB: "secret", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "padded default is not trimmed into authorization", sql: "SELECT * FROM orders", defaultDB: " secret ", allowed: []string{"secret"}, wantCode: CodeSchemaNotAllowed},
		{name: "unqualified without default", sql: "SELECT * FROM orders", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "CTE child denied", sql: "WITH recent AS (SELECT * FROM secret.orders) SELECT * FROM recent", defaultDB: "app", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "derived table child denied", sql: "SELECT * FROM (SELECT * FROM secret.orders) AS recent", defaultDB: "app", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "qualified name matching CTE is physical", sql: "WITH recent AS (SELECT 1) SELECT * FROM secret.recent", defaultDB: "app", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "nested CTE does not leak scope", sql: "SELECT * FROM (WITH private AS (SELECT 1) SELECT * FROM private) AS nested JOIN secret.private AS physical ON 1=1", defaultDB: "app", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			stmt, err := configured.ValidateReadQuery(test.sql)
			if err != nil {
				t.Fatalf("ValidateReadQuery() error = %v", err)
			}
			err = ValidateAllowedSchemas(stmt, test.defaultDB, test.allowed)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("ValidateAllowedSchemas() error = %v", err)
				}
				return
			}
			requireViolationCode(t, err, test.wantCode)
		})
	}
}

// TestValidateReadQueryForSchemas ensures callers cannot accidentally perform
// only one half of the combined read-and-schema authorization check.
func TestValidateReadQueryForSchemas(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")
	if _, err := configured.ValidateReadQueryForSchemas("SELECT * FROM app.orders", "app", []string{"app"}); err != nil {
		t.Fatalf("ValidateReadQueryForSchemas() error = %v", err)
	}
	_, err := configured.ValidateReadQueryForSchemas("SELECT * FROM secret.orders", "app", []string{"app"})
	requireViolationCode(t, err, CodeSchemaNotAllowed)
	_, err = configured.ValidateReadQueryForSchemas("DELETE FROM app.orders", "app", []string{"app"})
	requireViolationCode(t, err, CodeNotReadQuery)
}

// TestValidateAllowedSchemaPatterns covers the shared glob authorization path
// for reads. These cases are security-sensitive because configuring a pattern
// must activate the restriction even when allowed_schemas is empty, and an
// unqualified table must be checked against its resolved default database.
func TestValidateAllowedSchemaPatterns(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, test := range []struct {
		name      string
		sql       string
		defaultDB string
		exact     []string
		patterns  []string
		wantCode  ViolationCode
	}{
		{name: "empty configuration remains unrestricted", sql: "SELECT * FROM orders_prod.orders"},
		{name: "suffix pattern allows qualified schema", sql: "SELECT * FROM orders_dev.orders", patterns: []string{"*_dev"}},
		{name: "suffix pattern allows default schema", sql: "SELECT * FROM orders", defaultDB: "orders_dev", patterns: []string{"*_dev"}},
		{name: "exact name and pattern form a union", sql: "SELECT * FROM shared.orders JOIN audit_dev.events USING(id)", exact: []string{"shared"}, patterns: []string{"*_dev"}},
		{name: "pattern activates restriction", sql: "SELECT * FROM orders_prod.orders", patterns: []string{"*_dev"}, wantCode: CodeSchemaNotAllowed},
		{name: "disallowed default schema", sql: "SELECT * FROM orders", defaultDB: "orders_prod", patterns: []string{"*_dev"}, wantCode: CodeSchemaNotAllowed},
		{name: "missing default fails closed", sql: "SELECT * FROM orders", patterns: []string{"*_dev"}, wantCode: CodeSchemaNotAllowed},
		{name: "matching is case sensitive", sql: "SELECT * FROM ORDERS_DEV.orders", patterns: []string{"*_dev"}, wantCode: CodeSchemaNotAllowed},
		{name: "pattern is anchored to full schema", sql: "SELECT * FROM orders_dev_archive.orders", patterns: []string{"*_dev"}, wantCode: CodeSchemaNotAllowed},
		{name: "literal select does not require default schema", sql: "SELECT 1", defaultDB: "orders_prod", patterns: []string{"*_dev"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := configured.ValidateReadQueryForSchemas(test.sql, test.defaultDB, test.exact, test.patterns)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("ValidateReadQueryForSchemas() error = %v", err)
				}
				return
			}
			requireViolationCode(t, err, test.wantCode)
		})
	}
}

// TestValidateCommandForSchemaPatterns ensures every command-specific schema
// reference uses the same glob boundary. It exercises DML targets and sources,
// DDL destinations, database DDL, and USE so a secondary schema cannot bypass
// authorization merely because the primary target matched the pattern.
func TestValidateCommandForSchemaPatterns(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, test := range []struct {
		name     string
		sql      string
		wantCode ViolationCode
	}{
		{name: "DML target and source match", sql: "INSERT INTO orders_dev.archive(id) SELECT id FROM audit_dev.events"},
		{name: "DDL target and source match", sql: "CREATE TABLE orders_dev.copy LIKE audit_dev.orders"},
		{name: "database DDL matches", sql: "CREATE DATABASE tenant_dev"},
		{name: "USE matches", sql: "USE tenant_dev"},
		{name: "DML source outside pattern", sql: "INSERT INTO orders_dev.archive(id) SELECT id FROM audit_prod.events", wantCode: CodeSchemaNotAllowed},
		{name: "rename destination outside pattern", sql: "RENAME TABLE orders_dev.current TO orders_prod.current", wantCode: CodeSchemaNotAllowed},
		{name: "database DDL outside pattern", sql: "DROP DATABASE tenant_prod", wantCode: CodeSchemaNotAllowed},
		{name: "USE outside pattern", sql: "USE tenant_prod", wantCode: CodeSchemaNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			stmt, err := configured.ParseOne(test.sql)
			if err != nil {
				t.Fatalf("ParseOne() error = %v", err)
			}
			err = ValidateCommandForSchemas(stmt, "", nil, []string{"*_dev"})
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("ValidateCommandForSchemas() error = %v", err)
				}
				return
			}
			requireViolationCode(t, err, test.wantCode)
		})
	}
}

// TestValidateCommandForSchemas proves execute-style operations cannot escape
// allowed_schemas through either a target or a secondary source. It includes
// MySQL's multi-source DML and DDL forms, where checking only the first table
// would leave a cross-schema bypass.
func TestValidateCommandForSchemas(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, test := range []struct {
		name      string
		sql       string
		defaultDB string
		allowed   []string
		wantCode  ViolationCode
	}{
		{name: "insert target allowed", sql: "INSERT INTO app.orders(id) VALUES (1)", allowed: []string{"app"}},
		{name: "replace target allowed", sql: "REPLACE INTO app.orders(id) VALUES (1)", allowed: []string{"app"}},
		{name: "unqualified insert uses default", sql: "INSERT INTO orders(id) VALUES (1)", defaultDB: "app", allowed: []string{"app"}},
		{name: "insert select both allowed", sql: "INSERT INTO app.archive(id) SELECT id FROM audit.events", allowed: []string{"app", "audit"}},
		{name: "update join both allowed", sql: "UPDATE app.orders AS o JOIN audit.events AS e USING(id) SET o.id=e.id", allowed: []string{"app", "audit"}},
		{name: "delete allowed", sql: "DELETE FROM app.orders WHERE id=1", allowed: []string{"app"}},
		{name: "create table allowed", sql: "CREATE TABLE app.orders(id INT)", allowed: []string{"app"}},
		{name: "alter table allowed", sql: "ALTER TABLE app.orders ADD COLUMN note VARCHAR(10)", allowed: []string{"app"}},
		{name: "rename both allowed", sql: "RENAME TABLE app.old_orders TO archive.orders", allowed: []string{"app", "archive"}},
		{name: "drop multiple allowed", sql: "DROP TABLE app.one, archive.two", allowed: []string{"app", "archive"}},
		{name: "create table like allowed", sql: "CREATE TABLE app.copy LIKE archive.orders", allowed: []string{"app", "archive"}},
		{name: "create table select allowed", sql: "CREATE TABLE app.copy AS SELECT * FROM archive.orders", allowed: []string{"app", "archive"}},
		{name: "MERGE union schemas allowed for schema-only check", sql: "CREATE TABLE app.all_orders(id INT) ENGINE=MERGE UNION=(archive.old_orders, app.orders)", allowed: []string{"app", "archive"}},
		{name: "create view target and source allowed", sql: "CREATE VIEW app.order_view AS SELECT * FROM archive.orders", allowed: []string{"app", "archive"}},
		{name: "foreign key reference allowed", sql: "CREATE TABLE app.child(parent_id INT, FOREIGN KEY(parent_id) REFERENCES archive.parent(id))", allowed: []string{"app", "archive"}},
		{name: "database DDL allowed", sql: "CREATE DATABASE app", allowed: []string{"app"}},
		{name: "analyze allowed", sql: "ANALYZE TABLE app.orders", allowed: []string{"app"}},
		{name: "use allowed database", sql: "USE app", allowed: []string{"app"}},
		{name: "call allowed routine schema", sql: "CALL app.refresh_cache()", allowed: []string{"app"}},
		{name: "flush allowed table", sql: "FLUSH TABLES app.orders", allowed: []string{"app"}},
		{name: "CTE update allowed", sql: "WITH src AS (SELECT id FROM audit.events) UPDATE app.orders SET id=(SELECT id FROM src LIMIT 1)", allowed: []string{"app", "audit"}},
		{name: "insert target denied", sql: "INSERT INTO secret.orders(id) VALUES (1)", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "replace target denied", sql: "REPLACE INTO secret.orders(id) VALUES (1)", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "insert select source denied", sql: "INSERT INTO app.archive(id) SELECT id FROM secret.events", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "update joined source denied", sql: "UPDATE app.orders AS o JOIN secret.events AS e USING(id) SET o.id=e.id", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "delete target denied", sql: "DELETE FROM secret.orders WHERE id=1", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "create target denied", sql: "CREATE TABLE secret.orders(id INT)", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "rename destination denied", sql: "RENAME TABLE app.orders TO secret.orders", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "create table like source denied", sql: "CREATE TABLE app.copy LIKE secret.orders", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "create table select source denied", sql: "CREATE TABLE app.copy AS SELECT * FROM secret.orders", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "MERGE union source denied", sql: "CREATE TABLE app.all_orders(id INT) ENGINE=MERGE UNION=(secret.orders, app.orders)", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "MERGE target denied", sql: "CREATE TABLE secret.all_orders(id INT) ENGINE=MERGE UNION=(app.orders)", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "view source denied", sql: "CREATE VIEW app.order_view AS SELECT * FROM secret.orders", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "foreign key reference denied", sql: "CREATE TABLE app.child(parent_id INT, FOREIGN KEY(parent_id) REFERENCES secret.parent(id))", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "alter foreign key reference denied", sql: "ALTER TABLE app.child ADD CONSTRAINT fk_parent FOREIGN KEY(parent_id) REFERENCES secret.parent(id)", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "database DDL denied", sql: "DROP DATABASE secret", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "use denied database", sql: "USE secret", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "call denied routine schema", sql: "CALL secret.refresh_cache()", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "flush denied table", sql: "FLUSH TABLES secret.orders", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "CTE update source denied", sql: "WITH src AS (SELECT id FROM secret.events) UPDATE app.orders SET id=(SELECT id FROM src LIMIT 1)", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "exchange partition table denied", sql: "ALTER TABLE app.orders EXCHANGE PARTITION p0 WITH TABLE secret.staging", allowed: []string{"app"}, wantCode: CodeSchemaNotAllowed},
		{name: "admin AST without target fails closed", sql: "REPAIR TABLE secret.orders", allowed: []string{"app"}, wantCode: CodeSchemaUndetermined},
		{name: "load AST without target fails closed", sql: "LOAD DATA INFILE '/tmp/data' INTO TABLE app.orders", allowed: []string{"app"}, wantCode: CodeSchemaUndetermined},
		{name: "prepare AST fails closed", sql: "PREPARE read_orders FROM 'SELECT * FROM app.orders'", allowed: []string{"app"}, wantCode: CodeSchemaUndetermined},
		{name: "execute AST fails closed", sql: "EXECUTE read_orders", allowed: []string{"app"}, wantCode: CodeSchemaUndetermined},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			stmt, err := configured.ParseOne(test.sql)
			if err != nil {
				t.Fatalf("ParseOne() error = %v", err)
			}
			err = ValidateCommandForSchemas(stmt, test.defaultDB, test.allowed)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("ValidateCommandForSchemas() error = %v", err)
				}
				return
			}
			requireViolationCode(t, err, test.wantCode)
		})
	}
}

// TestValidateCommand protects the raw mysql.execute path. A write-shaped root
// is not sufficient: expressions inside that root must not invoke stored
// functions, block a worker, mutate session variables, or smuggle an unsafe
// source SELECT.
func TestValidateCommand(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, test := range []struct {
		name      string
		sql       string
		wantClass StatementClass
	}{
		{name: "insert values", sql: "INSERT INTO app.orders(id, name) VALUES (1, 'one')", wantClass: ClassWrite},
		{name: "replace values", sql: "REPLACE INTO app.orders(id) VALUES (1)", wantClass: ClassWrite},
		{name: "update safe builtin", sql: "UPDATE app.orders SET score=ABS(score)", wantClass: ClassWrite},
		{name: "insert select safe builtin", sql: "INSERT INTO app.archive(name) SELECT LOWER(name) FROM app.orders", wantClass: ClassWrite},
		{name: "delete", sql: "DELETE FROM app.orders WHERE id=1", wantClass: ClassWrite},
		{name: "create table", sql: "CREATE TABLE app.orders(id INT)", wantClass: ClassDDL},
		{name: "DDL safe current time AST", sql: "CREATE TABLE app.events(created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)", wantClass: ClassDDL},
		{name: "create table safe select", sql: "CREATE TABLE app.archive AS SELECT LOWER(name) AS name FROM app.orders", wantClass: ClassDDL},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			classification, err := configured.ValidateCommand(test.sql)
			if err != nil {
				t.Fatalf("ValidateCommand() error = %v", err)
			}
			if classification.Class != test.wantClass {
				t.Fatalf("ValidateCommand().Class = %q, want %q", classification.Class, test.wantClass)
			}
		})
	}

	for _, test := range []struct {
		name     string
		sql      string
		wantCode ViolationCode
	}{
		{name: "read root", sql: "SELECT 1", wantCode: CodeNotCommand},
		{name: "session root", sql: "SET @value=1", wantCode: CodeNotCommand},
		{name: "stored procedure root", sql: "CALL app.refresh_cache()", wantCode: CodeNotCommand},
		{name: "qualified function in update", sql: "UPDATE app.orders SET score=app.calculate_score(id)", wantCode: CodeQualifiedFunction},
		{name: "unknown function in insert", sql: "INSERT INTO app.orders(score) VALUES (calculate_score(1))", wantCode: CodeUnapprovedFunction},
		{name: "blocking function in update", sql: "UPDATE app.orders SET score=SLEEP(10)", wantCode: CodeDangerousFunction},
		{name: "advisory lock in delete", sql: "DELETE FROM app.orders WHERE GET_LOCK('x', 1)=1", wantCode: CodeDangerousFunction},
		{name: "user variable assignment", sql: "UPDATE app.orders SET score=(@counter := @counter + 1)", wantCode: CodeUserVariableAssignment},
		{name: "session mutating builtin not allowlisted", sql: "UPDATE app.orders SET score=LAST_INSERT_ID(42)", wantCode: CodeUnapprovedFunction},
		{name: "locking source select", sql: "INSERT INTO app.archive(id) SELECT id FROM app.orders FOR UPDATE", wantCode: CodeLockingRead},
		{name: "optimizer hint", sql: "UPDATE /*+ MAX_EXECUTION_TIME(100) */ app.orders SET score=1", wantCode: CodeUnsafeComment},
		{name: "multiple statements", sql: "UPDATE app.orders SET score=1; DELETE FROM app.orders", wantCode: CodeMultipleStatements},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := configured.ValidateCommand(test.sql)
			requireViolationCode(t, err, test.wantCode)
		})
	}
}

// TestValidateCommandRejectsExternalTableOptions confines generic DDL to
// ordinary InnoDB objects. FEDERATED CONNECTION can open a network-backed
// table, MERGE UNION names additional tables, and directory/tablespace options
// reach filesystem-managed storage outside the authorized table target.
// CREATE and ALTER share the same checks because both carry TableOptions in
// the Vitess AST.
func TestValidateCommandRejectsExternalTableOptions(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, sql := range []string{
		"CREATE TABLE app.orders(id INT) ENGINE=InnoDB",
		"ALTER TABLE app.orders ENGINE=InnoDB",
		"CREATE TABLE app.events(id INT) ENGINE=InnoDB PARTITION BY RANGE(id) (PARTITION p0 VALUES LESS THAN (10) ENGINE=InnoDB)",
	} {
		if _, err := configured.ValidateCommand(sql); err != nil {
			t.Errorf("ValidateCommand(%q) error = %v", sql, err)
		}
	}

	for _, test := range []struct {
		name string
		sql  string
	}{
		{name: "create FEDERATED connection", sql: "CREATE TABLE app.remote_orders(id INT) ENGINE=FEDERATED CONNECTION='mysql://user:password@db.example/app/orders'"},
		{name: "create connection option", sql: "CREATE TABLE app.remote_orders(id INT) CONNECTION='mysql://db.example/app/orders'"},
		{name: "create data directory", sql: "CREATE TABLE app.orders(id INT) DATA DIRECTORY='/srv/mysql-data'"},
		{name: "create index directory", sql: "CREATE TABLE app.orders(id INT) INDEX DIRECTORY='/srv/mysql-index'"},
		{name: "create MERGE union", sql: "CREATE TABLE app.all_orders(id INT) ENGINE=MERGE UNION=(app.orders_2025, app.orders_2026)"},
		{name: "create non InnoDB engine", sql: "CREATE TABLE app.legacy_orders(id INT) ENGINE=MyISAM"},
		{name: "alter FEDERATED engine", sql: "ALTER TABLE app.orders ENGINE=FEDERATED"},
		{name: "alter connection option", sql: "ALTER TABLE app.orders CONNECTION='mysql://db.example/app/orders'"},
		{name: "alter data directory", sql: "ALTER TABLE app.orders DATA DIRECTORY='/srv/mysql-data'"},
		{name: "alter index directory", sql: "ALTER TABLE app.orders INDEX DIRECTORY='/srv/mysql-index'"},
		{name: "alter MERGE union", sql: "ALTER TABLE app.orders ENGINE=MERGE UNION=(app.orders_2025, app.orders_2026)"},
		{name: "partition data directory", sql: "CREATE TABLE app.events(id INT) PARTITION BY RANGE(id) (PARTITION p0 VALUES LESS THAN (10) DATA DIRECTORY='/srv/mysql-data')"},
		{name: "partition external engine", sql: "CREATE TABLE app.events(id INT) PARTITION BY RANGE(id) (PARTITION p0 VALUES LESS THAN (10) ENGINE=MyISAM)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := configured.ValidateCommand(test.sql)
			requireViolationCode(t, err, CodeUnsafeTableOption)
		})
	}
}

// TestValidateCommandAST covers trusted pre-parsed callers and makes clear
// that this API still enforces the DML/DDL root boundary.
func TestValidateCommandAST(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")
	stmt, err := configured.ParseOne("UPDATE app.orders SET score=ABS(score)")
	if err != nil {
		t.Fatalf("ParseOne() error = %v", err)
	}
	if err := ValidateCommandAST(stmt); err != nil {
		t.Fatalf("ValidateCommandAST() error = %v", err)
	}
	stmt, err = configured.ParseOne("SELECT 1")
	if err != nil {
		t.Fatalf("ParseOne() error = %v", err)
	}
	requireViolationCode(t, ValidateCommandAST(stmt), CodeNotCommand)
	requireViolationCode(t, ValidateCommandAST(nil), CodeInvalidSQL)
}

// TestValidateCommandASTUsesConservativeBuiltinProfile covers callers that
// provide an AST without a datasource version. The static API must use only
// the cross-version common function set; callers needing a version-specific
// built-in must use Policy.ValidateCommand so the actual server version is
// part of the authorization decision.
func TestValidateCommandASTUsesConservativeBuiltinProfile(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, sql := range []string{
		"UPDATE app.orders SET role_name=CURRENT_ROLE()",
		"UPDATE app.orders SET valid_payload=JSON_VALID(payload)",
	} {
		classification, err := configured.ValidateCommand(sql)
		if err != nil {
			t.Fatalf("versioned ValidateCommand(%q) error = %v", sql, err)
		}
		requireViolationCode(t, ValidateCommandAST(classification.Statement), CodeUnapprovedFunction)
	}
}

// TestValidateCommandRejectsSessionAndStoredProgramDDL narrows feature.ddl to
// persistent table/view/database objects. Procedures must use dedicated
// routine controls, while temporary tables are forbidden because their state
// survives on a pooled physical connection after an MCP request returns.
func TestValidateCommandRejectsSessionAndStoredProgramDDL(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, test := range []struct {
		name     string
		sql      string
		wantCode ViolationCode
	}{
		{name: "create procedure", sql: "CREATE PROCEDURE app.refresh_cache() SELECT 1", wantCode: CodeStoredProgramDDL},
		{name: "drop procedure", sql: "DROP PROCEDURE app.refresh_cache", wantCode: CodeStoredProgramDDL},
		{name: "create temporary table", sql: "CREATE TEMPORARY TABLE app.request_cache(id INT)", wantCode: CodeTemporaryObject},
		{name: "drop temporary table", sql: "DROP TEMPORARY TABLE app.request_cache", wantCode: CodeTemporaryObject},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := configured.ValidateCommand(test.sql)
			requireViolationCode(t, err, test.wantCode)
		})
	}
}

// TestValidateCommandUnsupportedStoredProgramDDL records Vitess v0.24.2's
// actual grammar boundary. FUNCTION, TRIGGER, and EVENT lifecycle statements
// do not produce an AST in this version and therefore fail closed as invalid
// SQL before command classification.
func TestValidateCommandUnsupportedStoredProgramDDL(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")

	for _, test := range []struct {
		name string
		sql  string
	}{
		{name: "create function", sql: "CREATE FUNCTION app.increment_value(x INT) RETURNS INT DETERMINISTIC RETURN x + 1"},
		{name: "drop function", sql: "DROP FUNCTION app.increment_value"},
		{name: "create trigger", sql: "CREATE TRIGGER app.before_insert BEFORE INSERT ON app.orders FOR EACH ROW SET NEW.id=1"},
		{name: "drop trigger", sql: "DROP TRIGGER app.before_insert"},
		{name: "create event", sql: "CREATE EVENT app.cleanup ON SCHEDULE EVERY 1 DAY DO DELETE FROM app.orders"},
		{name: "drop event", sql: "DROP EVENT app.cleanup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := configured.ValidateCommand(test.sql)
			requireViolationCode(t, err, CodeInvalidSQL)
		})
	}
}

// TestValidateCommandKeepsPersistentDDL confirms the narrowed checks do not
// disable ordinary feature.ddl operations.
func TestValidateCommandKeepsPersistentDDL(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")
	for _, sql := range []string{
		"CREATE TABLE app.orders(id INT)",
		"DROP TABLE app.orders",
		"CREATE VIEW app.order_ids AS SELECT id FROM app.orders",
		"DROP VIEW app.order_ids",
		"CREATE DATABASE app_archive",
		"DROP DATABASE app_archive",
	} {
		classification, err := configured.ValidateCommand(sql)
		if err != nil {
			t.Errorf("ValidateCommand(%q) error = %v", sql, err)
			continue
		}
		if classification.Class != ClassDDL {
			t.Errorf("ValidateCommand(%q).Class = %q, want %q", sql, classification.Class, ClassDDL)
		}
	}
}

// TestPolicyErrorDoesNotLeakSQL verifies errors are safe to surface through an
// MCP response or audit log even when the parser's cause includes nearby SQL.
func TestPolicyErrorDoesNotLeakSQL(t *testing.T) {
	t.Parallel()
	configured := newTestPolicy(t, "8.0.36")
	const secret = "customer-secret-9876"
	_, err := configured.ValidateReadQuery("SELECT '" + secret + "' FROM")
	if err == nil {
		t.Fatal("ValidateReadQuery() unexpectedly succeeded")
	}
	if contains := stringContains(err.Error(), secret); contains {
		t.Fatalf("public error leaked SQL data: %v", err)
	}
}

// TestPolicyErrorAPI locks down the error contract used by MCP error mapping.
// Causes remain available to trusted diagnostics through errors.Unwrap, while
// client-facing text contains only the stable code and safe detail.
func TestPolicyErrorAPI(t *testing.T) {
	t.Parallel()
	cause := errors.New("parser diagnostic")
	err := &PolicyError{Code: CodeInvalidSQL, Cause: cause}
	if err.Error() != "SQL rejected: invalid_sql" {
		t.Fatalf("Error() = %q", err.Error())
	}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is() did not unwrap the parser cause")
	}
	if _, ok := CodeOf(errors.New("unrelated")); ok {
		t.Fatal("CodeOf() accepted a non-policy error")
	}
}

// TestParseOneFailClosed checks initialization and lexer failures that do not
// reach the AST validator. Both must be policy errors, never a panic or a
// permissive fallback.
func TestParseOneFailClosed(t *testing.T) {
	t.Parallel()
	var uninitialized *Policy
	_, err := uninitialized.ParseOne("SELECT 1")
	requireViolationCode(t, err, CodeInvalidSQL)

	configured := newTestPolicy(t, "8.0.36")
	_, err = configured.ParseOne("SELECT 'unterminated;")
	requireViolationCode(t, err, CodeInvalidSQL)
	_, err = configured.ParseOne("\";\"\"0")
	requireViolationCode(t, err, CodeInvalidSQL)
}

// TestValidateAllowedSchemasNilAST documents the allowlist short circuit and
// the fail-closed behavior when a caller claims restrictions but has no AST.
func TestValidateAllowedSchemasNilAST(t *testing.T) {
	t.Parallel()
	if err := ValidateAllowedSchemas(nil, "", nil); err != nil {
		t.Fatalf("empty allowlist with nil AST error = %v", err)
	}
	err := ValidateAllowedSchemas(nil, "app", []string{"app"})
	requireViolationCode(t, err, CodeInvalidSQL)
	err = ValidateAllowedSchemas(nil, "", nil, []string{"*_dev"})
	requireViolationCode(t, err, CodeInvalidSQL)
}

// TestClassForVitessType directly covers non-MySQL/Vitess-only statement
// categories. These classes are never treated as read by the authorization
// path even if a future caller submits one programmatically.
func TestClassForVitessType(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		statementType sqlparser.StatementType
		want          StatementClass
	}{
		{statementType: sqlparser.StmtStream, want: ClassOther},
		{statementType: sqlparser.StmtVStream, want: ClassOther},
		{statementType: sqlparser.StmtUnknown, want: ClassOther},
	} {
		if got := classForVitessType(test.statementType); got != test.want {
			t.Errorf("classForVitessType(%v) = %q, want %q", test.statementType, got, test.want)
		}
	}
}

func newTestPolicy(t *testing.T, version string) *Policy {
	t.Helper()
	configured, err := New(version)
	if err != nil {
		t.Fatalf("New(%q) error = %v", version, err)
	}
	return configured
}

func requireViolationCode(t *testing.T, err error, want ViolationCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("operation succeeded, want violation %q", want)
	}
	got, ok := CodeOf(err)
	if !ok {
		t.Fatalf("error %T is not a PolicyError: %v", err, err)
	}
	if got != want {
		t.Fatalf("violation code = %q, want %q; error = %v", got, want, err)
	}
	var policyErr *PolicyError
	if !errors.As(err, &policyErr) {
		t.Fatalf("errors.As(*PolicyError) = false for %v", err)
	}
}

func stringContains(value, substring string) bool {
	if substring == "" {
		return true
	}
	for index := 0; index+len(substring) <= len(value); index++ {
		if value[index:index+len(substring)] == substring {
			return true
		}
	}
	return false
}
