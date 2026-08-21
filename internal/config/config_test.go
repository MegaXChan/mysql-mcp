package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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

// TestCredentialSecretResolution covers all supported secret sources.
// Risk: relative paths may resolve against the process directory, and line
// endings may accidentally become part of a password. Expected: `password`
// distinguishes only an exact ${ENV_NAME} reference from a literal, paths are
// relative to config.yaml, and one conventional line ending is removed.
func TestCredentialSecretResolution(t *testing.T) {
	t.Run("password environment reference", func(t *testing.T) {
		t.Setenv("MYSQL_MCP_PASSWORD_REFERENCE", "reference-password")
		yamlConfig := strings.Replace(
			minimalYAML("unused"),
			"password_env: unused",
			"password: ${MYSQL_MCP_PASSWORD_REFERENCE}",
			1,
		)
		cfg, err := Load(writeTestConfig(t, yamlConfig))
		if err != nil {
			t.Fatal(err)
		}
		credential := cfg.Datasources[0].Credentials.Read
		if got := credential.Password(); got != "reference-password" {
			t.Fatalf("Password() = %q, want resolved reference value", got)
		}
		if credential.PasswordValue != "${MYSQL_MCP_PASSWORD_REFERENCE}" {
			t.Fatalf("PasswordValue = %q, want original environment reference", credential.PasswordValue)
		}
	})

	t.Run("literal password", func(t *testing.T) {
		const literal = "literal-${THIS_IS_NOT_EXPANDED}"
		yamlConfig := strings.Replace(
			minimalYAML("unused"),
			"password_env: unused",
			"password: "+literal,
			1,
		)
		cfg, err := Load(writeTestConfig(t, yamlConfig))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.Datasources[0].Credentials.Read.Password(); got != literal {
			t.Fatalf("Password() = %q, want literal value", got)
		}
	})

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

// TestPasswordNonExactReferencesAreLiteral documents the intentionally narrow
// interpolation rule. Only the complete ${ENV_NAME} grammar is expanded; shell
// defaults, concatenation, shorthand, invalid names, and surrounding whitespace
// are ordinary literal passwords. This prevents surprising partial expansion.
func TestPasswordNonExactReferencesAreLiteral(t *testing.T) {
	t.Setenv("MYSQL_MCP_LOOKALIKE", "must-not-be-expanded")
	literals := []string{
		"plain-text-password",
		"prefix-${MYSQL_MCP_LOOKALIKE}",
		"${MYSQL_MCP_LOOKALIKE}-suffix",
		"${MYSQL_MCP_LOOKALIKE:-fallback}",
		"${}",
		"$MYSQL_MCP_LOOKALIKE",
		"${1INVALID_NAME}",
		" ${MYSQL_MCP_LOOKALIKE}",
		"${MYSQL_MCP_LOOKALIKE} ",
	}
	for index, literal := range literals {
		t.Run(fmt.Sprintf("literal_%d", index), func(t *testing.T) {
			quoted, err := json.Marshal(literal)
			if err != nil {
				t.Fatalf("quote test password: %v", err)
			}
			yamlConfig := strings.Replace(
				minimalYAML("unused"),
				"password_env: unused",
				"password: "+string(quoted),
				1,
			)
			cfg, err := Load(writeTestConfig(t, yamlConfig))
			if err != nil {
				t.Fatalf("Load() rejected literal form %q: %v", literal, err)
			}
			if got := cfg.Datasources[0].Credentials.Read.Password(); got != literal {
				t.Fatalf("Password() = %q, want literal %q", got, literal)
			}
		})
	}
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
        password: ${MYSQL_MCP_OFFLINE_PASSWORD}
  - name: healthy
    address: healthy.example:3306
    credentials:
      read:
        username: reader
        password: ${MYSQL_MCP_HEALTHY_PASSWORD}
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
		{name: "empty password", yaml: strings.Replace(minimalYAML("unused"), "password_env: unused", `password: ""`, 1), wantText: "exactly one"},
		{name: "unset environment", yaml: minimalYAML("MYSQL_MCP_DOES_NOT_EXIST"), wantText: "is not set"},
		{name: "empty environment", yaml: minimalYAML("MYSQL_MCP_EMPTY"), prepare: func(t *testing.T) { t.Setenv("MYSQL_MCP_EMPTY", "") }, wantText: "is empty"},
		{name: "unset password reference", yaml: strings.Replace(minimalYAML("unused"), "password_env: unused", `password: ${MYSQL_MCP_REFERENCE_DOES_NOT_EXIST}`, 1), wantText: "is not set"},
		{name: "empty password reference", yaml: strings.Replace(minimalYAML("unused"), "password_env: unused", `password: ${MYSQL_MCP_EMPTY_REFERENCE}`, 1), prepare: func(t *testing.T) { t.Setenv("MYSQL_MCP_EMPTY_REFERENCE", "") }, wantText: "is empty"},
		{name: "dual source", yaml: strings.Replace(minimalYAML("MYSQL_MCP_TEST_PASSWORD"), "        password_env: MYSQL_MCP_TEST_PASSWORD", "        password_env: MYSQL_MCP_TEST_PASSWORD\n        password_file: password.txt", 1), prepare: func(t *testing.T) { t.Setenv("MYSQL_MCP_TEST_PASSWORD", "secret") }, wantText: "exactly one"},
		{name: "password and password_env", yaml: strings.Replace(minimalYAML("unused"), "        password_env: unused", "        password: literal-conflict-secret\n        password_env: MYSQL_MCP_TEST_PASSWORD", 1), wantText: "exactly one"},
		{name: "password and password_file", yaml: strings.Replace(minimalYAML("unused"), "        password_env: unused", "        password: literal-conflict-secret\n        password_file: password.txt", 1), wantText: "exactly one"},
		{name: "all three sources", yaml: strings.Replace(minimalYAML("unused"), "        password_env: unused", "        password: literal-conflict-secret\n        password_env: MYSQL_MCP_TEST_PASSWORD\n        password_file: password.txt", 1), wantText: "exactly one"},
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
			if strings.Contains(err.Error(), "literal-conflict-secret") {
				t.Fatalf("Load() error leaked a literal password: %v", err)
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

// TestAllowedSchemaPatternsLoadAndSerialize locks down the public configuration
// spelling used by operators and integrations. Risk: a mismatched YAML or JSON
// tag could make a reviewed restriction silently disappear. Expected: the glob
// list round-trips under allowed_schema_patterns in both encodings.
func TestAllowedSchemaPatternsLoadAndSerialize(t *testing.T) {
	t.Setenv("MYSQL_MCP_TEST_PASSWORD", "secret")
	yamlConfig := strings.Replace(
		minimalYAML("MYSQL_MCP_TEST_PASSWORD"),
		"    credentials:\n",
		"    allowed_schema_patterns:\n      - '*_dev'\n    credentials:\n",
		1,
	)

	cfg, err := Load(writeTestConfig(t, yamlConfig))
	if err != nil {
		t.Fatalf("Load() rejected valid allowed_schema_patterns: %v", err)
	}
	patterns := cfg.Datasources[0].AllowedSchemaPatterns
	if len(patterns) != 1 || patterns[0] != "*_dev" {
		t.Fatalf("AllowedSchemaPatterns = %#v, want [\"*_dev\"]", patterns)
	}

	encoded, err := json.Marshal(cfg.Datasources[0])
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !strings.Contains(string(encoded), `"allowed_schema_patterns":["*_dev"]`) {
		t.Fatalf("JSON does not contain allowed_schema_patterns: %s", encoded)
	}
}

// TestAllowedSchemaPatternValidation covers the fail-closed configuration
// boundary. Patterns must contain a wildcard so exact names cannot be placed in
// two fields with subtly different review expectations; matching and duplicate
// checks remain case-sensitive just like MySQL schema authorization.
func TestAllowedSchemaPatternValidation(t *testing.T) {
	t.Parallel()

	validPatterns := [][]string{
		{"*_dev"},
		{"tenant_*_archive", "*_DEV"},
		{"*"}, // A universal pattern is valid only when configured explicitly.
	}
	for _, patterns := range validPatterns {
		cfg := validConfig()
		cfg.Datasources[0].AllowedSchemaPatterns = patterns
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() rejected patterns %#v: %v", patterns, err)
		}
	}

	tests := []struct {
		name     string
		patterns []string
		wantText string
	}{
		{name: "empty", patterns: []string{""}, wantText: "must contain at least one * wildcard"},
		{name: "exact name belongs in exact list", patterns: []string{"app_dev"}, wantText: "use allowed_schemas for exact names"},
		{name: "duplicate", patterns: []string{"*_dev", "*_dev"}, wantText: "duplicate pattern"},
		{name: "NUL", patterns: []string{"*_dev\x00"}, wantText: "allowed_schema_patterns[0]"},
		{name: "invalid UTF-8", patterns: []string{"*\xff"}, wantText: "allowed_schema_patterns[0]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Datasources[0].AllowedSchemaPatterns = test.patterns
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("Validate() error = %v, want text %q", err, test.wantText)
			}
		})
	}

	// Case-distinct patterns authorize different schemas on case-sensitive
	// hosts and therefore must not be treated as duplicates.
	cfg := validConfig()
	cfg.Datasources[0].AllowedSchemaPatterns = []string{"*_dev", "*_DEV"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("case-distinct patterns rejected: %v", err)
	}
}

