package mcpserver

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MegaXChan/mysql-mcp/internal/config"
	"github.com/MegaXChan/mysql-mcp/internal/database"
	"github.com/MegaXChan/mysql-mcp/internal/datasource"
	"github.com/MegaXChan/mysql-mcp/internal/policy"
)

func TestMonitorToolsHideUnsupportedPerformanceSchemaCapabilities(t *testing.T) {
	// tools/list is a capability contract. With Performance Schema disabled,
	// MySQL 8 lock/digest tools must be absent instead of being advertised only
	// to fail every call. Other configured fixed-query tools remain available.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	source := monitorToolTestSource(t, database.Capability{
		Family: database.MySQL80, Major: 8, Minor: 0, Patch: 36, PerformanceSchema: false,
	})
	client, closeSessions := connectTestMCP(t, ctx, source)
	defer closeSessions()

	listed, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	names := make(map[string]bool, len(listed.Tools))
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	for _, expected := range []string{
		"mysql.monitor.overview", "mysql.monitor.storage", "mysql.monitor.sessions",
		"mysql.monitor.replication", "mysql.monitor.innodb_status",
	} {
		if !names[expected] {
			t.Errorf("supported monitor tool %q is missing: %v", expected, names)
		}
	}
	for _, unsupported := range []string{"mysql.monitor.locks", "mysql.monitor.top_queries"} {
		if names[unsupported] {
			t.Errorf("unsupported monitor tool %q was advertised", unsupported)
		}
	}
}

func TestMySQL57LocksRemainAvailableWithoutPerformanceSchema(t *testing.T) {
	// MySQL 5.7 uses INFORMATION_SCHEMA.INNODB_LOCK_WAITS, so disabling
	// Performance Schema must hide digest summaries but not the lock tool.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	source := monitorToolTestSource(t, database.Capability{
		Family: database.MySQL57, Major: 5, Minor: 7, Patch: 44, PerformanceSchema: false,
	})
	client, closeSessions := connectTestMCP(t, ctx, source)
	defer closeSessions()

	listed, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	names := make(map[string]bool, len(listed.Tools))
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	if !names["mysql.monitor.locks"] || names["mysql.monitor.top_queries"] {
		t.Fatalf("MySQL 5.7 monitor tools = %v, want locks and no top_queries", names)
	}
}

func monitorToolTestSource(t *testing.T, capability database.Capability) *datasource.Source {
	t.Helper()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	query, err := database.NewQueryExecutor(db, database.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := database.NewMetadataService(db, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	monitor, err := database.NewMonitorService(db, capability, database.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	version := capability.ServerVersion
	if version == "" {
		version = "8.0.36"
		if capability.Family == database.MySQL57 {
			version = "5.7.44"
		}
	}
	sqlPolicy, err := policy.New(version)
	if err != nil {
		t.Fatal(err)
	}
	return &datasource.Source{
		Name:        "primary",
		Version:     datasource.Version{Raw: version, Major: capability.Major, Minor: capability.Minor, Patch: capability.Patch},
		Policy:      sqlPolicy,
		MaxRows:     100,
		DefaultRows: 20,
		MaxSQLBytes: 1 << 20,
		Monitoring: config.Monitoring{
			Enabled: true, Sessions: true, Locks: true, TopQueries: true,
			Replication: true, InnoDBStatus: true,
		},
		Services: datasource.Services{Query: query, Metadata: metadata, Monitor: monitor},
	}
}
