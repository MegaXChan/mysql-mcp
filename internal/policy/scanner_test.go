package policy

import "testing"

// TestFindUnsafeComment verifies the lexical scanner distinguishes executable
// syntax from identical byte sequences stored as data or ordinary comments.
// This prevents both bypasses and false positives before Vitess parses the AST.
func TestFindUnsafeComment(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		sql       string
		wantFound bool
		wantKind  string
	}{
		{name: "version comment", sql: "SELECT /*!80000 SQL_NO_CACHE */ 1", wantFound: true, wantKind: "MySQL executable comment"},
		{name: "optimizer hint", sql: "SELECT /*+ MAX_EXECUTION_TIME(1) */ 1", wantFound: true, wantKind: "optimizer hint"},
		{name: "single quoted data", sql: "SELECT '/*!80000 SELECT 2 */'"},
		{name: "SQL mode ambiguous escape fails closed", sql: "SELECT '\\'/*+ mode-dependent */'", wantFound: true, wantKind: "optimizer hint"},
		{name: "escaped single quote without marker", sql: "SELECT '\\'ordinary data'"},
		{name: "doubled single quote", sql: "SELECT 'a''/*+ still data */b'"},
		{name: "double quoted data", sql: "SELECT \"/*+ still data */\""},
		{name: "backtick identifier", sql: "SELECT `/*! identifier */` FROM t"},
		{name: "ordinary block comment", sql: "SELECT /* ordinary */ 1"},
		{name: "hash line comment", sql: "SELECT 1 # /*+ ignored in comment */\n"},
		{name: "dash line comment", sql: "SELECT 1 -- /*! ignored in comment */\n"},
		{name: "dash expression is not comment", sql: "SELECT 2--1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			kind, found := findUnsafeComment(test.sql)
			if found != test.wantFound || kind != test.wantKind {
				t.Fatalf("findUnsafeComment() = (%q, %v), want (%q, %v)", kind, found, test.wantKind, test.wantFound)
			}
		})
	}
}

// TestFindAmbiguousBuiltinCall locks down the lexical distinction that the
// Vitess AST cannot retain. SYM_FN names separated from "(" can resolve to a
// stored function when IGNORE_SPACE is disabled; the same bytes inside data,
// identifiers, or an enclosing comment must not be mistaken for executable
// syntax.
func TestFindAmbiguousBuiltinCall(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		sql       string
		wantFound bool
		wantName  string
	}{
		{name: "space before parenthesis", sql: "SELECT ADDDATE ('2026-01-01', 1)", wantFound: true, wantName: "adddate"},
		{name: "tab before parenthesis", sql: "SELECT COUNT\t(*) FROM orders", wantFound: true, wantName: "count"},
		{name: "ordinary block comment separator", sql: "SELECT SUBSTRING/* trace */('abc', 1)", wantFound: true, wantName: "substring"},
		{name: "line comment separator", sql: "SELECT SUM-- trace\n(amount) FROM orders", wantFound: true, wantName: "sum"},
		{name: "quoted identifier routine", sql: "SELECT `ADDDATE`('2026-01-01', 1)", wantFound: true, wantName: "adddate"},
		{name: "immediate builtin call", sql: "SELECT ADDDATE('2026-01-01', 1), COUNT(*) FROM orders"},
		{name: "longer identifier", sql: "SELECT my_adddate ('2026-01-01', 1)"},
		{name: "single quoted data", sql: "SELECT 'ADDDATE (1, 2)'"},
		{name: "double quoted data", sql: "SELECT \"COUNT (*)\""},
		{name: "backtick column", sql: "SELECT `ADDDATE (not a call)` FROM orders"},
		{name: "ordinary block comment", sql: "SELECT 1 /* SUBSTRING ('abc', 1) */"},
		{name: "hash line comment", sql: "SELECT 1 # SUM (amount)\n"},
		{name: "dash expression is not comment", sql: "SELECT SUM--1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			name, found := findAmbiguousBuiltinCall(test.sql)
			if found != test.wantFound || name != test.wantName {
				t.Fatalf("findAmbiguousBuiltinCall() = (%q, %v), want (%q, %v)", name, found, test.wantName, test.wantFound)
			}
		})
	}
}