// TestDefaultDatabaseMustMatchSchemaRestrictions catches a configuration that
// could start successfully but reject every unqualified physical table at
// request time. Exact names and glob patterns use the same case-sensitive
// union as runtime SQL authorization; an empty default remains valid for
// clients that always qualify physical tables explicitly.
func TestDefaultDatabaseMustMatchSchemaRestrictions(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		defaultDB  string
		exact      []string
		patterns   []string
		wantReject bool
	}{
		{name: "exact match", defaultDB: "shared", exact: []string{"shared"}},
		{name: "pattern match", defaultDB: "orders_dev", patterns: []string{"*_dev"}},
		{name: "empty default permits qualified-only clients", patterns: []string{"*_dev"}},
		{name: "unrestricted default", defaultDB: "orders_prod"},
		{name: "outside pattern", defaultDB: "orders_prod", patterns: []string{"*_dev"}, wantReject: true},
		{name: "case mismatch", defaultDB: "ORDERS_DEV", patterns: []string{"*_dev"}, wantReject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Datasources[0].DefaultDatabase = test.defaultDB
			cfg.Datasources[0].AllowedSchemas = test.exact
			cfg.Datasources[0].AllowedSchemaPatterns = test.patterns
			err := cfg.Validate()
			if test.wantReject {
				if err == nil || !strings.Contains(err.Error(), "default_database") {
					t.Fatalf("Validate() error = %v, want default_database rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() rejected valid default database: %v", err)
			}
		})
	}
}

