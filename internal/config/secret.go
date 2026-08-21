package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func (c *Config) resolveSecrets(configDirectory string) error {
	if c.Server.Transport == TransportHTTP && c.Server.HTTP.Auth.Mode == AuthModeToken {
		secret, err := resolveSecret(
			c.Server.HTTP.Auth.TokenEnv,
			c.Server.HTTP.Auth.TokenFile,
			configDirectory,
			"server.http.auth token",
		)
		if err != nil {
			return err
		}
		if err := validateBearerTokenValue(secret); err != nil {
			return fmt.Errorf("resolve server.http.auth token: %w", err)
		}
		c.Server.HTTP.Auth.token = secret
	}

	for index := range c.Datasources {
		datasource := &c.Datasources[index]
		if err := resolveCredential(&datasource.Credentials.Read, configDirectory, datasource.Name, "read"); err != nil {
			return err
		}
		if datasource.Credentials.Write.Configured() {
			if err := resolveCredential(&datasource.Credentials.Write, configDirectory, datasource.Name, "write"); err != nil {
				return err
			}
		}
		if datasource.Credentials.Monitor.Configured() {
			if err := resolveCredential(&datasource.Credentials.Monitor, configDirectory, datasource.Name, "monitor"); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateBearerTokenValue applies the RFC 6750 b64token grammar. Validating
// at startup avoids accepting a configured secret that no Authorization header
// can represent (for example one containing whitespace or control bytes).
func validateBearerTokenValue(value string) error {
	if value == "" {
		return errors.New("bearer token cannot be empty")
	}
	padding := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '=' {
			padding = true
			continue
		}
		if padding || !isBearerTokenCharacter(character) {
			return errors.New("bearer token contains characters outside the RFC 6750 b64token grammar")
		}
	}
	return nil
}

func isBearerTokenCharacter(character byte) bool {
	return character >= 'A' && character <= 'Z' ||
		character >= 'a' && character <= 'z' ||
		character >= '0' && character <= '9' ||
		strings.ContainsRune("-._~+/", rune(character))
}

func resolveCredential(credential *Credential, configDirectory, datasourceName, role string) error {
	environmentName := credential.PasswordEnv
	if credential.PasswordValue != "" {
		if referencedEnvironment, valid := passwordReferenceEnvironment(credential.PasswordValue); valid {
			environmentName = referencedEnvironment
		} else {
			// A password is interpreted as an environment reference only when the
			// entire value matches ${ENV_NAME}; every other non-empty scalar is
			// literal and must never be included in an error message.
			credential.password = credential.PasswordValue
			return nil
		}
	}
	secret, err := resolveSecret(
		environmentName,
		credential.PasswordFile,
		configDirectory,
		fmt.Sprintf("datasource %q %s password", datasourceName, role),
	)
	if err != nil {
		return err
	}
	credential.password = secret
	return nil
}

func resolveSecret(environmentName, fileName, configDirectory, label string) (string, error) {
	if environmentName != "" {
		value, found := os.LookupEnv(environmentName)
		if !found {
			return "", fmt.Errorf("resolve %s: environment variable %q is not set", label, environmentName)
		}
		if value == "" {
			return "", fmt.Errorf("resolve %s: environment variable %q is empty", label, environmentName)
		}
		return value, nil
	}

	path := fileName
	if !filepath.IsAbs(path) {
		path = filepath.Join(configDirectory, path)
	}
	value, err := readSecretFile(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s from file %q: %w", label, path, err)
	}
	return value, nil
}

func readSecretFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maximumSecretFileBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maximumSecretFileBytes {
		return "", fmt.Errorf("secret file exceeds %d bytes", maximumSecretFileBytes)
	}
	// Secret files commonly end in one line ending. Remove exactly one rather
	// than TrimSpace, because spaces and additional newlines can be intentional.
	data = bytes.TrimSuffix(data, []byte("\n"))
	data = bytes.TrimSuffix(data, []byte("\r"))
	if len(data) == 0 {
		return "", fmt.Errorf("secret file is empty")
	}
	return string(data), nil
}
