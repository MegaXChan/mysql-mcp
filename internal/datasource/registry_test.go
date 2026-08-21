package datasource

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/MegaXChan/mysql-mcp/internal/config"
)

func TestOpenRegistryBuildsSortedReadOnlySources(t *testing.T) {
	// Two sources deliberately arrive out of order. The test verifies stable
	// endpoint order, per-source parser versions, read-only service wiring, and
	// the absence of write/admin pools under secure defaults.
	dbs := make(map[string]*sql.DB)
	for _, name := range []string{"zeta", "alpha"} {
		db, _, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		dbs[name] = db
	}
	cfg := registryTestConfig("zeta", "alpha")
	registry, err := OpenRegistry(context.Background(), &cfg, RegistryOptions{
		OpenPool: func(_ context.Context, datasource config.DatasourceConfig, _ config.Credential, role Role, _ time.Duration) (*sql.DB, error) {
			if role != RoleRead {
				t.Fatalf("unexpected role %q for read-only config", role)
			}
			return dbs[datasource.Name], nil
		},
		DetectVersion: func(_ context.Context, _ QueryRower) (Version, error) {
			return Version{Raw: "5.7.44", Major: 5, Minor: 7, Patch: 44}, nil
		},
	})
	if err != nil {
		t.Fatalf("OpenRegistry() error = %v", err)
	}
	defer registry.Close()

	if got, want := registry.Names(), []string{"alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for _, name := range registry.Names() {
		source, found := registry.Source(name)
		if !found || source.Policy == nil || source.Services.Query == nil || source.Services.Metadata == nil {
			t.Fatalf("source %q is incompletely initialized: %+v", name, source)
		}
		if !source.ReadOnly || source.Services.Command != nil || source.Services.Admin != nil {
			t.Fatalf("source %q exposes write services in read-only mode", name)
		}
		if source.Policy.MySQLServerVersion() != "5.7.44" {
			t.Errorf("source %q parser version = %q", name, source.Policy.MySQLServerVersion())
		}
	}
	if _, found := registry.Source("missing"); found {
		t.Fatal("Source(missing) unexpectedly fell back to another datasource")
	}
}

func TestOpenRegistrySeparatesReadWriteAndMonitorPools(t *testing.T) {
	// Enabling DML, admin, a write-effect function, and monitoring should open
	// exactly the three least-privilege roles and wire each optional service.
	readDB, _, _ := sqlmock.New()
	writeDB, _, _ := sqlmock.New()
	monitorDB, _, _ := sqlmock.New()
	cfg := registryTestConfig("primary")
	cfg.Server.ReadOnly = false
	cfg.Server.Features = config.FeatureConfig{DML: true, Admin: true, FunctionWrite: true}
	datasourceConfig := &cfg.Datasources[0]
	datasourceConfig.Credentials.Write = config.Credential{Username: "writer", PasswordEnv: "WRITE_PASSWORD"}
	datasourceConfig.Credentials.Monitor = config.Credential{Username: "monitor", PasswordEnv: "MONITOR_PASSWORD"}
	datasourceConfig.Monitoring.Enabled = true
	datasourceConfig.Monitoring.Sessions = true
	datasourceConfig.Functions = []config.FunctionAllow{{Name: "app.rebuild", Effect: config.FunctionEffectWrite}}

	openedRoles := make([]Role, 0, 3)
	registry, err := OpenRegistry(context.Background(), &cfg, RegistryOptions{
		OpenPool: func(_ context.Context, _ config.DatasourceConfig, _ config.Credential, role Role, _ time.Duration) (*sql.DB, error) {
			openedRoles = append(openedRoles, role)
			switch role {
			case RoleRead:
				return readDB, nil
			case RoleWrite:
				return writeDB, nil
			case RoleMonitor:
				return monitorDB, nil
			default:
				return nil, errors.New("unexpected role")
			}
		},
		DetectVersion: func(context.Context, QueryRower) (Version, error) {
			return Version{Raw: "8.4.2", Major: 8, Minor: 4, Patch: 2}, nil
		},
		DetectPerformanceSchema: func(context.Context, QueryRower) (bool, error) { return true, nil },
	})
	if err != nil {
		t.Fatalf("OpenRegistry() error = %v", err)
	}
	defer registry.Close()
	if want := []Role{RoleRead, RoleWrite, RoleMonitor}; !reflect.DeepEqual(openedRoles, want) {
		t.Fatalf("opened roles = %v, want %v", openedRoles, want)
	}
	source, _ := registry.Source("primary")
	if source.ReadOnly || source.Services.Command == nil || source.Services.Admin == nil || source.Services.Monitor == nil || source.Services.Functions == nil {
		t.Fatalf("optional services are incompletely initialized: %+v", source.Services)
	}
}

