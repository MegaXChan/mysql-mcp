// Package config loads and validates mysql-mcp configuration files.
//
// The package deliberately keeps resolved passwords and bearer tokens out of
// YAML and JSON serialization. Callers can obtain effective values through
// Password and Token after Load has resolved the configured secret source.
package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// TransportStdio serves MCP over the process standard streams.
	TransportStdio = "stdio"
	// TransportHTTP serves MCP over Streamable HTTP.
	TransportHTTP = "http"

	// AuthModeNone disables HTTP bearer-token authentication.
	AuthModeNone = "none"
	// AuthModeToken enables HTTP bearer-token authentication.
	AuthModeToken = "token"

	// FunctionEffectRead marks a stored function as permitted in read-only mode.
	FunctionEffectRead = "read"
	// FunctionEffectWrite marks a stored function as capable of changing data.
	FunctionEffectWrite = "write"
)

// Config is the root of config.yaml.
type Config struct {
	Version     int                `yaml:"version" json:"version"`
	Server      ServerConfig       `yaml:"server" json:"server"`
	Datasources []DatasourceConfig `yaml:"datasources" json:"datasources"`
}

// ServerConfig controls the MCP transport and server-wide safety policy.
type ServerConfig struct {
	Transport string        `yaml:"transport" json:"transport"`
	ReadOnly  bool          `yaml:"read_only" json:"read_only"`
	HTTP      HTTPConfig    `yaml:"http" json:"http"`
	Features  FeatureConfig `yaml:"features" json:"features"`
	Limits    LimitConfig   `yaml:"limits" json:"limits"`
}

// HTTPConfig controls the Streamable HTTP listener.
//
// The MCP path is intentionally not configurable here. The server registers
// exactly /{datasource_name}/mcp for each validated data source.
type HTTPConfig struct {
	Listen string     `yaml:"listen" json:"listen"`
	Auth   AuthConfig `yaml:"auth" json:"auth"`
}

// AuthConfig configures HTTP authentication. token is resolved by Load and is
// never serialized. In token mode exactly one of TokenEnv and TokenFile must be
// configured (unless a token is supplied programmatically for tests/embedding).
type AuthConfig struct {
	Mode      string `yaml:"mode" json:"mode"`
	TokenEnv  string `yaml:"token_env,omitempty" json:"token_env,omitempty"`
	TokenFile string `yaml:"token_file,omitempty" json:"token_file,omitempty"`

	token string
}

// Token returns the resolved HTTP bearer token. It is empty when authentication
// is disabled or when a programmatically-created Config has not been resolved.
func (a AuthConfig) Token() string { return a.token }

// String is intentionally redacted so logging an AuthConfig cannot reveal the
// bearer token.
func (a AuthConfig) String() string {
	return fmt.Sprintf("{Mode:%q TokenEnv:%q TokenFile:%q Token:<redacted>}", a.Mode, a.TokenEnv, a.TokenFile)
}

// GoString makes %#v formatting safe as well as ordinary String formatting.
func (a AuthConfig) GoString() string { return a.String() }

// FeatureConfig contains opt-in capabilities that can change server state.
// Every field defaults to false.
type FeatureConfig struct {
	DML           bool `yaml:"dml" json:"dml"`
	DDL           bool `yaml:"ddl" json:"ddl"`
	Admin         bool `yaml:"admin" json:"admin"`
	FunctionWrite bool `yaml:"function_write" json:"function_write"`
}

// AnyWrite reports whether any server-wide state-changing capability is on.
func (f FeatureConfig) AnyWrite() bool {
	return f.DML || f.DDL || f.Admin || f.FunctionWrite
}

