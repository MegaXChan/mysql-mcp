package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadDefaultsAndEnvironmentSecret covers the normal minimal deployment.
// Risk: an omitted safety field could silently enable writes or unbounded work.
// Expected: secure defaults are retained while the read password is resolved.
func TestLoadDefaultsAndEnvironmentSecret(t *testing.T) {
	t.Setenv("MYSQL_MCP_TEST_PASSWORD", "top-secret-password")
	path := writeTestConfig(t, minimalYAML("MYSQL_MCP_TEST_PASSWORD"))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Server.Transport != TransportStdio || !cfg.Server.ReadOnly {
		t.Fatalf("unsafe defaults: transport=%q read_only=%v", cfg.Server.Transport, cfg.Server.ReadOnly)
	}
	if cfg.Server.Features.AnyWrite() {
		t.Fatal("write features must default to disabled")
	}
	limits := cfg.Server.Limits
	if limits.QueryTimeout != 10*time.Second || limits.MaxSQLBytes.Bytes() != 64*1024 || limits.DefaultRows != 200 || limits.MaxRows != 1000 || limits.MaxResultBytes.Bytes() != 1024*1024 || limits.MaxConcurrencyPerSource != 4 {
		t.Fatalf("unexpected limits: %+v", limits)
	}
	datasource := cfg.Datasources[0]
	if datasource.Network != "tcp" || datasource.TLS.Mode != "disabled" {
		t.Fatalf("unexpected datasource defaults: %+v", datasource)
	}
	if datasource.Pool.MaxOpen != 10 || datasource.Pool.MaxIdle != 5 || datasource.Pool.ConnMaxLifetime != 30*time.Minute || datasource.Pool.ConnMaxIdleTime != 5*time.Minute {
		t.Fatalf("unexpected pool defaults: %+v", datasource.Pool)
	}
	if got := datasource.Credentials.Read.Password(); got != "top-secret-password" {
		t.Fatalf("resolved password mismatch: %q", got)
	}
	if !cfg.EffectiveReadOnly(datasource) {
		t.Fatal("global read-only policy must not be relaxed by a datasource")
	}
}

// TestLoadStrictYAML groups ambiguity and schema tests. Each case represents a
// configuration-review risk; every malformed document must fail before use.
func TestLoadStrictYAML(t *testing.T) {
	t.Setenv("MYSQL_MCP_TEST_PASSWORD", "secret")
	tests := []struct {
		name     string
		yaml     string
		wantText string
	}{
		{
			name: "unknown field",
			// Risk: a misspelled security option appears accepted but has no effect.
			yaml:     minimalYAML("MYSQL_MCP_TEST_PASSWORD") + "unexpected: true\n",
			wantText: "field unexpected not found",
		},
		{
			name: "duplicate nested key",
			// Risk: different YAML implementations choose different policy values.
			yaml: `version: 1
server:
  read_only: true
  read_only: false
datasources: []
`,
			wantText: "duplicate YAML key",
		},
		{
			name: "merge key",
			// Risk: anchors obscure which authentication or write policy wins.
			yaml: `version: 1
defaults: &defaults
  read_only: true
server:
  <<: *defaults
datasources: []
`,
			wantText: "merge key",
		},
		{
			name: "multiple documents",
			// Risk: operators and parsers may select different YAML documents.
			yaml:     minimalYAML("MYSQL_MCP_TEST_PASSWORD") + "---\nversion: 1\n",
			wantText: "multiple YAML documents",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeTestConfig(t, test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("Load() error = %v, want text %q", err, test.wantText)
			}
		})
	}
}

// TestValidateDatasourceNamesAndDuplicates protects HTTP route registration.
// Risk: slashes, percent escapes, or ambiguous names can escape one URL segment.
// Expected: only the documented route-safe grammar and unique names are valid.
func TestValidateDatasourceNamesAndDuplicates(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Config)
		wantText string
	}{
		{name: "slash", mutate: func(c *Config) { c.Datasources[0].Name = "prod/main" }, wantText: "name must match"},
		{name: "dot segment", mutate: func(c *Config) { c.Datasources[0].Name = ".." }, wantText: "name must match"},
		{name: "percent escape", mutate: func(c *Config) { c.Datasources[0].Name = "prod%2Fmain" }, wantText: "name must match"},
		{name: "too long", mutate: func(c *Config) { c.Datasources[0].Name = strings.Repeat("a", 65) }, wantText: "name must match"},
		{name: "duplicate", mutate: func(c *Config) { c.Datasources = append(c.Datasources, c.Datasources[0]) }, wantText: "duplicate datasource"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("Validate() error = %v, want text %q", err, test.wantText)
			}
		})
	}
}

