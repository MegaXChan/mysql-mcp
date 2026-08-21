package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

func TestRequestLoggingCapturesResponseOutcome(t *testing.T) {
	t.Parallel()

	// Each case exercises a distinct net/http response path. The access log must
	// describe what the wrapped writer actually accepted, including HTTP's
	// implicit 200 when a handler returns without calling WriteHeader.
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus int
		wantBytes  int64
	}{
		{
			name: "implicit status through Write",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(response, "hello")
			},
			wantStatus: http.StatusOK,
			wantBytes:  5,
		},
		{
			name: "custom status and body",
			handler: func(response http.ResponseWriter, _ *http.Request) {
				response.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(response, "accepted")
			},
			wantStatus: http.StatusAccepted,
			wantBytes:  8,
		},
		{
			name:       "implicit status without write",
			handler:    func(http.ResponseWriter, *http.Request) {},
			wantStatus: http.StatusOK,
			wantBytes:  0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			logger, logs := requestLogTestLogger()
			handler := requestLoggingMiddleware(test.handler, logger)
			request := httptest.NewRequest(http.MethodPatch, "/alpha/mcp?not_logged=true", nil)
			request.RemoteAddr = "203.0.113.10:43123"
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			entries := decodeRequestLogs(t, logs)
			if len(entries) != 1 {
				t.Fatalf("access log count = %d, want 1; logs=%s", len(entries), logs.String())
			}
			entry := entries[0]
			assertRequestLogFields(t, entry)
			if got := requestLogInt(t, entry, "status"); got != int64(test.wantStatus) {
				t.Fatalf("logged status = %d, want %d", got, test.wantStatus)
			}
			if got := requestLogInt(t, entry, "response_bytes"); got != test.wantBytes {
				t.Fatalf("logged response_bytes = %d, want %d", got, test.wantBytes)
			}
			if entry["method"] != http.MethodPatch || entry["path"] != "/alpha/mcp" || entry["remote_addr"] != request.RemoteAddr {
				t.Fatalf("unexpected request attributes: %#v", entry)
			}
			if duration := requestLogInt(t, entry, "duration_ms"); duration < 0 {
				t.Fatalf("duration_ms = %d, want non-negative", duration)
			}
			requestID, ok := entry["request_id"].(string)
			if !ok || requestID == "" || response.Header().Get(requestIDHeader) != requestID {
				t.Fatalf("request ID log/header mismatch: entry=%#v header=%q", entry["request_id"], response.Header().Get(requestIDHeader))
			}
		})
	}
}

func TestLoggingResponseWriterTracksFinalStatusAfterEarlyHints(t *testing.T) {
	t.Parallel()

	// httptest.ResponseRecorder intentionally models a single WriteHeader call,
	// so use a small protocol-accurate writer to exercise an informational 103
	// followed by the final response. The middleware must log only the final 202.
	logger, logs := requestLogTestLogger()
	underlying := newInformationalTestResponseWriter()
	handler := requestLoggingMiddleware(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusEarlyHints)
		response.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(response, "accepted")
	}), logger)
	handler.ServeHTTP(underlying, httptest.NewRequest(http.MethodPost, "/alpha/mcp", nil))

	if len(underlying.informationalStatuses) != 1 || underlying.informationalStatuses[0] != http.StatusEarlyHints {
		t.Fatalf("informational statuses = %v, want [103]", underlying.informationalStatuses)
	}
	if underlying.finalStatus != http.StatusAccepted || underlying.body.String() != "accepted" {
		t.Fatalf("underlying final response = (%d, %q), want (202, accepted)", underlying.finalStatus, underlying.body.String())
	}
	entries := decodeRequestLogs(t, logs)
	if len(entries) != 1 || requestLogInt(t, entries[0], "status") != http.StatusAccepted || requestLogInt(t, entries[0], "response_bytes") != 8 {
		t.Fatalf("early-hints access log = %#v, want final 202 and 8 bytes", entries)
	}
}

