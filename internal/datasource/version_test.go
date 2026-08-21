package datasource

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()

	// Include common suffixes returned by managed services and distributions.
	// MariaDB may begin with a MySQL-compatible number, so it must be rejected
	// using both VERSION() and version_comment rather than the prefix alone.
	tests := []struct {
		name      string
		raw       string
		comment   string
		wantMajor int
		wantMinor int
		wantPatch int
		wantError bool
	}{
		{name: "mysql 5.7", raw: "5.7.44-log", comment: "MySQL Community Server (GPL)", wantMajor: 5, wantMinor: 7, wantPatch: 44},
		{name: "mysql 8.0", raw: "8.0.36", comment: "MySQL Community Server - GPL", wantMajor: 8, wantMinor: 0, wantPatch: 36},
		{name: "mysql 8.4", raw: "8.4.2-commercial", comment: "MySQL Enterprise Server", wantMajor: 8, wantMinor: 4, wantPatch: 2},
		{name: "mariadb numeric prefix", raw: "5.7.12-MariaDB", comment: "MariaDB Server", wantError: true},
		{name: "old mysql", raw: "5.6.51", comment: "MySQL Community Server", wantError: true},
		{name: "future unsupported major", raw: "9.0.1", comment: "MySQL Community Server", wantError: true},
		{name: "malformed", raw: "development", comment: "MySQL", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseVersion(test.raw, test.comment)
			if test.wantError {
				if err == nil {
					t.Fatalf("ParseVersion(%q) error = nil, want error", test.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseVersion(%q) error = %v", test.raw, err)
			}
			if got.Major != test.wantMajor || got.Minor != test.wantMinor || got.Patch != test.wantPatch {
				t.Fatalf("ParseVersion(%q) = %d.%d.%d, want %d.%d.%d", test.raw, got.Major, got.Minor, got.Patch, test.wantMajor, test.wantMinor, test.wantPatch)
			}
		})
	}
}

func TestDetectVersionQueriesBothVersionFields(t *testing.T) {
	t.Parallel()

	// The version comment is part of the trust decision because MariaDB often
	// presents a MySQL-compatible numeric VERSION() prefix.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta(versionQuery)).
		WillReturnRows(sqlmock.NewRows([]string{"version", "comment"}).AddRow("8.0.36", "MySQL Community Server"))
	got, err := DetectVersion(context.Background(), db)
	if err != nil {
		t.Fatalf("DetectVersion() error = %v", err)
	}
	if got.ParserVersion() != "8.0.36" {
		t.Fatalf("ParserVersion() = %q, want 8.0.36", got.ParserVersion())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
