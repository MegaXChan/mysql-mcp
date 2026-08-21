package datasource

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const versionQuery = "SELECT VERSION(), @@version_comment"

var mysqlVersionPattern = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?`)

// QueryRower is the minimal database capability required for server discovery.
// It is exported so embedders can inject deterministic discovery in tests.
type QueryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Version is the server version discovered from MySQL itself. Raw and Comment
// are retained for diagnostics; authorization decisions use numeric fields.
type Version struct {
	Raw     string `json:"raw"`
	Comment string `json:"comment,omitempty"`
	Major   int    `json:"major"`
	Minor   int    `json:"minor"`
	Patch   int    `json:"patch"`
}

// ParserVersion returns the value passed to Vitess's version-aware parser.
func (v Version) ParserVersion() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// DetectVersion queries and validates the target. mysql-mcp intentionally
// fails startup for MariaDB and for MySQL releases outside the supported 5.7
// and 8.x families rather than silently applying the wrong grammar.
func DetectVersion(ctx context.Context, db QueryRower) (Version, error) {
	if ctx == nil {
		return Version{}, fmt.Errorf("detect MySQL version: nil context")
	}
	if db == nil {
		return Version{}, fmt.Errorf("detect MySQL version: nil database")
	}
	var raw, comment string
	if err := db.QueryRowContext(ctx, versionQuery).Scan(&raw, &comment); err != nil {
		return Version{}, fmt.Errorf("detect MySQL version: %w", err)
	}
	return ParseVersion(raw, comment)
}

// ParseVersion validates a VERSION()/version_comment pair.
func ParseVersion(raw, comment string) (Version, error) {
	combined := strings.ToLower(raw + " " + comment)
	if strings.Contains(combined, "mariadb") {
		return Version{}, fmt.Errorf("unsupported server %q: MariaDB is not MySQL 5.7/8", raw)
	}
	match := mysqlVersionPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return Version{}, fmt.Errorf("unsupported MySQL version %q", raw)
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch := 0
	if match[3] != "" {
		patch, _ = strconv.Atoi(match[3])
	}
	if !((major == 5 && minor == 7) || major == 8) {
		return Version{}, fmt.Errorf("unsupported MySQL version %q: only MySQL 5.7 and 8.x are supported", raw)
	}
	return Version{Raw: raw, Comment: comment, Major: major, Minor: minor, Patch: patch}, nil
}