// TestFunctionSchemaMayMatchAllowedPattern ensures the stored-function allow
// list cannot bypass schema restrictions while still supporting fleet-style
// names such as tenant_dev. Exact allowed schemas and patterns are combined as
// a union, and pattern matching remains case-sensitive.
func TestFunctionSchemaMayMatchAllowedPattern(t *testing.T) {
	t.Parallel()

	cfg := validConfig()
	cfg.Datasources[0].AllowedSchemas = []string{"shared"}
	cfg.Datasources[0].AllowedSchemaPatterns = []string{"*_dev"}
	cfg.Datasources[0].Functions = []FunctionAllow{
		{Name: "tenant_dev.lookup", Effect: FunctionEffectRead},
		{Name: "shared.lookup", Effect: FunctionEffectRead},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("functions covered by exact name or pattern rejected: %v", err)
	}

	for _, schema := range []string{"tenant_prod", "tenant_DEV"} {
		cfg.Datasources[0].Functions = []FunctionAllow{{Name: schema + ".lookup", Effect: FunctionEffectRead}}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "outside allowed_schemas and allowed_schema_patterns") {
			t.Fatalf("Validate() for schema %q error = %v, want schema restriction rejection", schema, err)
		}
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
	cfg.Datasources[0].AllowedSchemaPatterns = []string{"*_dev"}
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
	// Redacted is documented as an independent copy. Mutating its pattern list
	// must not alter the live authorization policy retained by the server.
	redacted.Datasources[0].AllowedSchemaPatterns[0] = "*"
	if cfg.Datasources[0].AllowedSchemaPatterns[0] != "*_dev" {
		t.Fatalf("Redacted() shared AllowedSchemaPatterns backing storage with the original: %#v", cfg.Datasources[0].AllowedSchemaPatterns)
	}
}