// LimitConfig bounds resource use for every data source.
type LimitConfig struct {
	QueryTimeout            time.Duration `yaml:"query_timeout" json:"query_timeout"`
	MaxSQLBytes             ByteSize      `yaml:"max_sql_bytes" json:"max_sql_bytes"`
	DefaultRows             int           `yaml:"default_rows" json:"default_rows"`
	MaxRows                 int           `yaml:"max_rows" json:"max_rows"`
	MaxResultBytes          ByteSize      `yaml:"max_result_bytes" json:"max_result_bytes"`
	MaxConcurrencyPerSource int           `yaml:"max_concurrency_per_source" json:"max_concurrency_per_source"`
}

// DatasourceConfig is one independently-addressable MySQL server. Name is also
// the single URL segment used by /{datasource_name}/mcp.
type DatasourceConfig struct {
	Name                  string          `yaml:"name" json:"name"`
	Network               string          `yaml:"network" json:"network"`
	Address               string          `yaml:"address" json:"address"`
	DefaultDatabase       string          `yaml:"default_database,omitempty" json:"default_database,omitempty"`
	ReadOnly              bool            `yaml:"read_only" json:"read_only"`
	AllowedSchemas        []string        `yaml:"allowed_schemas,omitempty" json:"allowed_schemas,omitempty"`
	AllowedSchemaPatterns []string        `yaml:"allowed_schema_patterns,omitempty" json:"allowed_schema_patterns,omitempty"`
	Credentials           Credentials     `yaml:"credentials" json:"credentials"`
	TLS                   TLS             `yaml:"tls" json:"tls"`
	Pool                  Pool            `yaml:"pool" json:"pool"`
	Monitoring            Monitoring      `yaml:"monitoring" json:"monitoring"`
	Functions             []FunctionAllow `yaml:"functions,omitempty" json:"functions,omitempty"`
}

// Credentials separates least-privilege accounts by responsibility. Read is
// mandatory; Write and Monitor are optional until the corresponding capability
// is enabled. Monitoring can fall back to Read when Monitor is absent.
type Credentials struct {
	Read    Credential `yaml:"read" json:"read"`
	Write   Credential `yaml:"write,omitempty" json:"write,omitempty"`
	Monitor Credential `yaml:"monitor,omitempty" json:"monitor,omitempty"`
}

// Credential configures a password. PasswordValue is populated by the `password`
// field: an exact ${ENV_NAME} value references the environment, while any other
// non-empty value is a literal password. PasswordEnv and PasswordFile remain
// available for backward compatibility.
type Credential struct {
	Username      string `yaml:"username" json:"username"`
	PasswordValue string `yaml:"password,omitempty" json:"password,omitempty"`
	PasswordEnv   string `yaml:"password_env,omitempty" json:"password_env,omitempty"`
	PasswordFile  string `yaml:"password_file,omitempty" json:"password_file,omitempty"`

	password string
}

// Password returns the password resolved by Load.
func (c Credential) Password() string { return c.password }

// Configured reports whether any field for this optional credential was set.
func (c Credential) Configured() bool {
	return c.Username != "" || c.PasswordValue != "" || c.PasswordEnv != "" || c.PasswordFile != "" || c.password != ""
}

// String prevents accidental password disclosure through ordinary logging.
func (c Credential) String() string {
	return fmt.Sprintf("{Username:%q Password:%q PasswordEnv:%q PasswordFile:%q ResolvedPassword:<redacted>}", c.Username, c.safePasswordValue(), c.PasswordEnv, c.PasswordFile)
}

// GoString makes %#v formatting safe as well as ordinary String formatting.
func (c Credential) GoString() string { return c.String() }

// MarshalYAML preserves auditable environment references but masks literal
// passwords. This protects direct serialization as well as Config.String.
func (c Credential) MarshalYAML() (any, error) {
	type credentialAlias Credential
	safe := credentialAlias(c)
	safe.PasswordValue = c.safePasswordValue()
	return safe, nil
}

// MarshalJSON applies the same literal-password masking as MarshalYAML.
func (c Credential) MarshalJSON() ([]byte, error) {
	type credentialAlias Credential
	safe := credentialAlias(c)
	safe.PasswordValue = c.safePasswordValue()
	return json.Marshal(safe)
}

