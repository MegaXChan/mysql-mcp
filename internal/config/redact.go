package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Redacted returns an independent copy with all resolved secrets removed. The
// environment-variable names and file references remain visible because they
// are configuration metadata, not secret values.
func (c Config) Redacted() Config {
	redacted := c
	redacted.Server.HTTP.Auth.token = ""
	redacted.Datasources = make([]DatasourceConfig, len(c.Datasources))
	for index := range c.Datasources {
		redacted.Datasources[index] = c.Datasources[index]
		redacted.Datasources[index].AllowedSchemas = append([]string(nil), c.Datasources[index].AllowedSchemas...)
		redacted.Datasources[index].Functions = append([]FunctionAllow(nil), c.Datasources[index].Functions...)
		redacted.Datasources[index].Credentials.Read.password = ""
		redacted.Datasources[index].Credentials.Write.password = ""
		redacted.Datasources[index].Credentials.Monitor.password = ""
	}
	return redacted
}

// String produces safe YAML suitable for diagnostics. Resolved passwords and
// tokens are never included.
func (c Config) String() string {
	data, err := yaml.Marshal(c.Redacted())
	if err != nil {
		return fmt.Sprintf("<invalid config: %v>", err)
	}
	return string(data)
}

// GoString makes %#v formatting of Config safe too.
func (c Config) GoString() string { return c.String() }
