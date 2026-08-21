package mcpserver

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MegaXChan/mysql-mcp/internal/config"
	"github.com/MegaXChan/mysql-mcp/internal/datasource"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReadOnlyMCPServerEndToEnd(t *testing.T) {
	// This uses the SDK's real client/server transports and sqlmock's real
	// database/sql driver. It verifies tool discovery, JSON input decoding,
	// Vitess authorization, placeholder binding, read-only transactions, and
	// structured MCP output as one request path.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	readDB, readMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	registry := openTestRegistry(t, readDB, nil, false)
	source, _ := registry.Source("primary")
	clientSession, closeSessions := connectTestMCP(t, ctx, source)
	defer closeSessions()

	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	names := make(map[string]bool, len(listed.Tools))
	for _, tool := range listed.Tools {
		names[tool.Name] = true
	}
	if !names["mysql.query"] || !names["mysql.metadata.tables"] || names["mysql.execute"] {
		t.Fatalf("read-only tools = %v; want query/metadata and no execute", names)
	}

	statement := "SELECT id FROM app.users WHERE id = ?"
	readMock.ExpectBegin()
	readMock.ExpectQuery(regexp.QuoteMeta(statement)).WithArgs(float64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(7)))
	readMock.ExpectRollback()
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "mysql.query",
		Arguments: map[string]any{
			"sql":  statement,
			"args": []any{7},
		},
	})
	if err != nil {
		t.Fatalf("CallTool(mysql.query) protocol error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool(mysql.query) returned tool error: %+v", result.Content)
	}

	// A DELETE sent to the read tool is rejected by the AST policy before
	// database/sql is touched. MCP represents handler failures as tool errors.
	rejected, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mysql.query",
		Arguments: map[string]any{"sql": "DELETE FROM app.users"},
	})
	if err != nil {
		t.Fatalf("CallTool(rejected query) protocol error = %v", err)
	}
	if !rejected.IsError {
		t.Fatal("DELETE through mysql.query was not rejected")
	}
	if err := readMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("read database expectations: %v", err)
	}
}

func TestSchemaPatternMCPQueryAuthorization(t *testing.T) {
	// This adapter-level regression proves allowed_schema_patterns is forwarded
	// from the bound datasource into the SQL policy. A matching qualified schema
	// and a matching default database may execute; a non-matching schema must be
	// rejected before database/sql receives any operation.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	readDB, readMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	registry := openTestRegistry(t, readDB, nil, false)
	source, _ := registry.Source("primary")
	source.DefaultDatabase = "orders_dev"
	source.AllowedSchemas = nil
	source.AllowedSchemaPatterns = []string{"*_dev"}
	clientSession, closeSessions := connectTestMCP(t, ctx, source)
	defer closeSessions()

	for _, statement := range []string{
		"SELECT id FROM analytics_dev.events",
		"SELECT id FROM events",
	} {
		readMock.ExpectBegin()
		readMock.ExpectQuery(regexp.QuoteMeta(statement)).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
		readMock.ExpectRollback()
		result, callErr := clientSession.CallTool(ctx, &mcp.CallToolParams{
			Name:      "mysql.query",
			Arguments: map[string]any{"sql": statement},
		})
		if callErr != nil {
			t.Fatalf("CallTool(mysql.query %q) protocol error = %v", statement, callErr)
		}
		if result.IsError {
			t.Fatalf("CallTool(mysql.query %q) returned tool error: %+v", statement, result.Content)
		}
	}

	rejected, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mysql.query",
		Arguments: map[string]any{"sql": "SELECT id FROM analytics_prod.events"},
	})
	if err != nil {
		t.Fatalf("CallTool(non-matching schema) protocol error = %v", err)
	}
	if !rejected.IsError {
		t.Fatal("non-matching schema passed allowed_schema_patterns")
	}
	if err := readMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("read database expectations: %v", err)
	}
}