func TestRequestLoggingAuthenticationNotFoundAndSensitiveData(t *testing.T) {
	t.Parallel()

	// Use the real auth/router boundary so both early 401 and route-level 404
	// responses pass through the middleware. Sensitive query, header, body, and
	// client request-ID values must not appear anywhere in structured logs.
	router, err := mcpserver.NewHTTPHandler(map[string]http.Handler{
		"alpha": http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(response, "ok")
		}),
	}, mcpserver.HTTPOptions{AuthMode: mcpserver.AuthModeToken, Token: "correct-token", Ready: func() bool { return true }})
	if err != nil {
		t.Fatalf("NewHTTPHandler() error = %v", err)
	}
	logger, logs := requestLogTestLogger()
	handler := requestLoggingMiddleware(router, logger)

	unauthorized := httptest.NewRequest(
		http.MethodPost,
		"/alpha/mcp?query_secret=never-log-query",
		strings.NewReader(`{"sql":"SELECT never_log_body FROM secrets"}`),
	)
	unauthorized.Header.Set("Authorization", "Bearer never-log-token")
	unauthorized.Header.Set("X-Forwarded-For", "never-log-forwarded-address")
	unauthorized.Header.Set(requestIDHeader, "never-log-client-request-id")
	unauthorizedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthorizedResponse, unauthorized)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorizedResponse.Code, http.StatusUnauthorized)
	}
	if got := unauthorizedResponse.Header().Get(requestIDHeader); got == "" || got == "never-log-client-request-id" {
		t.Fatalf("server request ID = %q, want generated value", got)
	}

	notFound := httptest.NewRequest(http.MethodPost, "/missing/mcp", nil)
	notFound.Header.Set("Authorization", "Bearer correct-token")
	notFoundResponse := httptest.NewRecorder()
	handler.ServeHTTP(notFoundResponse, notFound)
	if notFoundResponse.Code != http.StatusNotFound {
		t.Fatalf("not-found status = %d, want %d", notFoundResponse.Code, http.StatusNotFound)
	}

	entries := decodeRequestLogs(t, logs)
	if len(entries) != 2 {
		t.Fatalf("access log count = %d, want 2; logs=%s", len(entries), logs.String())
	}
	assertRequestLogFields(t, entries[0])
	assertRequestLogFields(t, entries[1])
	if got := requestLogInt(t, entries[0], "status"); got != http.StatusUnauthorized {
		t.Fatalf("first logged status = %d, want 401", got)
	}
	if got := requestLogInt(t, entries[1], "status"); got != http.StatusNotFound {
		t.Fatalf("second logged status = %d, want 404", got)
	}
	for _, secret := range []string{
		"query_secret",
		"never-log-query",
		"Authorization",
		"never-log-token",
		"never-log-forwarded-address",
		"SELECT never_log_body",
		"never-log-client-request-id",
	} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("access log leaked %q: %s", secret, logs.String())
		}
	}
}

func TestRequestLoggingSkipsReadOnlyProbeRequests(t *testing.T) {
	t.Parallel()

	router, err := mcpserver.NewHTTPHandler(map[string]http.Handler{
		"alpha": http.NotFoundHandler(),
	}, mcpserver.HTTPOptions{
		AuthMode: mcpserver.AuthModeNone,
		Ready:    func() bool { return true },
	})
	if err != nil {
		t.Fatalf("NewHTTPHandler() error = %v", err)
	}
	logger, logs := requestLogTestLogger()
	handler := requestLoggingMiddleware(router, logger)

	// Query strings do not change URL.Path. Normal GET/HEAD orchestrator probes
	// produce neither a log entry nor a response request ID.
	for _, test := range []struct {
		method string
		target string
	}{
		{method: http.MethodGet, target: "/healthz?frequent=true"},
		{method: http.MethodHead, target: "/healthz?frequent=true"},
		{method: http.MethodGet, target: "/readyz?frequent=true"},
		{method: http.MethodHead, target: "/readyz?frequent=true"},
	} {
		request := httptest.NewRequest(test.method, test.target, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if got := response.Header().Get(requestIDHeader); got != "" {
			t.Errorf("%s %s response request ID = %q, want empty", test.method, test.target, got)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("probe requests produced access logs: %s", logs.String())
	}

	// Unsupported methods are not health checks even when their path is the
	// same. Their 405 responses retain normal observability and request IDs.
	for _, test := range []struct {
		method string
		target string
	}{
		{method: http.MethodPost, target: "/healthz?frequent=true"},
		{method: http.MethodDelete, target: "/readyz?frequent=true"},
	} {
		request := httptest.NewRequest(test.method, test.target, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s status = %d, want 405", test.method, test.target, response.Code)
		}
		if got := response.Header().Get(requestIDHeader); got == "" {
			t.Errorf("%s %s response request ID is empty", test.method, test.target)
		}
	}
	entries := decodeRequestLogs(t, logs)
	if len(entries) != 2 || requestLogInt(t, entries[0], "status") != http.StatusMethodNotAllowed || requestLogInt(t, entries[1], "status") != http.StatusMethodNotAllowed {
		t.Fatalf("unsupported probe-method logs = %#v, want two 405 entries", entries)
	}
}

func TestLoggingResponseWriterUnwrapSupportsFlush(t *testing.T) {
	t.Parallel()

	logger, logs := requestLogTestLogger()
	underlying := httptest.NewRecorder()
	var unwrapped http.ResponseWriter
	var flushError error
	handler := requestLoggingMiddleware(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		unwrapper, ok := response.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			flushError = errors.New("logging writer does not implement Unwrap")
			return
		}
		unwrapped = unwrapper.Unwrap()
		_, _ = io.WriteString(response, "chunk")
		flushError = http.NewResponseController(response).Flush()
	}), logger)

	handler.ServeHTTP(underlying, httptest.NewRequest(http.MethodPost, "/alpha/mcp", nil))

	if unwrapped != underlying {
		t.Fatalf("Unwrap() type = %T, want the underlying recorder", unwrapped)
	}
	if flushError != nil {
		t.Fatalf("ResponseController.Flush() error = %v", flushError)
	}
	if !underlying.Flushed {
		t.Fatal("ResponseController.Flush() did not reach the underlying writer")
	}
	entries := decodeRequestLogs(t, logs)
	if len(entries) != 1 || requestLogInt(t, entries[0], "response_bytes") != 5 {
		t.Fatalf("flush request logs = %#v, want one five-byte response", entries)
	}
}

