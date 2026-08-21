package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultHTTPListen      = "127.0.0.1:8080"
	defaultMaxSQLBytes     = 64 * 1024
	defaultMaxResultBytes  = 1024 * 1024
	maximumSecretFileBytes = 64 * 1024
)

// Defaults returns a new configuration populated with secure server defaults.
// Data sources are intentionally absent because connection details and secrets
// can never be inferred safely.
func Defaults() Config {
	return Config{
		Version: 1,
		Server: ServerConfig{
			Transport: TransportStdio,
			ReadOnly:  true,
			HTTP: HTTPConfig{
				Listen: defaultHTTPListen,
				Auth:   AuthConfig{Mode: AuthModeNone},
			},
			Features: FeatureConfig{},
			Limits: LimitConfig{
				QueryTimeout:            10 * time.Second,
				MaxSQLBytes:             ByteSize(defaultMaxSQLBytes),
				DefaultRows:             200,
				MaxRows:                 1000,
				MaxResultBytes:          ByteSize(defaultMaxResultBytes),
				MaxConcurrencyPerSource: 4,
			},
		},
	}
}

// Load strictly decodes one YAML document, applies defaults, validates it, and
// resolves referenced secrets. Unknown fields, duplicate mapping keys, and
// additional YAML documents are rejected.
func Load(path string) (*Config, error) {
	cfg, configDirectory, err := loadUnresolved(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.resolveSecrets(configDirectory); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadForServe strictly loads a runtime configuration while applying transport
// selection before secret resolution. HTTP resolves every datasource because
// every endpoint starts atomically. stdio resolves only its selected source, so
// an unrelated source's unavailable secret cannot block startup.
func LoadForServe(path, requestedDatasource string) (*Config, error) {
	cfg, configDirectory, err := loadUnresolved(path)
	if err != nil {
		return nil, err
	}
	requestedDatasource = strings.TrimSpace(requestedDatasource)
	switch cfg.Server.Transport {
	case TransportHTTP:
		if requestedDatasource != "" {
			return nil, errors.New("--datasource is only valid for stdio transport; HTTP exposes every source at /{datasource_name}/mcp")
		}
	case TransportStdio:
		selected, selectErr := selectStdioDatasource(cfg.Datasources, requestedDatasource)
		if selectErr != nil {
			return nil, selectErr
		}
		cfg.Datasources = []DatasourceConfig{selected}
	default:
		// loadUnresolved validates transport; retain a defensive branch for
		// programmatic changes between validation and this switch.
		return nil, fmt.Errorf("unsupported transport %q", cfg.Server.Transport)
	}
	if err := cfg.resolveSecrets(configDirectory); err != nil {
		return nil, err
	}
	return cfg, nil
}

func loadUnresolved(path string) (*Config, string, error) {
	if path == "" {
		return nil, "", errors.New("config path cannot be empty")
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve config path: %w", err)
	}
	data, err := os.ReadFile(absolutePath)
	if err != nil {
		return nil, "", fmt.Errorf("read config file %q: %w", absolutePath, err)
	}
	if err := rejectDuplicateYAMLKeys(data); err != nil {
		return nil, "", fmt.Errorf("decode config: %w", err)
	}

	cfg := Defaults()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, "", fmt.Errorf("decode config: %w", err)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, "", fmt.Errorf("decode config: %w", err)
		}
		return nil, "", errors.New("decode config: multiple YAML documents are not supported")
	}

	configDirectory := filepath.Dir(absolutePath)
	cfg.applyDatasourceDefaults()
	cfg.resolveTLSPaths(configDirectory)
	if err := cfg.Validate(); err != nil {
		return nil, "", err
	}
	return &cfg, configDirectory, nil
}

func selectStdioDatasource(datasources []DatasourceConfig, requested string) (DatasourceConfig, error) {
	if requested != "" {
		for _, datasourceConfig := range datasources {
			if datasourceConfig.Name == requested {
				return datasourceConfig, nil
			}
		}
		return DatasourceConfig{}, fmt.Errorf("unknown datasource %q", requested)
	}
	if len(datasources) == 1 {
		return datasources[0], nil
	}
	names := make([]string, 0, len(datasources))
	for _, datasourceConfig := range datasources {
		names = append(names, datasourceConfig.Name)
	}
	sort.Strings(names)
	return DatasourceConfig{}, fmt.Errorf("--datasource is required for stdio when multiple datasources are configured (available: %s)", strings.Join(names, ", "))
}

func (c *Config) resolveTLSPaths(configDirectory string) {
	for index := range c.Datasources {
		tls := &c.Datasources[index].TLS
		tls.CAFile = absoluteConfigPath(configDirectory, tls.CAFile)
		tls.CertFile = absoluteConfigPath(configDirectory, tls.CertFile)
		tls.KeyFile = absoluteConfigPath(configDirectory, tls.KeyFile)
	}
}

func absoluteConfigPath(configDirectory, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(configDirectory, path))
}

func (c *Config) applyDatasourceDefaults() {
	for index := range c.Datasources {
		datasource := &c.Datasources[index]
		if datasource.Network == "" {
			datasource.Network = "tcp"
		}
		if datasource.TLS.Mode == "" {
			datasource.TLS.Mode = "disabled"
		}
		if datasource.Pool.MaxOpen == 0 {
			datasource.Pool.MaxOpen = 10
		}
		if datasource.Pool.MaxIdle == 0 {
			datasource.Pool.MaxIdle = 5
		}
		if datasource.Pool.ConnMaxLifetime == 0 {
			datasource.Pool.ConnMaxLifetime = 30 * time.Minute
		}
		if datasource.Pool.ConnMaxIdleTime == 0 {
			datasource.Pool.ConnMaxIdleTime = 5 * time.Minute
		}
		if datasource.Monitoring.QueryTimeout == 0 {
			datasource.Monitoring.QueryTimeout = 5 * time.Second
		}
	}
}

// rejectDuplicateYAMLKeys rejects ambiguous mappings before decoding into Go
// structs. YAML merge keys are rejected as well because they obscure the final
// policy value and make security review unnecessarily difficult.
func rejectDuplicateYAMLKeys(data []byte) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	if len(document.Content) == 0 {
		return errors.New("configuration is empty")
	}
	return inspectYAMLNode(document.Content[0], "")
}

func inspectYAMLNode(node *yaml.Node, path string) error {
	switch node.Kind {
	case yaml.MappingNode:
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			keyNode := node.Content[index]
			valueNode := node.Content[index+1]
			if keyNode.Kind != yaml.ScalarNode {
				return errors.New("configuration mapping keys must be strings")
			}
			key := keyNode.Value
			keyPath := key
			if path != "" {
				keyPath = path + "." + key
			}
			if key == "<<" {
				return fmt.Errorf("YAML merge key %q is not supported", keyPath)
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate YAML key %q", keyPath)
			}
			seen[key] = struct{}{}
			if err := inspectYAMLNode(valueNode, keyPath); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			childPath := fmt.Sprintf("%s[%d]", path, index)
			if err := inspectYAMLNode(child, childPath); err != nil {
				return err
			}
		}
	}
	return nil
}
