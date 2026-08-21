package app

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MegaXChan/mysql-mcp/internal/config"
	"github.com/MegaXChan/mysql-mcp/internal/datasource"
	"github.com/MegaXChan/mysql-mcp/internal/mcpserver"
)

func TestHTTPHandlerContractUsedByApp(t *testing.T) {
	t.Parallel()

	// serveHTTP delegates its route and authentication boundary to this shared
	// handler. Exercising it directly through httptest.ResponseRecorder keeps the
	// unit suite independent of socket permissions while protecting the app's
	// required /{datasource_name}/mcp contract and bearer-token behavior.
	handler, err := mcpserver.NewHTTPHandler(map[string]http.Handler{
		"alpha": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "alpha")
		}),
		"beta": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "beta")
		}),
	}, mcpserver.HTTPOptions{
		AuthMode: mcpserver.AuthModeToken,
		Token:    "correct-token",
		Ready:    func() bool { return true },
	})
	if err != nil {
		t.Fatalf("NewHTTPHandler() error = %v", err)
	}

	// Health probes deliberately remain unauthenticated so an orchestrator can
	// make health decisions without possessing a database-capable MCP token.
	response := performHandlerRequest(handler, http.MethodGet, "/healthz", "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", response.Code, http.StatusOK)
	}

	// Missing and incorrect credentials are rejected at the endpoint boundary.
	// The challenge header matters to standards-compliant MCP HTTP clients.
	for _, token := range []string{"", "wrong-token"} {
		response = performHandlerRequest(handler, http.MethodPost, "/alpha/mcp", token)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("POST /alpha/mcp with token %q status = %d, want %d", token, response.Code, http.StatusUnauthorized)
		}
		if got := response.Header().Get("WWW-Authenticate"); got != `Bearer realm="mysql-mcp"` {
			t.Fatalf("WWW-Authenticate = %q, want MySQL MCP bearer challenge", got)
		}
	}

	// Each exact path reaches only its pre-bound data-source handler. The request
	// contains no datasource field, so there is no opportunity to pivot sources.
	for _, test := range []struct {
		path string
		body string
	}{
		{path: "/alpha/mcp", body: "alpha"},
		{path: "/beta/mcp", body: "beta"},
	} {
		response = performHandlerRequest(handler, http.MethodPost, test.path, "correct-token")
		if response.Code != http.StatusOK || response.Body.String() != test.body {
			t.Fatalf("POST %s = (%d, %q), want (200, %q)", test.path, response.Code, response.Body.String(), test.body)
		}
	}

	// No shared endpoint, wildcard fallback, or trailing-slash alias is exposed.
	// This prevents ambiguous routing from weakening fixed-source isolation.
	for _, path := range []string{"/mcp", "/missing/mcp", "/alpha/mcp/"} {
		response = performHandlerRequest(handler, http.MethodPost, path, "correct-token")
		if response.Code != http.StatusNotFound {
			t.Fatalf("POST %s status = %d, want %d", path, response.Code, http.StatusNotFound)
		}
	}
}

func TestServeHTTPPassesResolvedTokenIntoHandlerAssembly(t *testing.T) {
	// This test reaches serveHTTP itself without opening a listener. A malformed
	// listen address fails only after MCP servers and the authenticated router are
	// assembled. Therefore the expected listen error also proves that the token
	// resolved by config.Load survived the app wiring; an empty or omitted token
	// would fail earlier in NewHTTPHandler with an authentication error.
	t.Setenv("MYSQL_MCP_APP_HTTP_PASSWORD", "database-secret")
	t.Setenv("MYSQL_MCP_APP_HTTP_TOKEN", "correct-token")
	cfg := loadHTTPAppTestConfig(t)
	registry, mocks := openAppTestRegistry(t, cfg)
	t.Cleanup(func() {
		if err := registry.Close(); err != nil {
			t.Errorf("registry.Close() error = %v", err)
		}
		for name, mock := range mocks {
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("sqlmock expectations for %q: %v", name, err)
			}
		}
	})

	cfg.Server.HTTP.Listen = "not-a-host-port"
	err := serveHTTP(
		context.Background(),
		cfg,
		registry,
		"test-version",
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "serve HTTP on not-a-host-port") {
		t.Fatalf("serveHTTP() error = %v, want post-assembly listen error", err)
	}
	if strings.Contains(err.Error(), "token authentication") {
		t.Fatalf("serveHTTP() lost the resolved bearer token: %v", err)
	}
}

func TestCLIRejectsInvalidFlagsBeforeDatabaseStartup(t *testing.T) {
	t.Parallel()

	// urfave/cli must reject malformed invocations before serve can attempt a
	// MySQL connection. This is especially important for automation: a typo must
	// fail deterministically rather than looking like a database outage.
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "serve requires config",
			args: []string{"mysql-mcp", "serve"},
			want: "Required flag \"config\" not set",
		},
		{
			name: "serve rejects unknown flag",
			args: []string{"mysql-mcp", "serve", "--not-a-real-flag"},
			want: "flag provided but not defined",
		},
		{
			name: "datasource flag does not leak into validation command",
			args: []string{"mysql-mcp", "validate-config", "--datasource", "alpha"},
			want: "flag provided but not defined",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			command := NewCommand("test", "test-commit")
			command.Writer = io.Discard
			command.ErrWriter = io.Discard
			err := command.Run(context.Background(), test.args)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Run(%v) error = %v, want text %q", test.args, err, test.want)
			}
		})
	}
}