// TestValidateLimitsAndPool uses table-driven mutations to cover every bound.
// Risk: invalid limits can exhaust memory/connections or make requests immortal.
// Expected: non-positive, negative, and internally inconsistent values fail.
func TestValidateLimitsAndPool(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*Config)
		wantText string
	}{
		{name: "query timeout", mutate: func(c *Config) { c.Server.Limits.QueryTimeout = 0 }, wantText: "query_timeout"},
		{name: "SQL bytes", mutate: func(c *Config) { c.Server.Limits.MaxSQLBytes = -1 }, wantText: "max_sql_bytes"},
		{name: "default rows", mutate: func(c *Config) { c.Server.Limits.DefaultRows = 0 }, wantText: "default_rows"},
		{name: "row ordering", mutate: func(c *Config) { c.Server.Limits.DefaultRows = c.Server.Limits.MaxRows + 1 }, wantText: "cannot exceed"},
		{name: "result bytes", mutate: func(c *Config) { c.Server.Limits.MaxResultBytes = 0 }, wantText: "max_result_bytes"},
		{name: "concurrency", mutate: func(c *Config) { c.Server.Limits.MaxConcurrencyPerSource = 0 }, wantText: "max_concurrency"},
		{name: "pool open", mutate: func(c *Config) { c.Datasources[0].Pool.MaxOpen = 0 }, wantText: "max_open"},
		{name: "pool idle negative", mutate: func(c *Config) { c.Datasources[0].Pool.MaxIdle = -1 }, wantText: "max_idle"},
		{name: "pool idle exceeds open", mutate: func(c *Config) { c.Datasources[0].Pool.MaxIdle = c.Datasources[0].Pool.MaxOpen + 1 }, wantText: "cannot exceed"},
		{name: "pool lifetime", mutate: func(c *Config) { c.Datasources[0].Pool.ConnMaxLifetime = -time.Second }, wantText: "conn_max_lifetime"},
		{name: "pool idle time", mutate: func(c *Config) { c.Datasources[0].Pool.ConnMaxIdleTime = -time.Second }, wantText: "conn_max_idle_time"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			test.mutate(&cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("Validate() error = %v, want text %q", err, test.wantText)
			}
		})
	}
}

// TestReadOnlyWriteFeatureConflict verifies the fail-closed policy invariant.
// Risk: registering a write tool while claiming read-only misleads MCP clients.
// Expected: each state-changing feature conflicts with global read-only mode.
func TestReadOnlyWriteFeatureConflict(t *testing.T) {
	tests := []struct {
		name   string
		enable func(*FeatureConfig)
	}{
		{name: "dml", enable: func(f *FeatureConfig) { f.DML = true }},
		{name: "ddl", enable: func(f *FeatureConfig) { f.DDL = true }},
		{name: "admin", enable: func(f *FeatureConfig) { f.Admin = true }},
		{name: "function write", enable: func(f *FeatureConfig) { f.FunctionWrite = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			test.enable(&cfg.Server.Features)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "conflicts") {
				t.Fatalf("Validate() error = %v, want read-only conflict", err)
			}
		})
	}
}

// TestWritableDatasourceRequiresSeparateCredential checks least privilege once
// the operator explicitly disables read-only mode. Risk: write tools could run
// with the broad or wrong account. Expected: a writable data source needs a
// complete write credential; a data-source read-only override still tightens it.
func TestWritableDatasourceRequiresSeparateCredential(t *testing.T) {
	cfg := validConfig()
	cfg.Server.ReadOnly = false
	cfg.Server.Features.DML = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "write credential") {
		t.Fatalf("Validate() error = %v, want missing write credential", err)
	}

	cfg.Datasources[0].Credentials.Write = Credential{
		Username:    "writer",
		PasswordEnv: "MYSQL_MCP_WRITE_PASSWORD",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("complete write credential rejected: %v", err)
	}

	cfg.Datasources[0].Credentials.Write = Credential{}
	cfg.Datasources[0].ReadOnly = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("read-only datasource should not require write credentials: %v", err)
	}
	if !cfg.EffectiveReadOnly(cfg.Datasources[0]) {
		t.Fatal("datasource read_only=true must tighten a writable global policy")
	}
}