func (c Credential) safePasswordValue() string {
	if c.PasswordValue == "" {
		return ""
	}
	if _, environmentReference := passwordReferenceEnvironment(c.PasswordValue); environmentReference {
		return c.PasswordValue
	}
	return "<redacted>"
}

// TLS configures encryption for a data-source connection.
type TLS struct {
	Mode       string `yaml:"mode" json:"mode"`
	CAFile     string `yaml:"ca_file,omitempty" json:"ca_file,omitempty"`
	CertFile   string `yaml:"cert_file,omitempty" json:"cert_file,omitempty"`
	KeyFile    string `yaml:"key_file,omitempty" json:"key_file,omitempty"`
	ServerName string `yaml:"server_name,omitempty" json:"server_name,omitempty"`
}

// Pool controls database/sql connection reuse.
type Pool struct {
	MaxOpen         int           `yaml:"max_open" json:"max_open"`
	MaxIdle         int           `yaml:"max_idle" json:"max_idle"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime" json:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `yaml:"conn_max_idle_time" json:"conn_max_idle_time"`
}

// Monitoring enables fixed, server-owned monitoring queries. These flags never
// authorize arbitrary SQL and may be disabled when the target lacks privileges.
type Monitoring struct {
	Enabled      bool          `yaml:"enabled" json:"enabled"`
	Sessions     bool          `yaml:"sessions" json:"sessions"`
	Locks        bool          `yaml:"locks" json:"locks"`
	TopQueries   bool          `yaml:"top_queries" json:"top_queries"`
	Replication  bool          `yaml:"replication" json:"replication"`
	InnoDBStatus bool          `yaml:"innodb_status" json:"innodb_status"`
	QueryTimeout time.Duration `yaml:"query_timeout" json:"query_timeout"`
}

// FunctionAllow is an explicit stored-function allow-list entry. Name is a
// schema-qualified routine name, for example app.calculate_discount.
type FunctionAllow struct {
	Name         string `yaml:"name" json:"name"`
	Effect       string `yaml:"effect" json:"effect"`
	AllowDefiner bool   `yaml:"allow_definer" json:"allow_definer"`
}

// ByteSize stores a byte count while accepting convenient YAML values such as
// 65536, "64KiB", "1MiB", "1MB", or "1GiB".
type ByteSize int64

// Bytes returns the plain byte count.
func (s ByteSize) Bytes() int64 { return int64(s) }

// UnmarshalYAML parses an integer byte count or an IEC/SI size string.
func (s *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("byte size must be a scalar")
	}

	if node.Tag == "!!int" {
		var value int64
		if err := node.Decode(&value); err != nil {
			return fmt.Errorf("invalid byte size: %w", err)
		}
		*s = ByteSize(value)
		return nil
	}

	value, err := parseByteSize(node.Value)
	if err != nil {
		return err
	}
	*s = ByteSize(value)
	return nil
}

func parseByteSize(raw string) (int64, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, fmt.Errorf("byte size cannot be empty")
	}

	units := []struct {
		suffix     string
		multiplier int64
	}{
		{"GiB", 1024 * 1024 * 1024},
		{"MiB", 1024 * 1024},
		{"KiB", 1024},
		{"GB", 1000 * 1000 * 1000},
		{"MB", 1000 * 1000},
		{"KB", 1000},
		{"B", 1},
	}

	for _, unit := range units {
		if strings.HasSuffix(value, unit.suffix) {
			number := strings.TrimSpace(strings.TrimSuffix(value, unit.suffix))
			parsed, err := strconv.ParseInt(number, 10, 64)
			if err != nil || parsed < 0 || (parsed > 0 && parsed > int64(^uint64(0)>>1)/unit.multiplier) {
				return 0, fmt.Errorf("invalid byte size %q", raw)
			}
			return parsed * unit.multiplier, nil
		}
	}

	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q", raw)
	}
	return parsed, nil
}