func TestCLIRejectsInvalidLogLevelBeforeOpeningDatasource(t *testing.T) {
	// The config is otherwise valid and names an unreachable-looking MySQL
	// address. Receiving the log-level error proves CLI validation happens before
	// datasource.OpenRegistry and therefore cannot trigger a network dependency.
	t.Setenv("MYSQL_MCP_APP_CLI_PASSWORD", "database-secret")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`version: 1
datasources:
  - name: primary
    address: 127.0.0.1:1
    credentials:
      read:
        username: reader
        password_env: MYSQL_MCP_APP_CLI_PASSWORD
`)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	command := NewCommand("test", "test-commit")
	command.Writer = io.Discard
	command.ErrWriter = io.Discard
	err := command.Run(context.Background(), []string{
		"mysql-mcp", "--log-level", "verbose", "serve", "--config", configPath,
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported log level "verbose"`) {
		t.Fatalf("Run() error = %v, want unsupported log level", err)
	}
}

func TestHTTPRequestBodyLimitSaturates(t *testing.T) {
	t.Parallel()

	// A very large configured SQL limit must not overflow to a negative body
	// limit and accidentally select an unrelated transport default.
	if got := httpRequestBodyLimit(1024); got != 1024+(1<<20) {
		t.Fatalf("httpRequestBodyLimit(1024) = %d", got)
	}
	if got := httpRequestBodyLimit(math.MaxInt64); got != math.MaxInt64 {
		t.Fatalf("httpRequestBodyLimit(MaxInt64) = %d, want saturation", got)
	}
}

func TestHTTPServerBoundsActiveRequestIO(t *testing.T) {
	t.Parallel()

	// IdleTimeout does not apply while a request body or response is active.
	// Explicit read/write deadlines prevent slow authenticated clients from
	// retaining an unbounded number of HTTP handlers before tool dispatch.
	server := newHTTPServer("127.0.0.1:8080", http.NotFoundHandler(), 10*time.Second, io.Discard)
	if server.ReadHeaderTimeout != 5*time.Second || server.ReadTimeout != 30*time.Second {
		t.Fatalf("HTTP read timeouts = header:%v request:%v", server.ReadHeaderTimeout, server.ReadTimeout)
	}
	if server.WriteTimeout != 25*time.Second {
		t.Fatalf("HTTP write timeout = %v, want query timeout plus response allowance", server.WriteTimeout)
	}
	if got := httpWriteTimeout(time.Duration(math.MaxInt64)); got != time.Duration(math.MaxInt64) {
		t.Fatalf("httpWriteTimeout(MaxInt64) = %v, want saturation", got)
	}
	cfg := config.Defaults()
	cfg.Server.Limits.QueryTimeout = time.Second
	cfg.Datasources = []config.DatasourceConfig{{
		Name: "monitored", Monitoring: config.Monitoring{Enabled: true, QueryTimeout: time.Minute},
	}}
	if got := maximumExecutionTimeout(&cfg); got != time.Minute {
		t.Fatalf("maximumExecutionTimeout() = %v, want monitor timeout", got)
	}
}

func performHandlerRequest(handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func loadHTTPAppTestConfig(t *testing.T) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`version: 1
server:
  transport: http
  http:
    listen: 127.0.0.1:8080
    auth:
      mode: token
      token_env: MYSQL_MCP_APP_HTTP_TOKEN
datasources:
  - name: beta
    address: 127.0.0.1:3306
    credentials:
      read:
        username: reader
        password_env: MYSQL_MCP_APP_HTTP_PASSWORD
  - name: alpha
    address: 127.0.0.1:3306
    credentials:
      read:
        username: reader
        password_env: MYSQL_MCP_APP_HTTP_PASSWORD
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write HTTP test config: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load() error = %v", err)
	}
	return cfg
}

func openAppTestRegistry(t *testing.T, cfg *config.Config) (*datasource.Registry, map[string]sqlmock.Sqlmock) {
	t.Helper()
	databases := make(map[string]*sql.DB, len(cfg.Datasources))
	mocks := make(map[string]sqlmock.Sqlmock, len(cfg.Datasources))
	for _, configured := range cfg.Datasources {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New(%q): %v", configured.Name, err)
		}
		mock.ExpectClose()
		databases[configured.Name] = db
		mocks[configured.Name] = mock
	}

	registry, err := datasource.OpenRegistry(context.Background(), cfg, datasource.RegistryOptions{
		OpenPool: func(
			_ context.Context,
			configured config.DatasourceConfig,
			_ config.Credential,
			role datasource.Role,
			_ time.Duration,
		) (*sql.DB, error) {
			if role != datasource.RoleRead {
				return nil, fmt.Errorf("unexpected %s pool for read-only app test", role)
			}
			return databases[configured.Name], nil
		},
		DetectVersion: func(context.Context, datasource.QueryRower) (datasource.Version, error) {
			return datasource.Version{Raw: "8.0.42", Major: 8, Minor: 0, Patch: 42}, nil
		},
	})
	if err != nil {
		for _, db := range databases {
			_ = db.Close()
		}
		t.Fatalf("datasource.OpenRegistry() error = %v", err)
	}
	return registry, mocks
}
