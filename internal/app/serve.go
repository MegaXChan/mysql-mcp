package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/MegaXChan/mysql-mcp/internal/config"
	"github.com/MegaXChan/mysql-mcp/internal/datasource"
	"github.com/MegaXChan/mysql-mcp/internal/mcpserver"
)

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
	httpServer := newHTTPServer(cfg.Server.HTTP.Listen, handler, maximumExecutionTimeout(cfg), errorWriter)

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
