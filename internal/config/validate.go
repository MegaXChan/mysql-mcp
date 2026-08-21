package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	datasourceNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	environmentNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Validate checks all structural, safety, and cross-field invariants. Secret
// references are checked here; their values are resolved only by Load.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config validation failed: config is nil")
	}
	if c.Version != 1 {
		return fmt.Errorf("config validation failed: version must be 1, got %d", c.Version)
	}
	if c.Server.Transport != TransportStdio && c.Server.Transport != TransportHTTP {
		return fmt.Errorf("config validation failed: server.transport must be %q or %q", TransportStdio, TransportHTTP)
	}
	if c.Server.ReadOnly && c.Server.Features.AnyWrite() {
		return errors.New("config validation failed: server.read_only=true conflicts with dml, ddl, admin, or function_write")
	}
	if c.Server.Transport == TransportStdio &&
		(c.Server.HTTP.Auth.Mode != AuthModeNone || c.Server.HTTP.Auth.TokenEnv != "" ||
			c.Server.HTTP.Auth.TokenFile != "" || c.Server.HTTP.Auth.token != "") {
		return errors.New("config validation failed: server.http.auth must use mode=none for stdio transport")
	}
	if err := validateHTTP(c.Server.HTTP); err != nil {
		return fmt.Errorf("config validation failed: server.http: %w", err)
	}
	if err := validateLimits(c.Server.Limits); err != nil {
		return fmt.Errorf("config validation failed: server.limits: %w", err)
	}
	if len(c.Datasources) == 0 {
		return errors.New("config validation failed: at least one datasource is required")
	}

	seenDatasources := make(map[string]struct{}, len(c.Datasources))
	for index := range c.Datasources {
		datasource := &c.Datasources[index]
		location := fmt.Sprintf("datasources[%d]", index)
		if err := validateDatasource(datasource, c.Server.Features, c.Server.ReadOnly); err != nil {
			return fmt.Errorf("config validation failed: %s: %w", location, err)
		}
		if _, exists := seenDatasources[datasource.Name]; exists {
			return fmt.Errorf("config validation failed: duplicate datasource name %q", datasource.Name)
		}
		seenDatasources[datasource.Name] = struct{}{}
	}
	return nil
}

// Validate is the package-level counterpart of (*Config).Validate.
func Validate(c *Config) error {
	return c.Validate()
}

