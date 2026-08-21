package datasource

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/MegaXChan/mysql-mcp/internal/config"
	"github.com/MegaXChan/mysql-mcp/internal/database"
	"github.com/MegaXChan/mysql-mcp/internal/policy"
	"github.com/MegaXChan/mysql-mcp/internal/schemafilter"
)

const performanceSchemaQuery = "SELECT @@performance_schema"

// Services contains the operations bound to one data source. Optional
// capabilities remain nil and are consequently not registered as MCP tools.
type Services struct {
	Query     *database.QueryExecutor
	Command   *database.CommandExecutor
	Metadata  *database.MetadataService
	Monitor   *database.MonitorService
	Functions *database.FunctionService
	Admin     *database.AdminService
}

// Source is a fully initialized data source. It owns separate pools for read,
// write, and monitoring credentials where configured.
type Source struct {
	Name                  string
	DefaultDatabase       string
	AllowedSchemas        []string
	AllowedSchemaPatterns []string
	ReadOnly              bool
	Features              config.FeatureConfig
	Monitoring            config.Monitoring
	Version               Version
	Policy                *policy.Policy
	Services              Services
	DefaultRows           int
	MaxRows               int
	MaxSQLBytes           int64
	FunctionCount         int
	HasWriteFunction      bool

	readDB    *sql.DB
	writeDB   *sql.DB
	monitorDB *sql.DB
	limiter   chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// Acquire reserves one of the configured per-source execution slots. Every
// MCP handler calls it before touching MySQL so one noisy client cannot consume
// the entire database/sql pool.
func (s *Source) Acquire(ctx context.Context) (func(), error) {
	if ctx == nil {
		return nil, fmt.Errorf("acquire data source %q: nil context", s.Name)
	}
	select {
	case s.limiter <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.limiter }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SchemaAllowed checks exact names and anchored glob patterns. MySQL database
// name case sensitivity varies by host platform, so matching remains
// case-sensitive to avoid authorizing a distinct schema. Both lists being
// empty intentionally means all schemas visible to the role account.
func (s *Source) SchemaAllowed(schema string) bool {
	return schemafilter.Allows(schema, s.AllowedSchemas, s.AllowedSchemaPatterns)
}

// Close releases every distinct pool owned by this source.
func (s *Source) Close() error {
	s.closeOnce.Do(func() {
		seen := make(map[*sql.DB]struct{}, 3)
		for _, db := range []*sql.DB{s.monitorDB, s.writeDB, s.readDB} {
			if db == nil {
				continue
			}
			if _, exists := seen[db]; exists {
				continue
			}
			seen[db] = struct{}{}
			s.closeErr = errors.Join(s.closeErr, db.Close())
		}
	})
	return s.closeErr
}

// Registry stores initialized sources by their URL-safe configured names.
type Registry struct {
	sources map[string]*Source
	names   []string
}

// RegistryOptions contains test seams for connection and server discovery.
// Zero values select the production implementations.
type RegistryOptions struct {
	OpenPool                PoolOpener
	DetectVersion           func(context.Context, QueryRower) (Version, error)
	DetectPerformanceSchema func(context.Context, QueryRower) (bool, error)
}

// OpenRegistry initializes all configured data sources atomically. If any
// source fails, every pool opened earlier in the attempt is closed.
func OpenRegistry(ctx context.Context, cfg *config.Config, options RegistryOptions) (_ *Registry, err error) {
	if ctx == nil {
		return nil, fmt.Errorf("open datasource registry: nil context")
	}
	if cfg == nil {
		return nil, fmt.Errorf("open datasource registry: nil config")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if options.OpenPool == nil {
		options.OpenPool = OpenMySQLPool
	}
	if options.DetectVersion == nil {
		options.DetectVersion = DetectVersion
	}
	if options.DetectPerformanceSchema == nil {
		options.DetectPerformanceSchema = DetectPerformanceSchema
	}

	registry := &Registry{
		sources: make(map[string]*Source, len(cfg.Datasources)),
		names:   make([]string, 0, len(cfg.Datasources)),
	}
	defer func() {
		if err != nil {
			_ = registry.Close()
		}
	}()

	for _, datasourceConfig := range cfg.Datasources {
		source, openErr := openSource(ctx, cfg, datasourceConfig, options)
		if openErr != nil {
			return nil, fmt.Errorf("initialize datasource %q: %w", datasourceConfig.Name, openErr)
		}
		registry.sources[source.Name] = source
		registry.names = append(registry.names, source.Name)
	}
	sort.Strings(registry.names)
	return registry, nil
}

func openSource(
	ctx context.Context,
	cfg *config.Config,
	datasourceConfig config.DatasourceConfig,
	options RegistryOptions,
) (_ *Source, err error) {
	source := &Source{
		Name:                  datasourceConfig.Name,
		DefaultDatabase:       datasourceConfig.DefaultDatabase,
		AllowedSchemas:        append([]string(nil), datasourceConfig.AllowedSchemas...),
		AllowedSchemaPatterns: append([]string(nil), datasourceConfig.AllowedSchemaPatterns...),
		ReadOnly:              cfg.EffectiveReadOnly(datasourceConfig),
		Features:              cfg.Server.Features,
		Monitoring:            datasourceConfig.Monitoring,
		DefaultRows:           cfg.Server.Limits.DefaultRows,
		MaxRows:               cfg.Server.Limits.MaxRows,
		MaxSQLBytes:           cfg.Server.Limits.MaxSQLBytes.Bytes(),
		FunctionCount:         len(datasourceConfig.Functions),
		limiter:               make(chan struct{}, cfg.Server.Limits.MaxConcurrencyPerSource),
	}
	defer func() {
		if err != nil {
			_ = source.Close()
		}
	}()

	source.readDB, err = options.OpenPool(
		ctx, datasourceConfig, datasourceConfig.Credentials.Read, RoleRead, cfg.Server.Limits.QueryTimeout,
	)
	if err != nil {
		return nil, err
	}
	source.Version, err = options.DetectVersion(ctx, source.readDB)
	if err != nil {
		return nil, err
	}
	source.Policy, err = policy.New(source.Version.ParserVersion())
	if err != nil {
		return nil, err
	}

	limits := database.Limits{
		QueryTimeout:   cfg.Server.Limits.QueryTimeout,
		MaxRows:        cfg.Server.Limits.MaxRows,
		MaxResultBytes: cfg.Server.Limits.MaxResultBytes.Bytes(),
	}
	source.Services.Query, err = database.NewQueryExecutor(source.readDB, limits)
	if err != nil {
		return nil, err
	}
	metadataRows := cfg.Server.Limits.MaxRows
	if metadataRows < math.MaxInt {
		metadataRows++ // One look-ahead row lets the MCP output mark truncation.
	}
	source.Services.Metadata, err = database.NewMetadataServiceWithMaxRows(source.readDB, cfg.Server.Limits.QueryTimeout, metadataRows)
	if err != nil {
		return nil, err
	}

	if !source.ReadOnly && cfg.Server.Features.AnyWrite() {
		source.writeDB, err = options.OpenPool(
			ctx, datasourceConfig, datasourceConfig.Credentials.Write, RoleWrite, cfg.Server.Limits.QueryTimeout,
		)
		if err != nil {
			return nil, err
		}
		if cfg.Server.Features.DML || cfg.Server.Features.DDL {
			source.Services.Command, err = database.NewCommandExecutor(source.writeDB, cfg.Server.Limits.QueryTimeout)
			if err != nil {
				return nil, err
			}
		}
		if cfg.Server.Features.Admin {
			source.Services.Admin, err = database.NewAdminService(source.writeDB, cfg.Server.Limits.QueryTimeout)
			if err != nil {
				return nil, err
			}
		}
	}

	functionPolicies := make([]database.FunctionPolicy, 0, len(datasourceConfig.Functions))
	for _, configured := range datasourceConfig.Functions {
		if configured.Effect == config.FunctionEffectWrite {
			source.HasWriteFunction = true
		}
		schema, name, _ := strings.Cut(configured.Name, ".")
		functionPolicies = append(functionPolicies, database.FunctionPolicy{
			Schema:       schema,
			Name:         name,
			Effect:       database.FunctionEffect(configured.Effect),
			AllowDefiner: configured.AllowDefiner,
		})
	}
	functionWriter := source.writeDB
	if source.ReadOnly || !cfg.Server.Features.FunctionWrite {
		functionWriter = nil
	}
	source.Services.Functions, err = database.NewFunctionService(source.readDB, functionWriter, functionPolicies, limits)
	if err != nil {
		return nil, err
	}

	if datasourceConfig.Monitoring.Enabled {
		source.monitorDB = source.readDB
		if datasourceConfig.Credentials.Monitor.Configured() {
			source.monitorDB, err = options.OpenPool(
				ctx, datasourceConfig, datasourceConfig.Credentials.Monitor, RoleMonitor, datasourceConfig.Monitoring.QueryTimeout,
			)
			if err != nil {
				return nil, err
			}
		}
		performanceSchema, detectErr := options.DetectPerformanceSchema(ctx, source.monitorDB)
		if detectErr != nil {
			return nil, detectErr
		}
		capability, capabilityErr := database.ParseCapability(source.Version.Raw, performanceSchema)
		if capabilityErr != nil {
			return nil, capabilityErr
		}
		monitorLimits := limits
		monitorLimits.QueryTimeout = datasourceConfig.Monitoring.QueryTimeout
		source.Services.Monitor, err = database.NewMonitorService(source.monitorDB, capability, monitorLimits)
		if err != nil {
			return nil, err
		}
	}
	return source, nil
}

// DetectPerformanceSchema reads the runtime switch used to choose safe fixed
// monitoring queries.
func DetectPerformanceSchema(ctx context.Context, db QueryRower) (bool, error) {
	if ctx == nil {
		return false, fmt.Errorf("detect performance_schema: nil context")
	}
	if db == nil {
		return false, fmt.Errorf("detect performance_schema: nil database")
	}
	var enabled bool
	if err := db.QueryRowContext(ctx, performanceSchemaQuery).Scan(&enabled); err != nil {
		return false, fmt.Errorf("detect performance_schema: %w", err)
	}
	return enabled, nil
}

// Source returns one initialized data source without performing any fallback.
func (r *Registry) Source(name string) (*Source, bool) {
	if r == nil {
		return nil, false
	}
	source, ok := r.sources[name]
	return source, ok
}

// Names returns a sorted copy of the configured source names.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.names...)
}

// Close closes all sources and combines independent close errors.
func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	var closeErr error
	for _, name := range r.names {
		closeErr = errors.Join(closeErr, r.sources[name].Close())
	}
	return closeErr
}
