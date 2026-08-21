package database

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestParseCapabilityRecognizesSupportedVersions(t *testing.T) {
	// Scenario: MySQL reports production-style version suffixes.
	// Risk covered: monitoring branches are selected from parsed numeric version
	// components rather than brittle full-string comparisons.
	tests := []struct {
		version string
		family  MySQLFamily
		patch   int
	}{
		{version: "5.7.44-log", family: MySQL57, patch: 44},
		{version: "8.0.36-commercial", family: MySQL80, patch: 36},
		{version: "8.4.1-lts", family: MySQL80, patch: 1},
	}
	for _, test := range tests {
		t.Run(test.version, func(t *testing.T) {
			capability, err := ParseCapability(test.version, true)
			if err != nil {
				t.Fatalf("ParseCapability() error = %v", err)
			}
			if capability.Family != test.family || capability.Patch != test.patch {
				t.Fatalf("ParseCapability() = %#v", capability)
			}
		})
	}
}

func TestParseCapabilityRejectsUnsupportedFamily(t *testing.T) {
	// Scenario: an unsupported major-minor version is configured.
	// Risk covered: the service does not silently run MySQL-specific monitoring
	// SQL against an unverified server family.
	_, err := ParseCapability("9.0.1", true)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ParseCapability() error = %v, want ErrUnsupported", err)
	}
}

func TestMonitorServiceAdvertisedCapabilitiesMatchRuntime(t *testing.T) {
	// MySQL 5.7 lock waits use INFORMATION_SCHEMA even when Performance Schema is
	// disabled. MySQL 8 locks and digest summaries require Performance Schema and
	// must not be advertised when its runtime switch is off.
	db, _ := newMockDatabase(t)
	tests := []struct {
		name       string
		capability Capability
		locks      bool
		topQueries bool
	}{
		{name: "mysql57 without performance schema", capability: Capability{Family: MySQL57, Major: 5, Minor: 7}, locks: true},
		{name: "mysql80 without performance schema", capability: Capability{Family: MySQL80, Major: 8}, locks: false},
		{name: "mysql80 with performance schema", capability: Capability{Family: MySQL80, Major: 8, PerformanceSchema: true}, locks: true, topQueries: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, err := NewMonitorService(db, test.capability, Limits{})
			if err != nil {
				t.Fatalf("NewMonitorService() error = %v", err)
			}
			if service.SupportsLocks() != test.locks || service.SupportsTopQueries() != test.topQueries {
				t.Fatalf("capabilities = locks:%v top:%v, want locks:%v top:%v",
					service.SupportsLocks(), service.SupportsTopQueries(), test.locks, test.topQueries)
			}
		})
	}
}

func TestMonitorServiceUsesMySQL57LockTables(t *testing.T) {
	// Scenario: lock inspection targets MySQL 5.7.
	// Risk covered: legacy INFORMATION_SCHEMA lock tables are used only for that
	// family and the query remains package-owned.
	db, mock := newMockDatabase(t)
	service, err := NewMonitorService(db, Capability{Family: MySQL57, Major: 5, Minor: 7, Patch: 44}, Limits{})
	if err != nil {
		t.Fatalf("NewMonitorService() error = %v", err)
	}
	expectReadQuery(mock, locks57SQL, sqlmock.NewRows([]string{
		"waiting_connection_id", "blocking_connection_id", "requested_lock_id",
		"blocking_lock_id", "waiting_statement", "blocking_statement",
	}))

	if _, err := service.Locks(context.Background()); err != nil {
		t.Fatalf("Locks() error = %v", err)
	}
	assertExpectations(t, mock)
}

func TestMonitorServiceUsesMySQL80PerformanceSchemaLocks(t *testing.T) {
	// Scenario: lock inspection targets MySQL 8.0 with Performance Schema.
	// Risk covered: the 8.0 data_locks model is selected instead of removed
	// INNODB_LOCKS tables.
	db, mock := newMockDatabase(t)
	service, err := NewMonitorService(db, Capability{
		Family: MySQL80, Major: 8, Minor: 0, Patch: 36, PerformanceSchema: true,
	}, Limits{})
	if err != nil {
		t.Fatalf("NewMonitorService() error = %v", err)
	}
	expectReadQuery(mock, locks80SQL, sqlmock.NewRows([]string{
		"waiting_connection_id", "blocking_connection_id", "requested_lock_id", "blocking_lock_id",
		"object_schema", "object_name", "lock_type", "lock_mode",
	}))

	if _, err := service.Locks(context.Background()); err != nil {
		t.Fatalf("Locks() error = %v", err)
	}
	assertExpectations(t, mock)
}

