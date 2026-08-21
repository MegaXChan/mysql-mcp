package database

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type MySQLFamily string

const (
	MySQL57 MySQLFamily = "mysql57"
	MySQL80 MySQLFamily = "mysql80"
)

// Capability captures the version-sensitive facts needed by fixed monitoring
// queries. PerformanceSchema must reflect the runtime server setting rather
// than just the server version.
type Capability struct {
	Family            MySQLFamily `json:"family"`
	ServerVersion     string      `json:"server_version"`
	Major             int         `json:"major"`
	Minor             int         `json:"minor"`
	Patch             int         `json:"patch"`
	PerformanceSchema bool        `json:"performance_schema"`
}

var versionPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)`)

// ParseCapability accepts normal MySQL version strings such as 5.7.44-log and
// 8.0.36-commercial. Other families are rejected explicitly.
func ParseCapability(version string, performanceSchema bool) (Capability, error) {
	match := versionPattern.FindStringSubmatch(strings.TrimSpace(version))
	if len(match) != 4 {
		return Capability{}, invalid("parse capability", "invalid MySQL version")
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	capability := Capability{
		ServerVersion:     version,
		Major:             major,
		Minor:             minor,
		Patch:             patch,
		PerformanceSchema: performanceSchema,
	}
	switch {
	case major == 5 && minor == 7:
		capability.Family = MySQL57
	case major == 8:
		capability.Family = MySQL80
	default:
		return Capability{}, unsupported("parse capability", fmt.Sprintf("MySQL %d.%d", major, minor))
	}
	return capability, nil
}

func (c Capability) validate() error {
	switch c.Family {
	case MySQL57:
		if c.Major != 0 && (c.Major != 5 || c.Minor != 7) {
			return invalid("validate capability", "family/version mismatch")
		}
	case MySQL80:
		if c.Major != 0 && c.Major != 8 {
			return invalid("validate capability", "family/version mismatch")
		}
	default:
		return unsupported("validate capability", string(c.Family))
	}
	return nil
}

func (c Capability) usesReplicaTerminology() bool {
	return c.Family == MySQL80 && (c.Major == 0 || c.Minor > 0 || c.Patch >= 22)
}

// MonitorService executes only package-owned SQL templates. It intentionally
// has no method that accepts arbitrary SQL.
type MonitorService struct {
	executor   *QueryExecutor
	capability Capability
}

const (
	overviewSQL = `SELECT @@version AS version,
       @@version_comment AS version_comment,
       @@hostname AS hostname,
       @@port AS port,
       @@read_only AS read_only,
       @@super_read_only AS super_read_only,
       @@max_connections AS max_connections,
       (SELECT COUNT(*) FROM INFORMATION_SCHEMA.PROCESSLIST) AS current_connections`

	storageSQL = `SELECT TABLE_SCHEMA AS table_schema,
       COUNT(*) AS table_count,
       COALESCE(SUM(DATA_LENGTH), 0) AS data_bytes,
       COALESCE(SUM(INDEX_LENGTH), 0) AS index_bytes,
       COALESCE(SUM(DATA_FREE), 0) AS free_bytes
FROM INFORMATION_SCHEMA.TABLES
WHERE TABLE_SCHEMA NOT IN ('information_schema', 'mysql', 'performance_schema', 'sys')
GROUP BY TABLE_SCHEMA
ORDER BY data_bytes + index_bytes DESC`

	sessionsSQL = `SELECT ID AS connection_id, USER AS user, HOST AS host, DB AS current_schema,
       COMMAND AS command, TIME AS seconds_in_state, STATE AS state, INFO AS statement
FROM INFORMATION_SCHEMA.PROCESSLIST
ORDER BY TIME DESC
LIMIT 200`

	locks57SQL = `SELECT r.trx_mysql_thread_id AS waiting_connection_id,
       b.trx_mysql_thread_id AS blocking_connection_id,
       w.requested_lock_id AS requested_lock_id,
       w.blocking_lock_id AS blocking_lock_id,
       r.trx_query AS waiting_statement,
       b.trx_query AS blocking_statement
FROM INFORMATION_SCHEMA.INNODB_LOCK_WAITS AS w
JOIN INFORMATION_SCHEMA.INNODB_TRX AS r ON r.trx_id = w.requesting_trx_id
JOIN INFORMATION_SCHEMA.INNODB_TRX AS b ON b.trx_id = w.blocking_trx_id
ORDER BY r.trx_wait_started`

	locks80SQL = `SELECT waiting_thread.PROCESSLIST_ID AS waiting_connection_id,
       blocking_thread.PROCESSLIST_ID AS blocking_connection_id,
       waits.REQUESTING_ENGINE_LOCK_ID AS requested_lock_id,
       waits.BLOCKING_ENGINE_LOCK_ID AS blocking_lock_id,
       requested.OBJECT_SCHEMA AS object_schema,
       requested.OBJECT_NAME AS object_name,
       requested.LOCK_TYPE AS lock_type,
       requested.LOCK_MODE AS lock_mode
