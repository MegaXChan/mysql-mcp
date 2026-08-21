package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthMiddleware(t *testing.T) {
	t.Parallel()

	// The table covers every externally visible branch: disabled auth, a valid
	// token, missing/malformed headers, wrong schemes, wrong values, and values
	// containing extra fields. Each unauthorized request must stop before the
	// protected MCP handler runs.
	tests := []struct {
		name       string
		mode       AuthMode
		header     string
		wantStatus int
		wantCalled bool
	}{
		{name: "no authentication", mode: AuthModeNone, wantStatus: http.StatusNoContent, wantCalled: true},
		{name: "valid token", mode: AuthModeToken, header: "Bearer top-secret", wantStatus: http.StatusNoContent, wantCalled: true},
		{name: "case insensitive scheme", mode: AuthModeToken, header: "bearer top-secret", wantStatus: http.StatusNoContent, wantCalled: true},
		{name: "missing header", mode: AuthModeToken, wantStatus: http.StatusUnauthorized},
		{name: "missing value", mode: AuthModeToken, header: "Bearer", wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", mode: AuthModeToken, header: "Basic top-secret", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", mode: AuthModeToken, header: "Bearer other", wantStatus: http.StatusUnauthorized},
		{name: "extra field", mode: AuthModeToken, header: "Bearer top-secret extra", wantStatus: http.StatusUnauthorized},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			called := false
			protected := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})
			handler := AuthMiddleware(test.mode, "top-secret")(protected)

			request := httptest.NewRequest(http.MethodPost, "/primary/mcp", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if called != test.wantCalled {
				t.Fatalf("protected handler called = %v, want %v", called, test.wantCalled)
			}
			if test.wantStatus == http.StatusUnauthorized && response.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("unauthorized response is missing WWW-Authenticate header")
			}
		})
	}
}

func TestConstantTimeEqual(t *testing.T) {
	t.Parallel()

	// Include different-length and empty values because the middleware must not
	// accidentally accept zero-padded prefixes when it normalizes comparison
	// buffers to a common size.
	tests := []struct {
		left  string
		right string
		want  bool
	}{
		{left: "same", right: "same", want: true},
		{left: "", right: "", want: true},
		{left: "same", right: "different", want: false},
		{left: "a", right: "a\x00", want: false},
		{left: "", right: "non-empty", want: false},
	}

	for _, test := range tests {
		if got := constantTimeEqual(test.left, test.right); got != test.want {
			t.Errorf("constantTimeEqual(%q, %q) = %v, want %v", test.left, test.right, got, test.want)
		}
	}
}
