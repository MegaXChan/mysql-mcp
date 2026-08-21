package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/MegaXChan/mysql-mcp/internal/config"
	"github.com/MegaXChan/mysql-mcp/internal/datasource"
	"github.com/MegaXChan/mysql-mcp/internal/mcpserver"
)

const requestIDHeader = "X-Request-ID"

var requestIDSequence atomic.Uint64

func serveStdio(ctx context.Context, source *datasource.Source, version string, logger *slog.Logger) error {
	server, err := mcpserver.NewServer(source, version, logger)
	if err != nil {
		return err
	}
	logger.Info("starting stdio MCP server", "datasource", source.Name)
	err = mcpserver.RunStdio(ctx, server)
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return nil
	}
	return err
}

func serveHTTP(
	ctx context.Context,
	cfg *config.Config,
	registry *datasource.Registry,
	version string,
	logger *slog.Logger,
	errorWriter io.Writer,
) error {
	endpoints := make(map[string]http.Handler, len(registry.Names()))
	for _, name := range registry.Names() {
		source, _ := registry.Source(name)
		server, err := mcpserver.NewServer(source, version, logger)
		if err != nil {
			return err
		}
		bodyLimit := httpRequestBodyLimit(source.MaxSQLBytes)
		endpoints[name] = mcpserver.StreamableHTTPHandler(server, bodyLimit)
		logger.Info("configured HTTP MCP endpoint", "datasource", name, "path", "/"+name+"/mcp")
	}

	handler, err := mcpserver.NewHTTPHandler(endpoints, mcpserver.HTTPOptions{
		AuthMode: mcpserver.AuthMode(cfg.Server.HTTP.Auth.Mode),
		Token:    cfg.Server.HTTP.Auth.Token(),
		Ready:    func() bool { return ctx.Err() == nil },
	})
	if err != nil {
		return err
	}
	if errorWriter == nil {
		errorWriter = io.Discard
	}
	httpServer := newHTTPServer(
		cfg.Server.HTTP.Listen,
		requestLoggingMiddleware(handler, logger),
		maximumExecutionTimeout(cfg),
		errorWriter,
	)

	listenError := make(chan error, 1)
	go func() {
		logger.Info("starting Streamable HTTP MCP server", "listen", cfg.Server.HTTP.Listen)
		listenError <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-listenError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve HTTP on %s: %w", cfg.Server.HTTP.Listen, err)
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		if err := <-listenError; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve HTTP on %s: %w", cfg.Server.HTTP.Listen, err)
		}
		return nil
	}
}

// requestLoggingMiddleware records one bounded access log after each non-probe
// request. GET/HEAD probe paths bypass both logging and request-ID generation so
// frequent orchestrator checks do not add noise; unsupported probe methods keep
// normal 405 observability. Only URL.Path is logged: query strings, headers
// (including Authorization), request bodies, and SQL are never read.
func requestLoggingMiddleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if (request.Method == http.MethodGet || request.Method == http.MethodHead) &&
			(request.URL.Path == "/healthz" || request.URL.Path == "/readyz") {
			next.ServeHTTP(response, request)
			return
		}

		requestID := newRequestID()
		response.Header().Set(requestIDHeader, requestID)
		loggedResponse := &loggingResponseWriter{ResponseWriter: response}
		started := time.Now()

		defer func() {
			panicValue := recover()
			status := loggedResponse.Status()
			if panicValue != nil && !loggedResponse.Committed() {
				status = http.StatusInternalServerError
			}
			logger.Info(
				"HTTP request completed",
				"method", request.Method,
				"path", request.URL.Path,
				"status", status,
				"response_bytes", loggedResponse.BytesWritten(),
				"duration_ms", time.Since(started).Milliseconds(),
				"remote_addr", request.RemoteAddr,
				"request_id", requestID,
			)
			if panicValue != nil {
				panic(panicValue)
			}
		}()

		next.ServeHTTP(loggedResponse, request)
	})
}

// loggingResponseWriter observes the HTTP status and the number of bytes
// accepted by the underlying writer. Unwrap lets http.ResponseController reach
// optional capabilities such as Flush and EnableFullDuplex, which Streamable
// HTTP transports rely on without this wrapper needing to guess interfaces.
type loggingResponseWriter struct {
	http.ResponseWriter
	status       int
	bytesWritten int64
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (w *loggingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// FlushError commits HTTP's implicit 200 through this wrapper before flushing
// the underlying stream. Without this hook ResponseController would unwrap
// directly, leaving the logger unaware that headers were already committed.
func (w *loggingResponseWriter) FlushError() error {
	if !w.Committed() {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Status returns the final observed status, treating a handler that writes no
// header or body as HTTP's implicit 200 response.
func (w *loggingResponseWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// BytesWritten reports bytes successfully accepted by the underlying writer.
func (w *loggingResponseWriter) BytesWritten() int64 { return w.bytesWritten }

// Committed reports whether a final response status has been sent. Informational
// 1xx responses other than 101 do not commit the final response.
func (w *loggingResponseWriter) Committed() bool { return w.status != 0 }

func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 100 && status < 200 && status != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.ResponseWriter.WriteHeader(status)
	w.status = status
}

func (w *loggingResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	written, err := w.ResponseWriter.Write(data)
	w.bytesWritten += int64(written)
	return written, err
}

func newRequestID() string {
	sequence := requestIDSequence.Add(1)
	var randomBytes [16]byte
	if _, err := rand.Read(randomBytes[:]); err == nil {
		// The random component prevents cross-process correlation; the monotonic
		// component guarantees uniqueness within this server process.
		return fmt.Sprintf("%s-%x", hex.EncodeToString(randomBytes[:]), sequence)
	}
	// crypto/rand failure is exceptionally rare. Retain process-local uniqueness
	// so a request never proceeds without the promised server-generated ID.
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), sequence)
}

func maximumExecutionTimeout(cfg *config.Config) time.Duration {
	maximum := cfg.Server.Limits.QueryTimeout
	for _, datasourceConfig := range cfg.Datasources {
		if datasourceConfig.Monitoring.Enabled && datasourceConfig.Monitoring.QueryTimeout > maximum {
			maximum = datasourceConfig.Monitoring.QueryTimeout
		}
	}
	return maximum
}

func newHTTPServer(address string, handler http.Handler, queryTimeout time.Duration, errorWriter io.Writer) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      httpWriteTimeout(queryTimeout),
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          log.New(errorWriter, "http: ", log.LstdFlags),
	}
}

func httpWriteTimeout(queryTimeout time.Duration) time.Duration {
	const responseOverhead = 15 * time.Second
	if queryTimeout > time.Duration(math.MaxInt64)-responseOverhead {
		return time.Duration(math.MaxInt64)
	}
	return queryTimeout + responseOverhead
}

// httpRequestBodyLimit reserves bounded JSON-RPC framing/argument overhead and
// saturates rather than overflowing if a deployment deliberately configures an
// extremely large SQL byte limit.
func httpRequestBodyLimit(maxSQLBytes int64) int64 {
	const framingAndArguments int64 = 1 << 20
	if maxSQLBytes > math.MaxInt64-framingAndArguments {
		return math.MaxInt64
	}
	return maxSQLBytes + framingAndArguments
}
