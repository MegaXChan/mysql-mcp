// Package app wires configuration, data sources, MCP transports, and the
// urfave/cli command surface together.
package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/MegaXChan/mysql-mcp/internal/config"
	"github.com/MegaXChan/mysql-mcp/internal/datasource"
	cli "github.com/urfave/cli/v3"
)

// NewCommand builds the mysql-mcp CLI. serve is the default command, while
// validate-config performs strict decoding and secret resolution without
// opening a database connection. The commit is included only in the human-facing
// CLI version; MCP clients continue to receive version as the implementation
// version.
func NewCommand(version, commit string) *cli.Command {
	return &cli.Command{
		Name:           "mysql-mcp",
		Usage:          "policy-controlled MCP access to MySQL 5.7 and 8.x",
		Version:        fmt.Sprintf("%s (commit %s)", version, commit),
		DefaultCommand: "serve",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "log-level",
				Value: "info",
				Usage: "log level: debug, info, warn, or error",
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "serve",
				Usage: "start the configured stdio or Streamable HTTP MCP server",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Required: true, Usage: "path to config.yaml"},
					&cli.StringFlag{Name: "datasource", Usage: "stdio datasource name; required when more than one is configured"},
				},
				Action: serveAction(version),
			},
			{
				Name:  "validate-config",
				Usage: "strictly validate config.yaml and resolve all referenced secrets",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "config", Aliases: []string{"c"}, Required: true, Usage: "path to config.yaml"},
				},
				Action: validateConfigAction,
			},
		},
	}
}

func validateConfigAction(_ context.Context, command *cli.Command) error {
	cfg, err := config.Load(command.String("config"))
	if err != nil {
		return err
	}
	for _, warning := range cfg.Warnings() {
		_, _ = fmt.Fprintln(command.ErrWriter, "warning:", warning)
	}
	_, err = fmt.Fprintf(command.Writer, "configuration valid: %d datasource(s), transport=%s\n", len(cfg.Datasources), cfg.Server.Transport)
	return err
}

func serveAction(version string) cli.ActionFunc {
	return func(ctx context.Context, command *cli.Command) error {
		cfg, err := config.LoadForServe(command.String("config"), command.String("datasource"))
		if err != nil {
			return err
		}
		logger, err := newLogger(command.String("log-level"), command.ErrWriter)
		if err != nil {
			return err
		}
		for _, warning := range cfg.Warnings() {
			logger.Warn(warning)
		}

		registry, err := datasource.OpenRegistry(ctx, cfg, datasource.RegistryOptions{})
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := registry.Close(); closeErr != nil {
				logger.Error("close datasource registry", "error", closeErr)
			}
		}()

		switch cfg.Server.Transport {
		case config.TransportStdio:
			// LoadForServe reduces stdio configuration to the one selected source
			// before resolving secrets or opening pools.
			source, _ := registry.Source(cfg.Datasources[0].Name)
			return serveStdio(ctx, source, version, logger)
		case config.TransportHTTP:
			return serveHTTP(ctx, cfg, registry, version, logger, command.ErrWriter)
		}
		return nil // Transport was exhaustively validated before opening pools.
	}
}

func newLogger(level string, writer io.Writer) (*slog.Logger, error) {
	if writer == nil {
		writer = os.Stderr
	}
	var configured slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		configured = slog.LevelDebug
	case "info", "":
		configured = slog.LevelInfo
	case "warn", "warning":
		configured = slog.LevelWarn
	case "error":
		configured = slog.LevelError
	default:
		return nil, fmt.Errorf("unsupported log level %q", level)
	}
	return slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: configured})), nil
}