// TestCredentialSecretResolution covers both supported secret sources.
// Risk: relative paths may resolve against the process directory, and line
// endings may accidentally become part of a password. Expected: paths are
// relative to config.yaml and one conventional line ending is removed.
func TestCredentialSecretResolution(t *testing.T) {
	t.Run("environment", func(t *testing.T) {
		t.Setenv("MYSQL_MCP_TEST_PASSWORD", "env-password")
		cfg, err := Load(writeTestConfig(t, minimalYAML("MYSQL_MCP_TEST_PASSWORD")))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Datasources[0].Credentials.Read.Password(); got != "env-password" {
			t.Fatalf("Password() = %q", got)
		}
	})

	t.Run("relative file", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, "mysql.password"), []byte("file-password\r\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "config.yaml")
		yaml := strings.Replace(minimalYAML("unused"), "password_env: unused", "password_file: mysql.password", 1)
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Datasources[0].Credentials.Read.Password(); got != "file-password" {
			t.Fatalf("Password() = %q", got)
		}
	})

}

// TestLoadForServeResolvesOnlySelectedStdioSecrets protects multi-source stdio
// availability. Structural policy for every source is still validated, but an
// unused source's missing runtime secret must not block the explicitly selected
// source. Full validation continues to resolve every secret.
func TestLoadForServeResolvesOnlySelectedStdioSecrets(t *testing.T) {
	t.Setenv("MYSQL_MCP_HEALTHY_PASSWORD", "healthy-secret")
	yaml := `version: 1
server:
  transport: stdio
datasources:
  - name: offline
    address: offline.example:3306
    credentials:
      read:
        username: reader
        password_env: MYSQL_MCP_OFFLINE_PASSWORD
  - name: healthy
    address: healthy.example:3306
    credentials:
      read:
        username: reader
        password_env: MYSQL_MCP_HEALTHY_PASSWORD
`
	path := writeTestConfig(t, yaml)

	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "MYSQL_MCP_OFFLINE_PASSWORD") {
		t.Fatalf("Load() error = %v, want full secret resolution failure", err)
	}
	cfg, err := LoadForServe(path, "  healthy\t")
	if err != nil {
		t.Fatalf("LoadForServe() error = %v", err)
	}
	if len(cfg.Datasources) != 1 || cfg.Datasources[0].Name != "healthy" ||
		cfg.Datasources[0].Credentials.Read.Password() != "healthy-secret" {
		t.Fatalf("LoadForServe() selected config = %#v", cfg.Redacted())
	}
	if _, err := LoadForServe(path, "HEALTHY"); err == nil || !strings.Contains(err.Error(), "unknown datasource") {
		t.Fatalf("case-mismatched selection error = %v", err)
	}
	if _, err := LoadForServe(path, ""); err == nil || !strings.Contains(err.Error(), "available: healthy, offline") {
		t.Fatalf("missing selection error = %v", err)
	}
}

// TestLoadForServeRejectsDatasourceFlagForHTTP does so before resolving any
// secret or opening pools, keeping CLI misuse deterministic.
func TestLoadForServeRejectsDatasourceFlagForHTTP(t *testing.T) {
	yaml := `version: 1
server:
  transport: http
  http:
    auth:
      mode: token
      token_env: MYSQL_MCP_UNSET_HTTP_TOKEN
datasources:
  - name: primary
    address: db.example:3306
    credentials:
      read:
        username: reader
        password_env: MYSQL_MCP_UNSET_DATABASE_PASSWORD
`
	_, err := LoadForServe(writeTestConfig(t, yaml), "primary")
	if err == nil || !strings.Contains(err.Error(), "only valid for stdio") {
		t.Fatalf("LoadForServe() error = %v, want transport/flag rejection", err)
	}
}

