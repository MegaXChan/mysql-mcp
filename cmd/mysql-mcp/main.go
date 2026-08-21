package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/MegaXChan/mysql-mcp/internal/app"
)

var version = "development"
var commit = "unknown"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.NewCommand(version, resolveCommit(commit)).Run(ctx, os.Args); err != nil {
		// Errors go to stderr so stdio transport never mixes diagnostics with MCP
		// protocol frames on stdout.
		_, _ = fmt.Fprintln(os.Stderr, "mysql-mcp:", err)
		os.Exit(1)
	}
}

func resolveCommit(injected string) string {
	info, ok := debug.ReadBuildInfo()
	return resolveCommitFromBuildInfo(injected, info, ok)
}

func resolveCommitFromBuildInfo(injected string, info *debug.BuildInfo, ok bool) string {
	const unknown = "unknown"

	if value := strings.TrimSpace(injected); value != "" && value != unknown {
		return value
	}
	if ok && info != nil {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				if revision := strings.TrimSpace(setting.Value); revision != "" {
					return revision
				}
			}
		}
	}
	return unknown
}