func TestWritableMCPServerExecutesOnlyEnabledClass(t *testing.T) {
	// A writable configuration gets a separate writer pool and exposes the one
	// command tool. The test proves the DML route commits on that writer and that
	// DDL remains denied when only the DML feature is enabled.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	readDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	writeDB, writeMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	registry := openTestRegistry(t, readDB, writeDB, true)
	source, _ := registry.Source("primary")
	clientSession, closeSessions := connectTestMCP(t, ctx, source)
	defer closeSessions()

	statement := "UPDATE app.users SET status = ? WHERE id = ?"
	writeMock.ExpectBegin()
	writeMock.ExpectExec(regexp.QuoteMeta(statement)).WithArgs("active", float64(3)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	writeMock.ExpectCommit()
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "mysql.execute",
		Arguments: map[string]any{
			"sql":  statement,
			"args": []any{"active", 3},
		},
	})
	if err != nil {
		t.Fatalf("CallTool(mysql.execute) protocol error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool(mysql.execute) returned tool error: %+v", result.Content)
	}

	ddl, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mysql.execute",
		Arguments: map[string]any{"sql": "DROP TABLE app.users"},
	})
	if err != nil {
		t.Fatalf("CallTool(disabled DDL) protocol error = %v", err)
	}
	if !ddl.IsError {
		t.Fatal("DDL was accepted when only the DML feature is enabled")
	}
	storedFunction, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mysql.execute",
		Arguments: map[string]any{"sql": "UPDATE app.users SET score = app.recalculate_score(id)"},
	})
	if err != nil {
		t.Fatalf("CallTool(raw stored function) protocol error = %v", err)
	}
	if !storedFunction.IsError {
		t.Fatal("schema-qualified stored function bypassed mysql.function.call")
	}
	if err := writeMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("write database expectations: %v", err)
	}
}

func TestWritableMCPServerExecutesDDLWithoutTransactionWrapper(t *testing.T) {
	// MySQL implicitly commits DDL. This end-to-end adapter test protects the
	// class-specific dispatch: authorized DDL must use ExecDDL directly and must
	// not issue the BEGIN/COMMIT expected by the DML path above.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	readDB, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	writeDB, writeMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	registry := openTestRegistryWithFeatures(t, readDB, writeDB, config.FeatureConfig{DDL: true})
	source, _ := registry.Source("primary")
	clientSession, closeSessions := connectTestMCP(t, ctx, source)
	defer closeSessions()

	statement := "CREATE TABLE app.audit_log (id BIGINT PRIMARY KEY)"
	writeMock.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mysql.execute",
		Arguments: map[string]any{"sql": statement},
	})
	if err != nil {
		t.Fatalf("CallTool(mysql.execute DDL) protocol error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool(mysql.execute DDL) returned tool error: %+v", result.Content)
	}
	if err := writeMock.ExpectationsWereMet(); err != nil {
		t.Fatalf("write database expectations: %v", err)
	}
}

func TestQueryInputLimits(t *testing.T) {
	t.Parallel()

	// These are transport-adapter boundaries that do not require MySQL: SQL byte
	// size, response row bounds, and scalar-only arguments must all fail closed.
	source := &datasource.Source{DefaultRows: 20, MaxRows: 100, MaxSQLBytes: 8}
	if err := validateSQLInput(source, "SELECT 123", nil); err == nil {
		t.Fatal("oversized SQL was accepted")
	}
	if err := validateScalarArguments([]any{map[string]any{"expression": "NOW()"}}); err == nil {
		t.Fatal("non-scalar SQL argument was accepted")
	}
	if err := validateScalarArguments([]any{float64(1 << 53)}); err == nil {
		t.Fatal("inexact JSON integer was accepted instead of requiring a decimal string")
	}
	if err := validateScalarArguments([]any{float64(1<<53 - 1), "9007199254740993"}); err != nil {
		t.Fatalf("exact JSON integer / decimal string rejected: %v", err)
	}
	if got, err := responseRowLimit(source, 0); err != nil || got != 20 {
		t.Fatalf("default response limit = %d, %v; want 20", got, err)
	}
	if _, err := responseRowLimit(source, 101); err == nil {
		t.Fatal("response limit above configured maximum was accepted")
	}
	metadata, truncated := trimMetadataRows([]int{1, 2, 3}, 2)
	if !truncated || len(metadata) != 2 {
		t.Fatalf("trimMetadataRows() = %v, %v; want two rows marked truncated", metadata, truncated)
	}
}