// TestCredentialSecretFailures enumerates fail-closed secret cases.
// Risk: silently using an empty or ambiguous password can connect as the wrong
// account. Expected: missing, empty, or dual sources are rejected explicitly.
func TestCredentialSecretFailures(t *testing.T) {
	tests := []struct {
		name     string
		yaml     string
		prepare  func(*testing.T)
		wantText string
	}{
		{name: "missing source", yaml: strings.Replace(minimalYAML("unused"), "      password_env: unused\n", "", 1), wantText: "exactly one"},
		{name: "unset environment", yaml: minimalYAML("MYSQL_MCP_DOES_NOT_EXIST"), wantText: "is not set"},
		{name: "empty environment", yaml: minimalYAML("MYSQL_MCP_EMPTY"), prepare: func(t *testing.T) { t.Setenv("MYSQL_MCP_EMPTY", "") }, wantText: "is empty"},
		{name: "dual source", yaml: strings.Replace(minimalYAML("MYSQL_MCP_TEST_PASSWORD"), "        password_env: MYSQL_MCP_TEST_PASSWORD", "        password_env: MYSQL_MCP_TEST_PASSWORD\n        password_file: password.txt", 1), prepare: func(t *testing.T) { t.Setenv("MYSQL_MCP_TEST_PASSWORD", "secret") }, wantText: "exactly one"},
		{name: "missing file", yaml: strings.Replace(minimalYAML("unused"), "password_env: unused", "password_file: missing.password", 1), wantText: "missing.password"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.prepare != nil {
				test.prepare(t)
			}
			_, err := Load(writeTestConfig(t, test.yaml))
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("Load() error = %v, want text %q", err, test.wantText)
			}
		})
	}
}

// TestHTTPTokenAuthentication covers every authentication mode and source.
// Risk: a typo could leave a remotely-bound server unauthenticated. Expected:
// token mode resolves exactly one source; unknown or incomplete modes fail.
func TestHTTPTokenAuthentication(t *testing.T) {
	validTokenYAML := func(auth string) string {
		return "version: 1\nserver:\n  transport: http\n  http:\n    listen: 127.0.0.1:8080\n    auth:\n" + auth + strings.TrimPrefix(minimalYAML("MYSQL_MCP_TEST_PASSWORD"), "version: 1\n")
	}

	t.Run("environment", func(t *testing.T) {
		t.Setenv("MYSQL_MCP_TEST_PASSWORD", "db-secret")
		t.Setenv("MYSQL_MCP_HTTP_TOKEN", "bearer-secret")
		cfg, err := Load(writeTestConfig(t, validTokenYAML("      mode: token\n      token_env: MYSQL_MCP_HTTP_TOKEN\n")))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.HTTP.Auth.Token() != "bearer-secret" {
			t.Fatal("HTTP token was not resolved")
		}
	})

	t.Run("relative file", func(t *testing.T) {
		t.Setenv("MYSQL_MCP_TEST_PASSWORD", "db-secret")
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, "http.token"), []byte("file-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "config.yaml")
		if err := os.WriteFile(path, []byte(validTokenYAML("      mode: token\n      token_file: http.token\n")), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil || cfg.Server.HTTP.Auth.Token() != "file-token" {
			t.Fatalf("Load() error = %v token resolved=%v", err, err == nil && cfg.Server.HTTP.Auth.Token() == "file-token")
		}
	})

	for _, test := range []struct {
		name  string
		value string
	}{
		{name: "leading whitespace", value: " bearer-secret"},
		{name: "embedded whitespace", value: "bearer secret"},
		{name: "extra newline", value: "bearer-secret\n"},
		{name: "padding before data", value: "bearer=secret"},
		{name: "non bearer punctuation", value: "bearer:secret"},
	} {
		t.Run("invalid resolved token "+test.name, func(t *testing.T) {
			t.Setenv("MYSQL_MCP_TEST_PASSWORD", "db-secret")
			t.Setenv("MYSQL_MCP_HTTP_TOKEN", test.value)
			_, err := Load(writeTestConfig(t, validTokenYAML("      mode: token\n      token_env: MYSQL_MCP_HTTP_TOKEN\n")))
			if err == nil || !strings.Contains(err.Error(), "RFC 6750") {
				t.Fatalf("Load() error = %v, want invalid bearer token rejection", err)
			}
		})
	}

	tests := []struct {
		name     string
		auth     string
		wantText string
	}{
		{name: "unknown mode", auth: "      mode: basic\n", wantText: "auth.mode"},
		{name: "missing token", auth: "      mode: token\n", wantText: "exactly one"},
		{name: "dual token", auth: "      mode: token\n      token_env: TOKEN\n      token_file: token.txt\n", wantText: "exactly one"},
		{name: "token with none", auth: "      mode: none\n      token_env: TOKEN\n", wantText: "require mode=token"},
		{name: "unset token environment", auth: "      mode: token\n      token_env: MYSQL_MCP_MISSING_TOKEN\n", wantText: "is not set"},
		{name: "missing token file", auth: "      mode: token\n      token_file: missing.token\n", wantText: "missing.token"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("MYSQL_MCP_TEST_PASSWORD", "db-secret")
			_, err := Load(writeTestConfig(t, validTokenYAML(test.auth)))
			if err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("Load() error = %v, want text %q", err, test.wantText)
			}
		})
	}
}

