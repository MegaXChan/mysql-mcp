package policy

import (
	"testing"

	"vitess.io/vitess/go/vt/sqlparser"
)

// FuzzValidateReadQuery seeds valid, malformed, multi-statement, comment, CTE,
// and dangerous-function inputs. The invariant is that arbitrary bytes never
// panic; every accepted input has a Select/Union root and classifies as read.
// Rejections must carry a stable PolicyError code suitable for MCP mapping.
func FuzzValidateReadQuery(f *testing.F) {
	for _, seed := range []string{
		"SELECT 1",
		"SELECT 'one;two'",
		"SELECT 1; DELETE FROM t",
		"WITH cte AS (SELECT 1) SELECT * FROM cte",
		"WITH cte AS (SELECT 1) UPDATE t SET id = 1",
		"SELECT /*+ MAX_EXECUTION_TIME(1) */ 1",
		"/*!80000 SELECT SLEEP(1) */",
		"SELECT app.function_name(1)",
		"SELECT ADDDATE ('2026-01-01', 1)",
		"SELECT COUNT/* separator */(*) FROM orders",
		"SELECT @value := 1",
		"\";\"\"0", // Vitess v0.24.2 splitter panic regression.
		"\xff\x00SELECT",
	} {
		f.Add(seed)
	}

	configured, err := New("8.0.36")
	if err != nil {
		f.Fatalf("New() error = %v", err)
	}

	f.Fuzz(func(t *testing.T, sql string) {
		stmt, validateErr := configured.ValidateReadQuery(sql)
		if validateErr != nil {
			if _, ok := CodeOf(validateErr); !ok {
				t.Fatalf("rejection has no stable policy code: %T %v", validateErr, validateErr)
			}
			return
		}

		switch stmt.(type) {
		case *sqlparser.Select, *sqlparser.Union:
		default:
			t.Fatalf("accepted non-read AST %T", stmt)
		}
		classification, classifyErr := configured.Classify(sql)
		if classifyErr != nil {
			t.Fatalf("accepted query could not be classified: %v", classifyErr)
		}
		if classification.Class != ClassRead {
			t.Fatalf("accepted query class = %q, want %q", classification.Class, ClassRead)
		}
	})
}