func TestMonitorServiceReportsPerformanceSchemaUnsupported(t *testing.T) {
	// Scenario: Performance Schema is disabled on MySQL 8.0.
	// Risk covered: tools that cannot work return ErrUnsupported before issuing
	// SQL, so adapters can communicate capability state rather than a vague SQL
	// error.
	db, mock := newMockDatabase(t)
	service, err := NewMonitorService(db, Capability{Family: MySQL80, Major: 8, Minor: 0, Patch: 36}, Limits{})
	if err != nil {
		t.Fatalf("NewMonitorService() error = %v", err)
	}
	if _, err := service.Locks(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Locks() error = %v, want ErrUnsupported", err)
	}
	if _, err := service.TopQueries(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("TopQueries() error = %v, want ErrUnsupported", err)
	}
	assertExpectations(t, mock)
}

func TestMonitorServiceSelectsReplicationTerminologyByPatch(t *testing.T) {
	// Scenario: REPLICA terminology exists only from MySQL 8.0.22.
	// Risk covered: 5.7 and early 8.0 use SHOW SLAVE STATUS while current 8.0
	// uses SHOW REPLICA STATUS.
	tests := []struct {
		name       string
		capability Capability
		statement  string
	}{
		{name: "mysql57", capability: Capability{Family: MySQL57, Major: 5, Minor: 7, Patch: 44}, statement: replication57SQL},
		{name: "mysql80-before-rename", capability: Capability{Family: MySQL80, Major: 8, Minor: 0, Patch: 21}, statement: replication57SQL},
		{name: "mysql80-replica", capability: Capability{Family: MySQL80, Major: 8, Minor: 0, Patch: 22}, statement: replication80SQL},
		{name: "mysql84-replica", capability: Capability{Family: MySQL80, Major: 8, Minor: 4, Patch: 1}, statement: replication80SQL},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock := newMockDatabase(t)
			service, err := NewMonitorService(db, test.capability, Limits{})
			if err != nil {
				t.Fatalf("NewMonitorService() error = %v", err)
			}
			expectReadQuery(mock, test.statement, sqlmock.NewRows([]string{"Source_Host"}))
			if _, err := service.Replication(context.Background()); err != nil {
				t.Fatalf("Replication() error = %v", err)
			}
			assertExpectations(t, mock)
		})
	}
}

func TestMonitorServiceMapsPermissionErrors(t *testing.T) {
	// Scenario: the monitoring account lacks PROCESS permission for sessions.
	// Risk covered: fixed-query failures retain a stable permission category and
	// the read-only transaction is still rolled back.
	db, mock := newMockDatabase(t)
	service, err := NewMonitorService(db, Capability{Family: MySQL57, Major: 5, Minor: 7, Patch: 44}, Limits{})
	if err != nil {
		t.Fatalf("NewMonitorService() error = %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(sessionsSQL)).WillReturnError(errors.New("PROCESS command denied"))
	mock.ExpectRollback()

	_, err = service.Sessions(context.Background())
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("Sessions() error = %v, want ErrPermissionDenied", err)
	}
	assertExpectations(t, mock)
}

func TestMonitorServiceOverviewStorageAndTopQueries(t *testing.T) {
	// Scenario: the remaining fixed monitor endpoints are exercised on MySQL 8.
	// Risk covered: every public monitor operation routes through the bounded
	// read-only executor and never accepts caller-provided SQL.
	db, mock := newMockDatabase(t)
	service, err := NewMonitorService(db, Capability{
		Family: MySQL80, Major: 8, Minor: 0, Patch: 36, PerformanceSchema: true,
	}, Limits{})
	if err != nil {
		t.Fatalf("NewMonitorService() error = %v", err)
	}
	expectReadQuery(mock, overviewSQL, sqlmock.NewRows([]string{"version"}).AddRow("8.0.36"))
	expectReadQuery(mock, storageSQL, sqlmock.NewRows([]string{"table_schema"}).AddRow("app"))
	expectReadQuery(mock, topQueriesSQL, sqlmock.NewRows([]string{"digest"}).AddRow("abc"))
	expectReadQuery(mock, innodbStatusSQL, sqlmock.NewRows([]string{"Type", "Name", "Status"}).AddRow("InnoDB", "", "diagnostic"))

	if _, err := service.Overview(context.Background()); err != nil {
		t.Fatalf("Overview() error = %v", err)
	}
	if _, err := service.Storage(context.Background()); err != nil {
		t.Fatalf("Storage() error = %v", err)
	}
	if _, err := service.TopQueries(context.Background()); err != nil {
		t.Fatalf("TopQueries() error = %v", err)
	}
	if _, err := service.InnoDBStatus(context.Background()); err != nil {
		t.Fatalf("InnoDBStatus() error = %v", err)
	}
	assertExpectations(t, mock)
}

func expectReadQuery(mock sqlmock.Sqlmock, statement string, rows *sqlmock.Rows) {
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(statement)).WillReturnRows(rows)
	mock.ExpectRollback()
}