// TestHTTPWarning distinguishes an accepted reverse-proxy deployment from an
// accidentally exposed listener. Expected: Validate succeeds in both cases,
// while only unauthenticated non-loopback HTTP produces a warning.
func TestHTTPWarning(t *testing.T) {
	cfg := validConfig()
	cfg.Server.Transport = TransportHTTP
	cfg.Server.HTTP.Listen = "0.0.0.0:8080"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() unexpectedly blocked explicit listener: %v", err)
	}
	if len(cfg.Warnings()) != 1 {
		t.Fatalf("Warnings() = %v, want one warning", cfg.Warnings())
	}
	cfg.Server.HTTP.Listen = "127.0.0.1:8080"
	if warnings := cfg.Warnings(); len(warnings) != 0 {
		t.Fatalf("loopback Warnings() = %v", warnings)
	}
}

func TestStdioRejectsUnusedHTTPAuthentication(t *testing.T) {
	t.Parallel()

	// Bearer authentication has no meaning on stdio. Rejecting dormant token
	// settings prevents startup from depending on an unused HTTP secret and
	// avoids a false impression that the local transport is token-protected.
	cfg := validConfig()
	cfg.Server.Transport = TransportStdio
	cfg.Server.HTTP.Auth = AuthConfig{Mode: AuthModeToken, TokenEnv: "MYSQL_MCP_UNUSED_TOKEN"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "mode=none for stdio") {
		t.Fatalf("Validate() error = %v, want unused HTTP auth rejection", err)
	}
}

// TestTLSRelativePathsResolveAgainstConfig verifies deterministic certificate
// lookup. Risk: starting from a different working directory selects other trust
// material. Expected: all non-empty relative TLS paths become absolute.
func TestTLSRelativePathsResolveAgainstConfig(t *testing.T) {
	t.Setenv("MYSQL_MCP_TEST_PASSWORD", "secret")
	directory := t.TempDir()
	yaml := minimalYAML("MYSQL_MCP_TEST_PASSWORD") + `
`
	yaml = strings.Replace(yaml, "    credentials:\n", "    tls:\n      mode: verify-full\n      ca_file: certs/ca.pem\n      cert_file: certs/client.pem\n      key_file: certs/client.key\n      server_name: mysql.example.com\n    credentials:\n", 1)
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	tls := cfg.Datasources[0].TLS
	for field, got := range map[string]string{"ca": tls.CAFile, "cert": tls.CertFile, "key": tls.KeyFile} {
		if !filepath.IsAbs(got) || !strings.HasPrefix(got, directory+string(filepath.Separator)) {
			t.Errorf("%s path = %q, want absolute path under %q", field, got, directory)
		}
	}
}