func validateHTTP(httpConfig HTTPConfig) error {
	if err := validateHostPort(httpConfig.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	auth := httpConfig.Auth
	switch auth.Mode {
	case AuthModeNone:
		if auth.TokenEnv != "" || auth.TokenFile != "" || auth.token != "" {
			return errors.New("auth token settings require mode=token")
		}
	case AuthModeToken:
		sourceCount := countNonEmpty(auth.TokenEnv, auth.TokenFile)
		if sourceCount != 1 {
			return errors.New("auth mode=token requires exactly one of token_env or token_file")
		}
		if auth.TokenEnv != "" && !environmentNamePattern.MatchString(auth.TokenEnv) {
			return errors.New("token_env is not a valid environment variable name")
		}
	default:
		return fmt.Errorf("auth.mode must be %q or %q", AuthModeNone, AuthModeToken)
	}
	return nil
}

func validateLimits(limits LimitConfig) error {
	if limits.QueryTimeout <= 0 {
		return errors.New("query_timeout must be greater than zero")
	}
	if limits.MaxSQLBytes <= 0 {
		return errors.New("max_sql_bytes must be greater than zero")
	}
	if limits.DefaultRows <= 0 {
		return errors.New("default_rows must be greater than zero")
	}
	if limits.MaxRows <= 0 {
		return errors.New("max_rows must be greater than zero")
	}
	if limits.DefaultRows > limits.MaxRows {
		return errors.New("default_rows cannot exceed max_rows")
	}
	if limits.MaxResultBytes <= 0 {
		return errors.New("max_result_bytes must be greater than zero")
	}
	if limits.MaxConcurrencyPerSource <= 0 {
		return errors.New("max_concurrency_per_source must be greater than zero")
	}
	return nil
}

func validateDatasource(datasource *DatasourceConfig, features FeatureConfig, globalReadOnly bool) error {
	if !datasourceNamePattern.MatchString(datasource.Name) {
		return errors.New("name must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$ for use as one URL segment")
	}
	switch datasource.Network {
	case "tcp":
		if err := validateHostPort(datasource.Address); err != nil {
			return fmt.Errorf("address: %w", err)
		}
	case "unix":
		if datasource.Address == "" {
			return errors.New("address is required for unix network")
		}
	default:
		return errors.New("network must be tcp or unix")
	}

	if datasource.DefaultDatabase != "" {
		if err := validateMySQLIdentifier(datasource.DefaultDatabase); err != nil {
			return fmt.Errorf("default_database: %w", err)
		}
	}
	seenSchemas := make(map[string]struct{}, len(datasource.AllowedSchemas))
	allowedSchemaNames := make(map[string]struct{}, len(datasource.AllowedSchemas))
	for index, schema := range datasource.AllowedSchemas {
		if err := validateMySQLIdentifier(schema); err != nil {
			return fmt.Errorf("allowed_schemas[%d]: %w", index, err)
		}
		// Database-name case sensitivity depends on the MySQL host platform.
		// Runtime authorization therefore compares schema names exactly, and
		// duplicate detection must use the same semantics.
		key := schema
		if _, exists := seenSchemas[key]; exists {
			return fmt.Errorf("allowed_schemas[%d]: duplicate schema %q", index, schema)
		}
		seenSchemas[key] = struct{}{}
		allowedSchemaNames[schema] = struct{}{}
	}

	if err := validateCredential("read", datasource.Credentials.Read, true); err != nil {
		return err
	}
	if err := validateCredential("write", datasource.Credentials.Write, false); err != nil {
		return err
	}
	if err := validateCredential("monitor", datasource.Credentials.Monitor, false); err != nil {
		return err
	}
	effectiveReadOnly := globalReadOnly || datasource.ReadOnly
	if features.AnyWrite() && !effectiveReadOnly && !datasource.Credentials.Write.Configured() {
		return errors.New("write credential is required when a write feature is enabled")
	}

	if err := validateTLS(datasource.TLS); err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	if err := validatePool(datasource.Pool); err != nil {
		return fmt.Errorf("pool: %w", err)
	}
	if err := validateMonitoring(datasource.Monitoring); err != nil {
		return fmt.Errorf("monitoring: %w", err)
	}
	if err := validateFunctions(datasource.Functions, features.FunctionWrite, effectiveReadOnly); err != nil {
		return fmt.Errorf("functions: %w", err)
	}
	if len(allowedSchemaNames) > 0 {
		for index, function := range datasource.Functions {
			schema, _, _ := strings.Cut(function.Name, ".")
			if _, allowed := allowedSchemaNames[schema]; !allowed {
				return fmt.Errorf("functions[%d].name schema %q is outside allowed_schemas", index, schema)
			}
		}
	}
	return nil
}

func validateCredential(role string, credential Credential, required bool) error {
	if !required && !credential.Configured() {
		return nil
	}
	if credential.Username == "" {
		return fmt.Errorf("credentials.%s.username is required", role)
	}
	if countNonEmpty(credential.PasswordEnv, credential.PasswordFile) != 1 {
		return fmt.Errorf("credentials.%s requires exactly one of password_env or password_file", role)
	}
	if credential.PasswordEnv != "" && !environmentNamePattern.MatchString(credential.PasswordEnv) {
		return fmt.Errorf("credentials.%s.password_env is not a valid environment variable name", role)
	}
	return nil
}

func validateTLS(tls TLS) error {
	switch tls.Mode {
	case "disabled", "preferred", "required", "verify-ca", "verify-full":
	default:
		return errors.New("mode must be disabled, preferred, required, verify-ca, or verify-full")
	}
	if (tls.CertFile == "") != (tls.KeyFile == "") {
		return errors.New("cert_file and key_file must be configured together")
	}
	if tls.Mode == "disabled" && (tls.CAFile != "" || tls.CertFile != "" || tls.KeyFile != "" || tls.ServerName != "") {
		return errors.New("TLS files and server_name cannot be set when mode=disabled")
	}
	return nil
}

func validatePool(pool Pool) error {
	if pool.MaxOpen <= 0 {
		return errors.New("max_open must be greater than zero")
	}
	if pool.MaxIdle < 0 {
		return errors.New("max_idle cannot be negative")
	}
	if pool.MaxIdle > pool.MaxOpen {
		return errors.New("max_idle cannot exceed max_open")
	}
	if pool.ConnMaxLifetime < 0 {
		return errors.New("conn_max_lifetime cannot be negative")
	}
	if pool.ConnMaxIdleTime < 0 {
		return errors.New("conn_max_idle_time cannot be negative")
	}
	return nil
}

func validateMonitoring(monitoring Monitoring) error {
	if monitoring.QueryTimeout <= 0 {
		return errors.New("query_timeout must be greater than zero")
	}
	if !monitoring.Enabled && (monitoring.Sessions || monitoring.Locks || monitoring.TopQueries || monitoring.Replication || monitoring.InnoDBStatus) {
		return errors.New("enabled must be true when a monitoring capability is selected")
	}
	return nil
}

func validateFunctions(functions []FunctionAllow, functionWriteEnabled, readOnly bool) error {
	seen := make(map[string]struct{}, len(functions))
	for index, function := range functions {
		parts := strings.Split(function.Name, ".")
		if len(parts) != 2 {
			return fmt.Errorf("[%d].name must be schema-qualified as schema.function", index)
		}
		if err := validateMySQLIdentifier(parts[0]); err != nil {
			return fmt.Errorf("[%d].name schema: %w", index, err)
		}
		if err := validateMySQLIdentifier(parts[1]); err != nil {
			return fmt.Errorf("[%d].name function: %w", index, err)
		}
		if function.Effect != FunctionEffectRead && function.Effect != FunctionEffectWrite {
			return fmt.Errorf("[%d].effect must be read or write", index)
		}
		if function.Effect == FunctionEffectWrite && (!functionWriteEnabled || readOnly) {
			return fmt.Errorf("[%d].effect=write requires function_write=true on a writable datasource", index)
		}
		// Routine names are case-insensitive within one schema, while schema-name
		// case sensitivity depends on the host. Preserve the schema exactly and
		// fold only the routine component.
		key := parts[0] + "\x00" + strings.ToLower(parts[1])
		if _, exists := seen[key]; exists {
			return fmt.Errorf("[%d].name duplicates %q", index, function.Name)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateMySQLIdentifier(value string) error {
	if value == "" {
		return errors.New("cannot be empty")
	}
	if !utf8.ValidString(value) {
		return errors.New("must be valid UTF-8")
	}
	if utf8.RuneCountInString(value) > 64 {
		return errors.New("cannot exceed 64 characters")
	}
	if strings.ContainsRune(value, '\x00') {
		return errors.New("cannot contain NUL")
	}
	return nil
}

func validateHostPort(address string) error {
	if address == "" {
		return errors.New("host:port is required")
	}
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("must be a valid host:port")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	return nil
}

func countNonEmpty(values ...string) int {
	count := 0
	for _, value := range values {
		if value != "" {
			count++
		}
	}
	return count
}

// EffectiveReadOnly reports the policy after combining server-wide and
// data-source settings. A data source can only tighten, never relax, the global
// read-only policy.
func (c *Config) EffectiveReadOnly(datasource DatasourceConfig) bool {
	if c == nil {
		return true
	}
	return c.Server.ReadOnly || datasource.ReadOnly
}

// Warnings returns non-fatal security observations. Listening without
// authentication on a non-loopback interface is permitted because deployments
// may provide authentication at a trusted reverse proxy, but it is surfaced.
func (c *Config) Warnings() []string {
	if c == nil || c.Server.Transport != TransportHTTP || c.Server.HTTP.Auth.Mode != AuthModeNone {
		return nil
	}
	host, _, err := net.SplitHostPort(c.Server.HTTP.Listen)
	if err != nil || isLoopbackHost(host) {
		return nil
	}
	return []string{"HTTP authentication is disabled on a non-loopback listener; ensure a trusted reverse proxy or network policy protects the MCP endpoints"}
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