func TestOpenRegistryClosesPoolAfterDiscoveryFailure(t *testing.T) {
	// Startup is atomic: a server-version discovery error cannot leave the pool
	// from a partially initialized source running in the background.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	cfg := registryTestConfig("primary")
	_, err = OpenRegistry(context.Background(), &cfg, RegistryOptions{
		OpenPool: func(context.Context, config.DatasourceConfig, config.Credential, Role, time.Duration) (*sql.DB, error) {
			return db, nil
		},
		DetectVersion: func(context.Context, QueryRower) (Version, error) {
			return Version{}, errors.New("version query failed")
		},
	})
	if err == nil {
		t.Fatal("OpenRegistry() error = nil, want discovery failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("partial pool was not closed: %v", err)
	}
}

func TestSourceAcquireHonorsConcurrencyAndCancellation(t *testing.T) {
	t.Parallel()

	// With the only slot held, another request must wait and then return the
	// caller's context error. Releasing restores the slot exactly once.
	source := &Source{Name: "primary", limiter: make(chan struct{}, 1)}
	release, err := source.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := source.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Acquire() error = %v, want deadline exceeded", err)
	}
	release()
	release() // The returned closure is idempotent and cannot over-release.
	secondRelease, err := source.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire() after release error = %v", err)
	}
	secondRelease()
}

func TestSourceSchemaAllowedCombinesExactNamesAndPatterns(t *testing.T) {
	t.Parallel()

	// Exact entries and anchored globs form one allow-list. Matching remains
	// case-sensitive so a Linux MySQL host cannot confuse two distinct schema
	// names. Both lists empty retains the documented unrestricted behavior.
	source := &Source{
		AllowedSchemas:        []string{"shared"},
		AllowedSchemaPatterns: []string{"*_dev"},
	}
	tests := []struct {
		name string
		want bool
	}{
		{name: "shared", want: true},
		{name: "orders_dev", want: true},
		{name: "_dev", want: true},
		{name: "orders_prod", want: false},
		{name: "ORDERS_DEV", want: false},
	}
	for _, test := range tests {
		if got := source.SchemaAllowed(test.name); got != test.want {
			t.Errorf("SchemaAllowed(%q) = %v, want %v", test.name, got, test.want)
		}
	}
	if got := (&Source{}).SchemaAllowed("anything"); !got {
		t.Fatal("SchemaAllowed() with no exact names or patterns = false, want unrestricted")
	}
}

func registryTestConfig(names ...string) config.Config {
	cfg := config.Defaults()
	for _, name := range names {
		cfg.Datasources = append(cfg.Datasources, config.DatasourceConfig{
			Name:            name,
			Network:         "tcp",
			Address:         "127.0.0.1:3306",
			DefaultDatabase: "app",
			AllowedSchemas:  []string{"app"},
			Credentials: config.Credentials{
				Read: config.Credential{Username: "reader", PasswordEnv: "READ_PASSWORD"},
			},
			TLS: config.TLS{Mode: "disabled"},
			Pool: config.Pool{
				MaxOpen:         2,
				MaxIdle:         1,
				ConnMaxLifetime: time.Minute,
				ConnMaxIdleTime: time.Minute,
			},
			Monitoring: config.Monitoring{QueryTimeout: time.Second},
		})
	}
	return cfg
}