// TestFunctionsValidation verifies explicit, schema-qualified allow-list rules.
// Risk: ambiguous or duplicate routine names can call a different function.
// Expected: valid read/write entries pass and malformed entries fail.
func TestFunctionsValidation(t *testing.T) {
	cfg := validConfig()
	cfg.Server.ReadOnly = false
	cfg.Server.Features.FunctionWrite = true
	cfg.Datasources[0].Credentials.Write = Credential{Username: "writer", PasswordEnv: "MYSQL_MCP_WRITE_PASSWORD"}
	cfg.Datasources[0].Functions = []FunctionAllow{
		{Name: "app.calculate_discount", Effect: FunctionEffectRead},
		{Name: "app.rebuild_counter", Effect: FunctionEffectWrite, AllowDefiner: true},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid functions rejected: %v", err)
	}

	tests := []struct {
		name      string
		functions []FunctionAllow
		wantText  string
	}{
		{name: "unqualified", functions: []FunctionAllow{{Name: "calculate", Effect: "read"}}, wantText: "schema-qualified"},
		{name: "missing effect", functions: []FunctionAllow{{Name: "app.calculate"}}, wantText: "effect"},
		{name: "invalid effect", functions: []FunctionAllow{{Name: "app.calculate", Effect: "unknown"}}, wantText: "effect"},
		{name: "duplicate routine differing only by case", functions: []FunctionAllow{{Name: "app.calculate", Effect: "read"}, {Name: "app.CALCULATE", Effect: "read"}}, wantText: "duplicates"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Datasources[0].Functions = test.functions
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("Validate() error = %v, want text %q", err, test.wantText)
			}
		})
	}

	// On a case-sensitive MySQL host, App and app may be distinct databases.
	// Duplicate detection preserves that distinction while still folding the
	// routine-name component inside each schema.
	cfg = validConfig()
	cfg.Datasources[0].AllowedSchemas = []string{"app", "APP"}
	cfg.Datasources[0].Functions = []FunctionAllow{
		{Name: "app.calculate", Effect: FunctionEffectRead},
		{Name: "APP.CALCULATE", Effect: FunctionEffectRead},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("functions in case-distinct schemas rejected: %v", err)
	}
}

// TestLoadRequiresExplicitFunctionEffect proves configuration defaulting does
// not silently turn an incompletely reviewed stored function into an allowed
// read function. Each allow-list entry must declare its effect explicitly.
func TestLoadRequiresExplicitFunctionEffect(t *testing.T) {
	t.Setenv("MYSQL_MCP_TEST_PASSWORD", "secret")
	yaml := minimalYAML("MYSQL_MCP_TEST_PASSWORD")
	yaml = strings.Replace(yaml, "    credentials:\n", "    functions:\n      - name: app.calculate\n    credentials:\n", 1)

	_, err := Load(writeTestConfig(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "effect must be read or write") {
		t.Fatalf("Load() error = %v, want explicit function effect rejection", err)
	}
}

// TestAllowedSchemaDuplicateMatchingIsExact keeps configuration validation and
// runtime authorization aligned on hosts where database names are
// case-sensitive. App and app may both be intentionally authorized, while an
// exact duplicate remains a configuration error.
func TestAllowedSchemaDuplicateMatchingIsExact(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.Datasources[0].AllowedSchemas = []string{"app", "APP"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("case-distinct schemas rejected: %v", err)
	}

	cfg.Datasources[0].AllowedSchemas = []string{"app", "app"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate schema") {
		t.Fatalf("Validate() error = %v, want exact duplicate rejection", err)
	}
}

// TestWriteFunctionRequiresExplicitFeature verifies that merely placing a
// state-changing routine in the allow list cannot expose it. The server-wide
// function_write switch, a writable source, and separate write credentials are
// all required before the dedicated call tool may use it.
func TestWriteFunctionRequiresExplicitFeature(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.Datasources[0].Functions = []FunctionAllow{{Name: "app.rebuild_counter", Effect: FunctionEffectWrite}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "function_write") {
		t.Fatalf("Validate() error = %v, want function_write requirement", err)
	}

	cfg.Server.ReadOnly = false
	cfg.Server.Features.FunctionWrite = true
	cfg.Datasources[0].Credentials.Write = Credential{Username: "writer", PasswordEnv: "MYSQL_MCP_WRITE_PASSWORD"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("fully authorized write function rejected: %v", err)
	}

	cfg.Datasources[0].ReadOnly = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "writable datasource") {
		t.Fatalf("Validate() error = %v, want read-only datasource rejection", err)
	}
}

