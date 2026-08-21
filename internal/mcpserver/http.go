package mcpserver

import (
	"fmt"
	"net/http"
	"strings"
)

// HTTPOptions configures the shared HTTP router. Endpoints must already be
// bound to exactly one data source; the router only exposes each one at the
// required /{datasource_name}/mcp path.
type HTTPOptions struct {
	AuthMode AuthMode
	Token    string
	Ready    func() bool
}

// NewHTTPHandler builds an HTTP handler containing one MCP endpoint per data
// source plus lightweight liveness and readiness probes. Probe responses reveal
// no database metadata and therefore intentionally remain unauthenticated.
func NewHTTPHandler(endpoints map[string]http.Handler, options HTTPOptions) (http.Handler, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("at least one MCP endpoint is required")
	}
	if options.AuthMode != AuthModeNone && options.AuthMode != AuthModeToken {
		return nil, fmt.Errorf("unsupported authentication mode %q", options.AuthMode)
	}
	if options.AuthMode == AuthModeToken && !validBearerTokenValue(options.Token) {
		return nil, fmt.Errorf("token authentication requires an RFC 6750 b64token value")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if options.Ready != nil && !options.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})

	authenticate := AuthMiddleware(options.AuthMode, options.Token)
	for name, endpoint := range endpoints {
		if endpoint == nil {
			return nil, fmt.Errorf("MCP endpoint %q is nil", name)
		}
		if err := validateDatasourcePathName(name); err != nil {
			return nil, err
		}
		mux.Handle("/"+name+"/mcp", authenticate(endpoint))
	}
	return mux, nil
}

func validateDatasourcePathName(name string) error {
	if name == "" {
		return fmt.Errorf("data source name cannot be empty")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\?#%") {
		return fmt.Errorf("data source name %q is not a safe URL path segment", name)
	}
	return nil
}
