package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewHTTPHandlerRoutesEachDatasource(t *testing.T) {
	t.Parallel()

	// Distinct marker bodies prove that each URL is bound to its own server and
	// cannot use a request parameter to switch to another configured data source.
	endpoints := map[string]http.Handler{
		"primary": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("primary")) }),
		"audit":   http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("audit")) }),
	}
	handler, err := NewHTTPHandler(endpoints, HTTPOptions{AuthMode: AuthModeNone})
	if err != nil {
		t.Fatalf("NewHTTPHandler() error = %v", err)
	}

	tests := []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{path: "/primary/mcp", wantStatus: http.StatusOK, wantBody: "primary"},
		{path: "/audit/mcp", wantStatus: http.StatusOK, wantBody: "audit"},
		{path: "/unknown/mcp", wantStatus: http.StatusNotFound},
		{path: "/primary/mcp/extra", wantStatus: http.StatusNotFound},
	}
	for _, test := range tests {
		request := httptest.NewRequest(http.MethodPost, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.wantStatus {
			t.Errorf("POST %s status = %d, want %d", test.path, response.Code, test.wantStatus)
		}
		if test.wantBody != "" && strings.TrimSpace(response.Body.String()) != test.wantBody {
			t.Errorf("POST %s body = %q, want %q", test.path, response.Body.String(), test.wantBody)
		}
	}
}

func TestNewHTTPHandlerProtectsOnlyMCPRoutes(t *testing.T) {
	t.Parallel()

	endpoint := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler, err := NewHTTPHandler(map[string]http.Handler{"primary": endpoint}, HTTPOptions{
		AuthMode: AuthModeToken,
		Token:    "secret",
		Ready:    func() bool { return false },
	})
	if err != nil {
		t.Fatalf("NewHTTPHandler() error = %v", err)
	}

	// MCP is protected, while orchestration systems can call the metadata-free
	// probes without receiving or storing the database access token.
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/primary/mcp", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated MCP status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	authorizedRequest := httptest.NewRequest(http.MethodPost, "/primary/mcp", nil)
	authorizedRequest.Header.Set("Authorization", "Bearer secret")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusNoContent {
		t.Fatalf("authenticated MCP status = %d, want %d", authorized.Code, http.StatusNoContent)
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Errorf("health status = %d, want %d", health.Code, http.StatusOK)
	}
	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Errorf("readiness status = %d, want %d", ready.Code, http.StatusServiceUnavailable)
	}
}

func TestNewHTTPHandlerRejectsUnsafeConfiguration(t *testing.T) {
	t.Parallel()

	endpoint := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	tests := []struct {
		name      string
		endpoints map[string]http.Handler
		options   HTTPOptions
	}{
		{name: "no endpoints", endpoints: map[string]http.Handler{}, options: HTTPOptions{AuthMode: AuthModeNone}},
		{name: "unknown auth mode", endpoints: map[string]http.Handler{"db": endpoint}, options: HTTPOptions{AuthMode: "basic"}},
		{name: "empty token", endpoints: map[string]http.Handler{"db": endpoint}, options: HTTPOptions{AuthMode: AuthModeToken}},
		{name: "token with whitespace", endpoints: map[string]http.Handler{"db": endpoint}, options: HTTPOptions{AuthMode: AuthModeToken, Token: "not representable"}},
		{name: "token with invalid padding", endpoints: map[string]http.Handler{"db": endpoint}, options: HTTPOptions{AuthMode: AuthModeToken, Token: "prefix=suffix"}},
		{name: "path separator", endpoints: map[string]http.Handler{"one/two": endpoint}, options: HTTPOptions{AuthMode: AuthModeNone}},
		{name: "encoded path", endpoints: map[string]http.Handler{"one%2Ftwo": endpoint}, options: HTTPOptions{AuthMode: AuthModeNone}},
		{name: "nil handler", endpoints: map[string]http.Handler{"db": nil}, options: HTTPOptions{AuthMode: AuthModeNone}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewHTTPHandler(test.endpoints, test.options); err == nil {
				t.Fatal("NewHTTPHandler() error = nil, want configuration error")
			}
		})
	}
}