// TestCredentialPasswordSerialization proves that accepting a literal password
// does not make it observable through any supported diagnostic or serialization
// path. Environment references are safe configuration metadata and remain
// visible, while both literal and resolved values are always masked or omitted.
func TestCredentialPasswordSerialization(t *testing.T) {
	const literalSecret = "literal-password-never-serialize"
	const resolvedEnvironmentSecret = "resolved-environment-password-never-serialize"

	literalCredential := Credential{
		Username:      "reader",
		PasswordValue: literalSecret,
		password:      literalSecret,
	}
	cfg := validConfig()
	cfg.Datasources[0].Credentials.Read = literalCredential

	credentialJSON, err := json.Marshal(literalCredential)
	if err != nil {
		t.Fatalf("json.Marshal(Credential) error = %v", err)
	}
	credentialYAML, err := yaml.Marshal(literalCredential)
	if err != nil {
		t.Fatalf("yaml.Marshal(Credential) error = %v", err)
	}
	configJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal(Config) error = %v", err)
	}
	configYAML, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal(Config) error = %v", err)
	}
	outputs := []string{
		literalCredential.String(),
		fmt.Sprintf("%#v", literalCredential),
		fmt.Sprintf("%+v", cfg.Datasources[0].Credentials),
		fmt.Sprintf("%#v", cfg.Datasources[0].Credentials),
		fmt.Sprintf("%+v", cfg.Datasources[0]),
		fmt.Sprintf("%#v", cfg.Datasources[0]),
		string(credentialJSON),
		string(credentialYAML),
		cfg.String(),
		fmt.Sprintf("%#v", cfg),
		string(configJSON),
		string(configYAML),
	}
	for index, output := range outputs {
		if strings.Contains(output, literalSecret) {
			t.Fatalf("literal diagnostic output %d leaked the password: %s", index, output)
		}
	}

	var encodedCredential map[string]any
	if err := json.Unmarshal(credentialJSON, &encodedCredential); err != nil {
		t.Fatalf("decode serialized credential: %v", err)
	}
	if got := encodedCredential["password"]; got != "<redacted>" {
		t.Fatalf("serialized literal password = %#v, want <redacted>", got)
	}

	redacted := cfg.Redacted()
	redactedCredential := redacted.Datasources[0].Credentials.Read
	if redactedCredential.PasswordValue != "<redacted>" || redactedCredential.Password() != "" {
		t.Fatalf("Redacted() credential = %s, want masked source and empty resolved value", redactedCredential)
	}

	environmentCredential := Credential{
		Username:      "reader",
		PasswordValue: "${MYSQL_MCP_SERIALIZED_REFERENCE}",
		password:      resolvedEnvironmentSecret,
	}
	environmentJSON, err := json.Marshal(environmentCredential)
	if err != nil {
		t.Fatalf("json.Marshal(environment Credential) error = %v", err)
	}
	environmentYAML, err := yaml.Marshal(environmentCredential)
	if err != nil {
		t.Fatalf("yaml.Marshal(environment Credential) error = %v", err)
	}
	for index, output := range []string{
		environmentCredential.String(),
		fmt.Sprintf("%#v", environmentCredential),
		string(environmentJSON),
		string(environmentYAML),
	} {
		if strings.Contains(output, resolvedEnvironmentSecret) {
			t.Fatalf("environment diagnostic output %d leaked the resolved password: %s", index, output)
		}
		if !strings.Contains(output, "${MYSQL_MCP_SERIALIZED_REFERENCE}") {
			t.Fatalf("environment diagnostic output %d omitted the safe reference: %s", index, output)
		}
	}
	cfg.Datasources[0].Credentials.Read = environmentCredential
	redactedEnvironmentCredential := cfg.Redacted().Datasources[0].Credentials.Read
	if redactedEnvironmentCredential.PasswordValue != "${MYSQL_MCP_SERIALIZED_REFERENCE}" || redactedEnvironmentCredential.Password() != "" {
		t.Fatalf("Redacted() environment credential = %s, want preserved reference and empty resolved value", redactedEnvironmentCredential)
	}
}

// TestCredentialPasswordJSONField verifies the public JSON API independently
// of YAML loading. Expected: `password` decodes into PasswordValue; exact
// environment references serialize unchanged, while literals serialize masked.
func TestCredentialPasswordJSONField(t *testing.T) {
	t.Parallel()

	var reference Credential
	if err := json.Unmarshal([]byte(`{"username":"reader","password":"${MYSQL_MCP_JSON_PASSWORD}"}`), &reference); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if reference.PasswordValue != "${MYSQL_MCP_JSON_PASSWORD}" || !reference.Configured() {
		t.Fatalf("decoded credential = %s", reference)
	}
	referenceJSON, err := json.Marshal(reference)
	if err != nil {
		t.Fatalf("json.Marshal(reference) error = %v", err)
	}
	if !strings.Contains(string(referenceJSON), `"password":"${MYSQL_MCP_JSON_PASSWORD}"`) {
		t.Fatalf("environment reference did not round-trip: %s", referenceJSON)
	}

	var literal Credential
	if err := json.Unmarshal([]byte(`{"username":"reader","password":"json-literal-secret"}`), &literal); err != nil {
		t.Fatalf("json.Unmarshal(literal) error = %v", err)
	}
	literalJSON, err := json.Marshal(literal)
	if err != nil {
		t.Fatalf("json.Marshal(literal) error = %v", err)
	}
	if strings.Contains(string(literalJSON), "json-literal-secret") {
		t.Fatalf("JSON serialization leaked literal password: %s", literalJSON)
	}
}

// TestOptionalCredentialConfigurationWithPassword covers Configured's role in
// deciding whether optional write/monitor credentials are resolved. A password
// value is configuration, while the completely zero credential remains absent.
func TestOptionalCredentialConfigurationWithPassword(t *testing.T) {
	t.Parallel()

	if (Credential{}).Configured() {
		t.Fatal("zero Credential must remain unconfigured")
	}
	if !(Credential{PasswordValue: "literal"}).Configured() {
		t.Fatal("password field must mark an optional Credential as configured")
	}
	if err := validateCredential("monitor", Credential{}, false); err != nil {
		t.Fatalf("empty optional credential rejected: %v", err)
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
