package datasource

import (
	"testing"
	"time"

	"github.com/MegaXChan/mysql-mcp/internal/config"
)

func TestMySQLDriverConfigUsesSafeDefaults(t *testing.T) {
	t.Parallel()

	datasource := config.DatasourceConfig{
		Name:            "primary",
		Network:         "tcp",
		Address:         "db.internal:3306",
		DefaultDatabase: "application",
		TLS:             config.TLS{Mode: "disabled"},
	}
	credential := config.Credential{Username: "reader"}
	got, err := mysqlDriverConfig(datasource, credential, RoleRead, 3*time.Second)
	if err != nil {
		t.Fatalf("mysqlDriverConfig() error = %v", err)
	}

	// These fields are security boundaries, not just performance preferences.
	// In particular, MultiStatements and local-file support must remain disabled
	// even if SQL parsing is accidentally bypassed by a future adapter.
	if got.MultiStatements {
		t.Error("MultiStatements = true, want false")
	}
	if got.InterpolateParams {
		t.Error("InterpolateParams = true, want false")
	}
	if got.AllowAllFiles {
		t.Error("AllowAllFiles = true, want false")
	}
	if !got.ParseTime {
		t.Error("ParseTime = false, want true")
	}
	if got.RejectReadOnly {
		t.Error("read pool RejectReadOnly = true, want false")
	}
	if got.User != "reader" || got.Net != "tcp" || got.Addr != "db.internal:3306" || got.DBName != "application" {
		t.Errorf("connection identity fields were not preserved: %+v", got)
	}
	if got.Params["sql_mode"] != enforcedSQLMode {
		t.Fatalf("sql_mode initializer = %q, want package-owned IGNORE_SPACE expression", got.Params["sql_mode"])
	}
}

func TestMySQLDriverConfigRejectsReadOnlyWriteTarget(t *testing.T) {
	t.Parallel()

	datasource := config.DatasourceConfig{
		Name:    "primary",
		Network: "tcp",
		Address: "db.internal:3306",
		TLS:     config.TLS{Mode: "required"},
	}
	got, err := mysqlDriverConfig(datasource, config.Credential{Username: "writer"}, RoleWrite, time.Second)
	if err != nil {
		t.Fatalf("mysqlDriverConfig() error = %v", err)
	}
	if !got.RejectReadOnly {
		t.Error("write pool RejectReadOnly = false, want true")
	}
	if got.TLS == nil || !got.TLS.InsecureSkipVerify {
		t.Fatal("required TLS should encrypt without certificate verification")
	}
	if got.AllowFallbackToPlaintext {
		t.Error("required TLS allows plaintext fallback")
	}
}

func TestMakeTLSConfigModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		mode           string
		wantTLS        bool
		wantFallback   bool
		wantSkipVerify bool
		wantError      bool
	}{
		{name: "disabled", mode: "disabled"},
		{name: "preferred", mode: "preferred", wantTLS: true, wantFallback: true, wantSkipVerify: true},
		{name: "required", mode: "required", wantTLS: true, wantSkipVerify: true},
		{name: "verify full", mode: "verify-full", wantTLS: true},
		{name: "unknown", mode: "surprise", wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, fallback, err := makeTLSConfig(config.TLS{Mode: test.mode}, "tcp", "localhost:3306")
			if test.wantError {
				if err == nil {
					t.Fatal("makeTLSConfig() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("makeTLSConfig() error = %v", err)
			}
			if (got != nil) != test.wantTLS {
				t.Fatalf("TLS config present = %v, want %v", got != nil, test.wantTLS)
			}
			if fallback != test.wantFallback {
				t.Errorf("plaintext fallback = %v, want %v", fallback, test.wantFallback)
			}
			if got != nil && got.InsecureSkipVerify != test.wantSkipVerify {
				t.Errorf("InsecureSkipVerify = %v, want %v", got.InsecureSkipVerify, test.wantSkipVerify)
			}
		})
	}
}
