package schemafilter

import (
	"strings"
	"testing"
)

// TestMatch documents the deliberately small glob language. In particular,
// it protects the full-name anchor and exact case comparison: relaxing either
// behavior could authorize a different database on a case-sensitive server.
func TestMatch(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		pattern string
		schema  string
		want    bool
	}{
		{name: "suffix match", pattern: "*_dev", schema: "orders_dev", want: true},
		{name: "suffix does not match production", pattern: "*_dev", schema: "orders_prod", want: false},
		{name: "star may consume zero characters", pattern: "*_dev", schema: "_dev", want: true},
		{name: "match is anchored at start", pattern: "dev*", schema: "orders_dev", want: false},
		{name: "match is anchored at end", pattern: "dev*", schema: "my_dev_orders", want: false},
		{name: "multiple stars backtrack", pattern: "tenant_*_dev", schema: "tenant_eu_west_dev", want: true},
		{name: "adjacent stars are harmless", pattern: "tenant**_dev", schema: "tenant_dev", want: true},
		{name: "star remains wildcard when schema contains star", pattern: "*a", schema: "*ba", want: true},
		{name: "question mark is literal", pattern: "tenant?_dev", schema: "tenant1_dev", want: false},
		{name: "question mark literal can match", pattern: "tenant?_dev", schema: "tenant?_dev", want: true},
		{name: "case is exact", pattern: "*_dev", schema: "ORDERS_DEV", want: false},
		{name: "unicode characters", pattern: "租户*_dev", schema: "租户甲_dev", want: true},
		{name: "invalid empty pattern", pattern: "", schema: "orders_dev", want: false},
		{name: "invalid UTF-8 schema", pattern: "*_dev", schema: string([]byte{0xff}), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Match(test.pattern, test.schema); got != test.want {
				t.Fatalf("Match(%q, %q) = %v, want %v", test.pattern, test.schema, got, test.want)
			}
		})
	}
}

// TestValidate covers malformed configuration before it reaches runtime
// authorization. The 64-character limit follows MySQL's schema-name limit and
// counts Unicode characters rather than UTF-8 bytes.
func TestValidate(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		pattern string
		wantErr bool
	}{
		{name: "suffix glob", pattern: "*_dev"},
		{name: "literal punctuation", pattern: "tenant?_dev"},
		{name: "unicode within character limit", pattern: strings.Repeat("库", 64)},
		{name: "empty", pattern: "", wantErr: true},
		{name: "too long", pattern: strings.Repeat("a", 65), wantErr: true},
		{name: "NUL", pattern: "tenant\x00*", wantErr: true},
		{name: "invalid UTF-8", pattern: string([]byte{0xff}), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(test.pattern)
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate(%q) error = %v, wantErr %v", test.pattern, err, test.wantErr)
			}
		})
	}
}

// TestAllowsAndRestricted fixes the combination rule used across policy,
// metadata, functions, and datasource routing. Exact names and patterns form
// one logical allow-list; an entirely empty list means unrestricted access.
func TestAllowsAndRestricted(t *testing.T) {
	t.Parallel()

	exact := []string{"shared"}
	patterns := []string{"*_dev"}
	if !Allows("shared", exact, patterns) {
		t.Fatal("exact schema should be allowed")
	}
	if !Allows("orders_dev", exact, patterns) {
		t.Fatal("pattern-matched schema should be allowed")
	}
	if Allows("orders_prod", exact, patterns) {
		t.Fatal("schema outside both allow-lists should be denied")
	}
	if !Restricted(exact, nil) || !Restricted(nil, patterns) {
		t.Fatal("either exact names or patterns must activate the restriction")
	}
	if Restricted(nil, nil) {
		t.Fatal("empty configuration must remain unrestricted")
	}
	if !Allows("any_visible_schema", nil, nil) {
		t.Fatal("empty configuration should allow every schema visible to MySQL")
	}
}

// TestToSQLLike proves that glob stars become LIKE wildcards while characters
// meaningful to LIKE remain literals. Using '=' as ESCAPE also requires a
// literal equals sign to be doubled.
func TestToSQLLike(t *testing.T) {
	t.Parallel()

	if got, want := ToSQLLike("tenant_%=*_dev"), "tenant=_=%==%=_dev"; got != want {
		t.Fatalf("ToSQLLike() = %q, want %q", got, want)
	}
	if got := SQLLike("*_dev"); got != "%=_dev" {
		t.Fatalf("SQLLike() alias = %q, want %%=_dev", got)
	}
}