func TestLoggingResponseWriterFlushCommitsImplicitStatus(t *testing.T) {
	t.Parallel()

	t.Run("late WriteHeader cannot replace flushed 200", func(t *testing.T) {
		logger, logs := requestLogTestLogger()
		underlying := httptest.NewRecorder()
		var flushError error
		handler := requestLoggingMiddleware(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			flushError = http.NewResponseController(response).Flush()
			response.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(response, "after")
		}), logger)
		handler.ServeHTTP(underlying, httptest.NewRequest(http.MethodPost, "/alpha/mcp", nil))

		if flushError != nil {
			t.Fatalf("ResponseController.Flush() error = %v", flushError)
		}
		if !underlying.Flushed || underlying.Code != http.StatusOK {
			t.Fatalf("flushed response = (flushed:%v status:%d), want (true, 200)", underlying.Flushed, underlying.Code)
		}
		entries := decodeRequestLogs(t, logs)
		if len(entries) != 1 || requestLogInt(t, entries[0], "status") != http.StatusOK || requestLogInt(t, entries[0], "response_bytes") != 5 {
			t.Fatalf("flush-then-WriteHeader log = %#v, want status 200 and 5 bytes", entries)
		}
	})

	t.Run("panic after flush retains committed 200", func(t *testing.T) {
		logger, logs := requestLogTestLogger()
		underlying := httptest.NewRecorder()
		panicValue := &struct{ message string }{message: "panic after flush"}
		var flushError error
		handler := requestLoggingMiddleware(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			flushError = http.NewResponseController(response).Flush()
			panic(panicValue)
		}), logger)
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			handler.ServeHTTP(underlying, httptest.NewRequest(http.MethodPost, "/alpha/mcp", nil))
		}()

		if flushError != nil {
			t.Fatalf("ResponseController.Flush() error = %v", flushError)
		}
		if recovered != panicValue {
			t.Fatalf("recovered panic = %#v, want original %#v", recovered, panicValue)
		}
		if !underlying.Flushed || underlying.Code != http.StatusOK {
			t.Fatalf("panic response = (flushed:%v status:%d), want committed 200", underlying.Flushed, underlying.Code)
		}
		entries := decodeRequestLogs(t, logs)
		if len(entries) != 1 || requestLogInt(t, entries[0], "status") != http.StatusOK || requestLogInt(t, entries[0], "response_bytes") != 0 {
			t.Fatalf("flush-then-panic log = %#v, want committed status 200 and 0 bytes", entries)
		}
	})
}