func openTestRegistry(t *testing.T, readDB, writeDB *sql.DB, writable bool) *datasource.Registry {
	t.Helper()
	features := config.FeatureConfig{}
	if writable {
		features.DML = true
	}
	return openTestRegistryWithFeatures(t, readDB, writeDB, features)
}

func openTestRegistryWithFeatures(t *testing.T, readDB, writeDB *sql.DB, features config.FeatureConfig) *datasource.Registry {
	t.Helper()
	cfg := config.Defaults()
	cfg.Datasources = []config.DatasourceConfig{{
		Name:            "primary",
		Network:         "tcp",
		Address:         "127.0.0.1:3306",
		DefaultDatabase: "app",
		AllowedSchemas:  []string{"app"},
		Credentials: config.Credentials{
			Read: config.Credential{Username: "reader", PasswordEnv: "TEST_READ_PASSWORD"},
		},
		TLS: config.TLS{Mode: "disabled"},
		Pool: config.Pool{
			MaxOpen:         2,
			MaxIdle:         1,
			ConnMaxLifetime: time.Minute,
			ConnMaxIdleTime: time.Minute,
		},
		Monitoring: config.Monitoring{QueryTimeout: time.Second},
	}}
	if features.AnyWrite() {
		cfg.Server.ReadOnly = false
		cfg.Server.Features = features
		cfg.Datasources[0].Credentials.Write = config.Credential{Username: "writer", PasswordEnv: "TEST_WRITE_PASSWORD"}
	}
	opener := func(_ context.Context, _ config.DatasourceConfig, _ config.Credential, role datasource.Role, _ time.Duration) (*sql.DB, error) {
		switch role {
		case datasource.RoleRead:
			return readDB, nil
		case datasource.RoleWrite:
			if writeDB == nil {
				return nil, errors.New("unexpected writer open")
			}
			return writeDB, nil
		default:
			return nil, errors.New("unexpected pool role")
		}
	}
	registry, err := datasource.OpenRegistry(context.Background(), &cfg, datasource.RegistryOptions{
		OpenPool: opener,
		DetectVersion: func(context.Context, datasource.QueryRower) (datasource.Version, error) {
			return datasource.Version{Raw: "8.0.36", Comment: "MySQL Community Server", Major: 8, Minor: 0, Patch: 36}, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenRegistry() error = %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return registry
}

func connectTestMCP(t *testing.T, ctx context.Context, source *datasource.Source) (*mcp.ClientSession, func()) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server, err := NewServer(source, "test", logger)
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect() error = %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		t.Fatalf("client.Connect() error = %v", err)
	}
	// Build commit provenance belongs only to the human-facing CLI version.
	// The MCP initialize response must retain the plain application version so
	// clients that treat Implementation.Version as a semantic version do not
	// receive a display string such as "v1.2.3 (commit ...)".
	initializeResult := clientSession.InitializeResult()
	if initializeResult == nil || initializeResult.ServerInfo == nil {
		_ = clientSession.Close()
		_ = serverSession.Close()
		t.Fatal("MCP initialize result or server info is nil")
	}
	if got, want := initializeResult.ServerInfo.Version, "test"; got != want {
		_ = clientSession.Close()
		_ = serverSession.Close()
		t.Fatalf("MCP server version = %q, want plain version %q", got, want)
	}
	return clientSession, func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	}
}
