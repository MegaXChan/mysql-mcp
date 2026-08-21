package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateConfigCommand(t *testing.T) {
	// validate-config must exercise the same strict loader as serve but must not
	// attempt a database connection. A referenced environment secret confirms
	// that deployment failures are caught before startup.
	t.Setenv("MYSQL_MCP_APP_TEST_PASSWORD", "secret")
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	data := []byte(`version: 1
datasources:
  - name: primary
    address: 127.0.0.1:3306
    credentials:
      read:
        username: reader
        password_env: MYSQL_MCP_APP_TEST_PASSWORD
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	command := NewCommand("test", "test-commit")
	command.Writer = &stdout
	command.ErrWriter = &stderr
	if err := command.Run(context.Background(), []string{"mysql-mcp", "validate-config", "--config", path}); err != nil {
		t.Fatalf("validate-config error = %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "configuration valid: 1 datasource") {
		t.Fatalf("stdout = %q, want validation summary", stdout.String())
	}
}

func TestNewCommandReportsVersionAndCommit(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	command := NewCommand("v1.2.3", "0123456789abcdef")
	command.Writer = &stdout
	command.ErrWriter = &stderr
	if err := command.Run(context.Background(), []string{"mysql-mcp", "--version"}); err != nil {
		t.Fatalf("--version error = %v; stderr=%s", err, stderr.String())
	}
	if got, want := stdout.String(), "mysql-mcp version v1.2.3 (commit 0123456789abcdef)\n"; got != want {
		t.Fatalf("--version output = %q, want %q", got, want)
	}
}

func TestNewLoggerRejectsUnknownLevel(t *testing.T) {
	t.Parallel()

	if _, err := newLogger("verbose", &bytes.Buffer{}); err == nil {
		t.Fatal("newLogger() error = nil, want unsupported-level error")
	}
}