func TestRequestLoggingRepanicsAndRecordsInternalServerError(t *testing.T) {
	t.Parallel()

	// Before a response is committed, a panic represents an effective 500. Once
	// the handler has sent a final status, the log must retain the status the
	// client actually received rather than claiming an unsent 500.
	for _, test := range []struct {
		name       string
		write      func(http.ResponseWriter)
		wantStatus int64
		wantBytes  int64
	}{
		{
			name:       "panic before response",
			write:      func(http.ResponseWriter) {},
			wantStatus: http.StatusInternalServerError,
		},
		{
			name: "panic after committed response",
			write: func(response http.ResponseWriter) {
				response.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(response, "partial")
			},
			wantStatus: http.StatusCreated,
			wantBytes:  int64(len("partial")),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			logger, logs := requestLogTestLogger()
			panicValue := &struct{ message string }{message: test.name}
			handler := requestLoggingMiddleware(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				test.write(response)
				panic(panicValue)
			}), logger)
			response := httptest.NewRecorder()
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/alpha/mcp", nil))
			}()

			if recovered != panicValue {
				t.Fatalf("recovered panic = %#v, want original %#v", recovered, panicValue)
			}
			entries := decodeRequestLogs(t, logs)
			if len(entries) != 1 {
				t.Fatalf("panic access log count = %d, want 1; logs=%s", len(entries), logs.String())
			}
			if got := requestLogInt(t, entries[0], "status"); got != test.wantStatus {
				t.Fatalf("panic logged status = %d, want %d", got, test.wantStatus)
			}
			if got := requestLogInt(t, entries[0], "response_bytes"); got != test.wantBytes {
				t.Fatalf("panic logged response_bytes = %d, want %d", got, test.wantBytes)
			}
		})
	}
}

func TestRequestLoggingGeneratesUniqueServerRequestIDs(t *testing.T) {
	t.Parallel()

	handler := requestLoggingMiddleware(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	seen := make(map[string]struct{}, 128)
	for index := 0; index < 128; index++ {
		request := httptest.NewRequest(http.MethodPost, "/alpha/mcp", nil)
		request.Header.Set(requestIDHeader, "client-supplied-id")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		requestID := response.Header().Get(requestIDHeader)
		if requestID == "" || requestID == "client-supplied-id" {
			t.Fatalf("request %d generated ID = %q", index, requestID)
		}
		if _, duplicate := seen[requestID]; duplicate {
			t.Fatalf("duplicate request ID %q at request %d", requestID, index)
		}
		seen[requestID] = struct{}{}
	}
}

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

func requestLogTestLogger() (*slog.Logger, *bytes.Buffer) {
	logs := &bytes.Buffer{}
	return slog.New(slog.NewJSONHandler(logs, nil)), logs
}

func decodeRequestLogs(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(logs.Bytes()))
	var entries []map[string]any
	for {
		var entry map[string]any
		err := decoder.Decode(&entry)
		if errors.Is(err, io.EOF) {
			return entries
		}
		if err != nil {
			t.Fatalf("decode access log: %v; logs=%s", err, logs.String())
		}
		entries = append(entries, entry)
	}
}

func requestLogInt(t *testing.T, entry map[string]any, key string) int64 {
	t.Helper()
	value, ok := entry[key].(float64)
	if !ok || value != math.Trunc(value) {
		t.Fatalf("log field %q = %#v, want integer", key, entry[key])
	}
	return int64(value)
}

func assertRequestLogFields(t *testing.T, entry map[string]any) {
	t.Helper()
	standardFields := map[string]struct{}{"time": {}, "level": {}, "msg": {}}
	businessFields := map[string]struct{}{
		"method": {}, "path": {}, "status": {}, "response_bytes": {},
		"duration_ms": {}, "remote_addr": {}, "request_id": {},
	}
	for field := range entry {
		if _, standard := standardFields[field]; standard {
			continue
		}
		if _, expected := businessFields[field]; !expected {
			t.Fatalf("unexpected access-log field %q in %#v", field, entry)
		}
	}
	for field := range businessFields {
		if _, present := entry[field]; !present {
			t.Fatalf("access log omitted field %q: %#v", field, entry)
		}
	}
}

type informationalTestResponseWriter struct {
	header                http.Header
	informationalStatuses []int
	finalStatus           int
	body                  bytes.Buffer
}

func newInformationalTestResponseWriter() *informationalTestResponseWriter {
	return &informationalTestResponseWriter{header: make(http.Header)}
}

func (w *informationalTestResponseWriter) Header() http.Header { return w.header }

func (w *informationalTestResponseWriter) WriteHeader(status int) {
	if w.finalStatus != 0 {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.informationalStatuses = append(w.informationalStatuses, status)
		return
	}
	w.finalStatus = status
}

func (w *informationalTestResponseWriter) Write(data []byte) (int, error) {
	if w.finalStatus == 0 {
		w.finalStatus = http.StatusOK
	}
	return w.body.Write(data)
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
