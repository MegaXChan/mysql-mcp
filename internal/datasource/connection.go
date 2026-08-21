// Package datasource opens and owns the independent connection pools for each
// configured MySQL data source.
package datasource

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/MegaXChan/mysql-mcp/internal/config"
	"github.com/go-sql-driver/mysql"
)

// Role identifies why a connection pool exists. Keeping roles explicit makes
// it hard to accidentally run a write operation with the read account.
type Role string

const (
	RoleRead    Role = "read"
	RoleWrite   Role = "write"
	RoleMonitor Role = "monitor"
)

// enforcedSQLMode aligns every physical connection with the lexical semantics
// used by the Vitess parser. A fixed allow-list is intentional: preserving a
// composite mode such as ANSI/DB2 could silently re-enable ANSI_QUOTES or
// PIPES_AS_CONCAT after SET even if those expanded names were removed first.
// IGNORE_SPACE makes native function names unambiguous before "(". The other
// modes provide conservative, predictable DML behavior without changing SQL
// tokenization. This package-owned literal never uses configuration input.
const enforcedSQLMode = "'ONLY_FULL_GROUP_BY,STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION,IGNORE_SPACE'"

// PoolOpener creates and verifies one role-specific pool. It is injectable so
// registry behavior can be tested without a live MySQL server.
type PoolOpener func(context.Context, config.DatasourceConfig, config.Credential, Role, time.Duration) (*sql.DB, error)

// OpenMySQLPool creates a database/sql pool using go-sql-driver/mysql. Driver
// safety switches are set explicitly: local files, statement interpolation,
// and multi-statements are always disabled.
func OpenMySQLPool(
	ctx context.Context,
	datasource config.DatasourceConfig,
	credential config.Credential,
	role Role,
	timeout time.Duration,
) (*sql.DB, error) {
	if ctx == nil {
		return nil, fmt.Errorf("open %s pool for %q: nil context", role, datasource.Name)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("open %s pool for %q: timeout must be positive", role, datasource.Name)
	}

	driverConfig, err := mysqlDriverConfig(datasource, credential, role, timeout)
	if err != nil {
		return nil, fmt.Errorf("configure %s pool for %q: %w", role, datasource.Name, err)
	}
	connector, err := mysql.NewConnector(driverConfig)
	if err != nil {
		return nil, fmt.Errorf("create %s connector for %q: %w", role, datasource.Name, err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(datasource.Pool.MaxOpen)
	db.SetMaxIdleConns(datasource.Pool.MaxIdle)
	db.SetConnMaxLifetime(datasource.Pool.ConnMaxLifetime)
	db.SetConnMaxIdleTime(datasource.Pool.ConnMaxIdleTime)

	pingContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := db.PingContext(pingContext); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping %s pool for %q: %w", role, datasource.Name, err)
	}
	return db, nil
}

func mysqlDriverConfig(
	datasource config.DatasourceConfig,
	credential config.Credential,
	role Role,
	timeout time.Duration,
) (*mysql.Config, error) {
	tlsConfig, allowPlaintextFallback, err := makeTLSConfig(datasource.TLS, datasource.Network, datasource.Address)
	if err != nil {
		return nil, err
	}

	driverConfig := mysql.NewConfig()
	driverConfig.User = credential.Username
	driverConfig.Passwd = credential.Password()
	driverConfig.Net = datasource.Network
	driverConfig.Addr = datasource.Address
	driverConfig.DBName = datasource.DefaultDatabase
	driverConfig.TLS = tlsConfig
	driverConfig.AllowFallbackToPlaintext = allowPlaintextFallback
	driverConfig.Timeout = timeout
	driverConfig.ReadTimeout = timeout
	driverConfig.WriteTimeout = timeout
	driverConfig.ParseTime = true
	driverConfig.Loc = time.UTC
	driverConfig.MultiStatements = false
	driverConfig.InterpolateParams = false
	driverConfig.AllowAllFiles = false
	driverConfig.AllowCleartextPasswords = false
	driverConfig.AllowOldPasswords = false
	driverConfig.RejectReadOnly = role == RoleWrite
	driverConfig.ConnectionAttributes = "program:mysql-mcp,role:" + string(role)
	driverConfig.Params = map[string]string{"sql_mode": enforcedSQLMode}
	return driverConfig, nil
}

func makeTLSConfig(tlsSettings config.TLS, network, address string) (*tls.Config, bool, error) {
	if tlsSettings.Mode == "disabled" {
		return nil, false, nil
	}

	serverName := tlsSettings.ServerName
	if serverName == "" && network == "tcp" {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, false, fmt.Errorf("derive TLS server name: %w", err)
		}
		serverName = host
	}

	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: serverName,
	}
	if tlsSettings.CertFile != "" {
		certificate, err := tls.LoadX509KeyPair(tlsSettings.CertFile, tlsSettings.KeyFile)
		if err != nil {
			return nil, false, fmt.Errorf("load client TLS certificate: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}

	switch tlsSettings.Mode {
	case "preferred":
		// Preferred and required encrypt the connection but deliberately do not
		// authenticate the server. Deployments needing identity verification use
		// verify-ca or, preferably, verify-full.
		tlsConfig.InsecureSkipVerify = true //nolint:gosec -- explicit documented mode semantics
		return tlsConfig, true, nil
	case "required":
		tlsConfig.InsecureSkipVerify = true //nolint:gosec -- explicit documented mode semantics
		return tlsConfig, false, nil
	case "verify-ca", "verify-full":
		roots, err := loadRootCAs(tlsSettings.CAFile)
		if err != nil {
			return nil, false, err
		}
		tlsConfig.RootCAs = roots
		if tlsSettings.Mode == "verify-full" {
			if serverName == "" {
				return nil, false, fmt.Errorf("verify-full requires server_name when it cannot be derived from a TCP address")
			}
			return tlsConfig, false, nil
		}

		// Go's standard verifier couples chain and hostname verification. In
		// verify-ca mode we intentionally verify the chain without a DNS name.
		tlsConfig.InsecureSkipVerify = true //nolint:gosec -- VerifyConnection performs CA validation below
		tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("MySQL server did not present a certificate")
			}
			intermediates := x509.NewCertPool()
			for _, certificate := range state.PeerCertificates[1:] {
				intermediates.AddCert(certificate)
			}
			_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         roots,
				Intermediates: intermediates,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			return err
		}
		return tlsConfig, false, nil
	default:
		return nil, false, fmt.Errorf("unsupported TLS mode %q", tlsSettings.Mode)
	}
}

func loadRootCAs(caFile string) (*x509.CertPool, error) {
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if caFile == "" {
		return roots, nil
	}
	pemData, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	if !roots.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("CA file does not contain a valid PEM certificate")
	}
	return roots, nil
}
