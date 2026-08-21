package mcpserver

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RunStdio serves one datasource over the process standard streams. Callers
// must direct all logs to stderr because stdout is reserved for MCP frames.
func RunStdio(ctx context.Context, server *mcp.Server) error {
	return server.Run(ctx, &mcp.StdioTransport{})
}

// StreamableHTTPHandler exposes one server using stateless JSON Streamable
// HTTP. The SDK enforces the body limit and request cancellation; Go's origin
// protection rejects unsafe cross-origin browser requests.
func StreamableHTTPHandler(server *mcp.Server, maxRequestBodyBytes int64) http.Handler {
	if maxRequestBodyBytes <= 0 {
		maxRequestBodyBytes = mcp.DefaultMaxRequestBodyBytes
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		MaxRequestBodyBytes:          maxRequestBodyBytes,
		PropagateRequestCancellation: true,
	})
	return http.NewCrossOriginProtection().Handler(handler)
}
