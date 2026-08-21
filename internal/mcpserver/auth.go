package mcpserver

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// AuthMode selects the authentication policy applied to HTTP MCP endpoints.
// Authentication is intentionally kept at the HTTP boundary so every MCP
// method, including initialization and tool discovery, is protected equally.
type AuthMode string

const (
	// AuthModeNone exposes MCP endpoints without application-level authentication.
	// It is suitable only behind another trusted authentication proxy or on a
	// tightly controlled network.
	AuthModeNone AuthMode = "none"
	// AuthModeToken requires an RFC 6750-style Authorization: Bearer header.
	AuthModeToken AuthMode = "token"
)

// AuthMiddleware returns middleware for the selected authentication mode.
// Configuration validation is responsible for ensuring token is non-empty
// when token authentication is selected.
func AuthMiddleware(mode AuthMode, token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if mode == AuthModeNone {
			return next
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			candidate, ok := bearerToken(r.Header.Get("Authorization"))
			if !ok || !constantTimeEqual(candidate, token) {
				// The response is deliberately generic: callers cannot distinguish a
				// malformed header from an incorrect secret, and the secret is never
				// copied into a log or response body.
				w.Header().Set("WWW-Authenticate", `Bearer realm="mysql-mcp"`)
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken accepts exactly two whitespace-separated fields. This rejects
// ambiguous values such as multiple tokens while treating the scheme name as
// case-insensitive, as required for HTTP authentication schemes.
func bearerToken(header string) (string, bool) {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") || fields[1] == "" {
		return "", false
	}
	return fields[1], true
}

func validBearerTokenValue(value string) bool {
	if value == "" {
		return false
	}
	padding := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '=' {
			padding = true
			continue
		}
		if padding || !(character >= 'A' && character <= 'Z' ||
			character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("-._~+/", rune(character))) {
			return false
		}
	}
	return true
}

func constantTimeEqual(left, right string) bool {
	// ConstantTimeCompare also accounts for content. Its documented early return
	// on unequal lengths is acceptable here because configured token length is not
	// treated as secret; pad both values so even that distinction is avoided.
	maxLen := len(left)
	if len(right) > maxLen {
		maxLen = len(right)
	}
	leftBytes := make([]byte, maxLen)
	rightBytes := make([]byte, maxLen)
	copy(leftBytes, left)
	copy(rightBytes, right)

	lengthEqual := subtle.ConstantTimeEq(int32(len(left)), int32(len(right)))
	contentEqual := subtle.ConstantTimeCompare(leftBytes, rightBytes)
	return lengthEqual&contentEqual == 1
}