// TestFunctionSchemaMustBeAllowed prevents a dedicated function call from
// becoming an alternate path around the datasource-wide schema allow list.
func TestFunctionSchemaMustBeAllowed(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.Datasources[0].AllowedSchemas = []string{"app"}
	cfg.Datasources[0].Functions = []FunctionAllow{{Name: "secret.lookup", Effect: FunctionEffectRead}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "outside allowed_schemas") {
		t.Fatalf("Validate() error = %v, want function schema rejection", err)
	}

	// Database-name case sensitivity depends on the MySQL host platform. Exact
	// matching avoids authorizing a distinct `APP` database on Unix-like hosts.
	cfg.Datasources[0].Functions = []FunctionAllow{{Name: "APP.lookup", Effect: FunctionEffectRead}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "outside allowed_schemas") {
		t.Fatalf("Validate() error = %v, want case-sensitive schema rejection", err)
	}
}

// TestRedaction proves resolved values cannot leak through the public diagnostic
// methods. Expected: source references remain useful, but neither database nor
// HTTP secret occurs in String, GoString, or nested Stringer output.
func TestRedaction(t *testing.T) {
	const databaseSecret = "database-secret-never-log"
	const tokenSecret = "token-secret-never-log"
	t.Setenv("MYSQL_MCP_TEST_PASSWORD", databaseSecret)
	t.Setenv("MYSQL_MCP_HTTP_TOKEN", tokenSecret)
	yaml := "version: 1\nserver:\n  transport: http\n  http:\n    auth:\n      mode: token\n      token_env: MYSQL_MCP_HTTP_TOKEN\n" + strings.TrimPrefix(minimalYAML("MYSQL_MCP_TEST_PASSWORD"), "version: 1\n")
	cfg, err := Load(writeTestConfig(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	outputs := []string{
		cfg.String(),
		fmt.Sprintf("%#v", *cfg),
		fmt.Sprintf("%+v", cfg.Server.HTTP.Auth),
		fmt.Sprintf("%+v", cfg.Datasources[0].Credentials.Read),
	}
	for index, output := range outputs {
		if strings.Contains(output, databaseSecret) || strings.Contains(output, tokenSecret) {
			t.Fatalf("diagnostic output %d leaked a secret: %s", index, output)
		}
	}
	redacted := cfg.Redacted()
	if redacted.Server.HTTP.Auth.Token() != "" || redacted.Datasources[0].Credentials.Read.Password() != "" {
		t.Fatal("Redacted() retained a resolved secret")
	}
}

// TestHumanReadableByteSizes confirms config.yaml can express operational limits
// without error-prone arithmetic. Expected: IEC and SI forms map exactly.
func TestHumanReadableByteSizes(t *testing.T) {
	t.Setenv("MYSQL_MCP_TEST_PASSWORD", "secret")
	yaml := "version: 1\nserver:\n  limits:\n    max_sql_bytes: 64KiB\n    max_result_bytes: 1MB\n" + strings.TrimPrefix(minimalYAML("MYSQL_MCP_TEST_PASSWORD"), "version: 1\n")
	cfg, err := Load(writeTestConfig(t, yaml))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Limits.MaxSQLBytes.Bytes() != 64*1024 || cfg.Server.Limits.MaxResultBytes.Bytes() != 1_000_000 {
		t.Fatalf("unexpected parsed sizes: %+v", cfg.Server.Limits)
	}
}

func validConfig() Config {
	cfg := Defaults()
	cfg.Datasources = []DatasourceConfig{
		{
			Name:    "primary",
			Network: "tcp",
			Address: "127.0.0.1:3306",
			Credentials: Credentials{
				Read: Credential{Username: "reader", PasswordEnv: "MYSQL_MCP_TEST_PASSWORD"},
			},
			TLS: TLS{Mode: "disabled"},
			Pool: Pool{
				MaxOpen:         10,
				MaxIdle:         5,
				ConnMaxLifetime: 30 * time.Minute,
				ConnMaxIdleTime: 5 * time.Minute,
			},
			Monitoring: Monitoring{QueryTimeout: 5 * time.Second},
		},
	}
	return cfg
}

func minimalYAML(passwordEnvironment string) string {
	return fmt.Sprintf(`version: 1
datasources:
  - name: primary
    address: 127.0.0.1:3306
    credentials:
      read:
        username: reader
        password_env: %s
`, passwordEnvironment)
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