FROM performance_schema.data_lock_waits AS waits
JOIN performance_schema.data_locks AS requested
  ON requested.ENGINE = waits.ENGINE
 AND requested.ENGINE_LOCK_ID = waits.REQUESTING_ENGINE_LOCK_ID
JOIN performance_schema.data_locks AS blocking
  ON blocking.ENGINE = waits.ENGINE
 AND blocking.ENGINE_LOCK_ID = waits.BLOCKING_ENGINE_LOCK_ID
LEFT JOIN performance_schema.threads AS waiting_thread
  ON waiting_thread.THREAD_ID = waits.REQUESTING_THREAD_ID
LEFT JOIN performance_schema.threads AS blocking_thread
  ON blocking_thread.THREAD_ID = waits.BLOCKING_THREAD_ID
ORDER BY waiting_connection_id`

	topQueriesSQL = `SELECT SCHEMA_NAME AS schema_name,
       DIGEST AS digest,
       DIGEST_TEXT AS statement,
       COUNT_STAR AS execution_count,
       SUM_TIMER_WAIT AS total_wait_picoseconds,
       SUM_ROWS_EXAMINED AS rows_examined,
       SUM_ROWS_SENT AS rows_sent,
       FIRST_SEEN AS first_seen,
       LAST_SEEN AS last_seen
FROM performance_schema.events_statements_summary_by_digest
WHERE DIGEST IS NOT NULL
ORDER BY SUM_TIMER_WAIT DESC
LIMIT 100`

	innodbStatusSQL = `SHOW ENGINE INNODB STATUS`

	replication57SQL = `SHOW SLAVE STATUS`
	replication80SQL = `SHOW REPLICA STATUS`
)

func NewMonitorService(db *sql.DB, capability Capability, limits Limits) (*MonitorService, error) {
	if err := capability.validate(); err != nil {
		return nil, err
	}
	executor, err := NewQueryExecutor(db, limits)
	if err != nil {
		return nil, err
	}
	return &MonitorService{executor: executor, capability: capability}, nil
}

func (s *MonitorService) Overview(ctx context.Context) (QueryResult, error) {
	return s.run(ctx, "monitor overview", overviewSQL)
}

func (s *MonitorService) Storage(ctx context.Context) (QueryResult, error) {
	return s.run(ctx, "monitor storage", storageSQL)
}

func (s *MonitorService) Sessions(ctx context.Context) (QueryResult, error) {
	return s.run(ctx, "monitor sessions", sessionsSQL)
}

// SupportsLocks reports whether the version/runtime capability has a fixed
// lock-wait query. MySQL 5.7 uses INFORMATION_SCHEMA; MySQL 8 requires
// performance_schema data lock tables.
func (s *MonitorService) SupportsLocks() bool {
	return s.capability.Family == MySQL57 ||
		(s.capability.Family == MySQL80 && s.capability.PerformanceSchema)
}

// SupportsTopQueries reports whether digest aggregation is available.
func (s *MonitorService) SupportsTopQueries() bool {
	return s.capability.PerformanceSchema
}

func (s *MonitorService) Locks(ctx context.Context) (QueryResult, error) {
	switch s.capability.Family {
	case MySQL57:
		return s.run(ctx, "monitor locks", locks57SQL)
	case MySQL80:
		if !s.capability.PerformanceSchema {
			return QueryResult{}, unsupported("monitor locks", "performance_schema is disabled")
		}
		return s.run(ctx, "monitor locks", locks80SQL)
	default:
		return QueryResult{}, unsupported("monitor locks", string(s.capability.Family))
	}
}

func (s *MonitorService) TopQueries(ctx context.Context) (QueryResult, error) {
	if !s.capability.PerformanceSchema {
		return QueryResult{}, unsupported("monitor top queries", "performance_schema is disabled")
	}
	return s.run(ctx, "monitor top queries", topQueriesSQL)
}

// InnoDBStatus returns the server-generated InnoDB diagnostic snapshot. The
// adapter should treat the Status cell as sensitive because it can contain SQL
// fragments and transaction details.
func (s *MonitorService) InnoDBStatus(ctx context.Context) (QueryResult, error) {
	return s.run(ctx, "monitor InnoDB status", innodbStatusSQL)
}

func (s *MonitorService) Replication(ctx context.Context) (QueryResult, error) {
	if s.capability.usesReplicaTerminology() {
		return s.run(ctx, "monitor replication", replication80SQL)
	}
	return s.run(ctx, "monitor replication", replication57SQL)
}

func (s *MonitorService) run(ctx context.Context, op, statement string) (QueryResult, error) {
	result, err := s.executor.Query(ctx, statement)
	if err != nil {
		return result, wrapDatabaseError(op, err)
	}
	return result, nil
}
