package policy

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"vitess.io/vitess/go/vt/sqlparser"
)

var (
	ErrEmptyDatasource   = errors.New("datasource name is empty")
	ErrDatasourceExists  = errors.New("datasource policy already exists")
	ErrUnknownDatasource = errors.New("datasource policy is not registered")
)

// Registry owns one immutable Policy per datasource. sync.Map makes reads and
// atomic policy replacement safe while a configuration reload is in progress.
type Registry struct {
	policies sync.Map // map[string]*Policy
}

// NewRegistry returns an empty datasource policy registry.
func NewRegistry() *Registry { return &Registry{} }

// RegisterDatasource adds a policy and fails if the name already exists.
func (r *Registry) RegisterDatasource(name, mysqlServerVersion string) error {
	key, err := datasourceKey(name)
	if err != nil {
		return err
	}
	configured, err := New(mysqlServerVersion)
	if err != nil {
		return err
	}
	if _, loaded := r.policies.LoadOrStore(key, configured); loaded {
		return fmt.Errorf("%w: %s", ErrDatasourceExists, key)
	}
	return nil
}

// SetDatasource atomically adds or replaces a datasource policy. Constructing
// the new parser happens before Store, so a bad version never replaces a
// working policy during configuration reload.
func (r *Registry) SetDatasource(name, mysqlServerVersion string) error {
	key, err := datasourceKey(name)
	if err != nil {
		return err
	}
	configured, err := New(mysqlServerVersion)
	if err != nil {
		return err
	}
	r.policies.Store(key, configured)
	return nil
}

// RemoveDatasource removes a policy and reports whether it existed.
func (r *Registry) RemoveDatasource(name string) bool {
	key, err := datasourceKey(name)
	if err != nil {
		return false
	}
	_, loaded := r.policies.LoadAndDelete(key)
	return loaded
}

// PolicyFor returns the immutable policy associated with name.
func (r *Registry) PolicyFor(name string) (*Policy, error) {
	if r == nil {
		return nil, ErrUnknownDatasource
	}
	key, err := datasourceKey(name)
	if err != nil {
		return nil, err
	}
	value, found := r.policies.Load(key)
	if !found {
		return nil, fmt.Errorf("%w: %s", ErrUnknownDatasource, key)
	}
	return value.(*Policy), nil
}

// Classify parses and classifies SQL using the named datasource's MySQL
// version-aware parser.
func (r *Registry) Classify(datasource, sql string) (Classification, error) {
	configured, err := r.PolicyFor(datasource)
	if err != nil {
		return Classification{}, err
	}
	return configured.Classify(sql)
}

// ValidateReadQuery applies the named datasource's read-only policy.
func (r *Registry) ValidateReadQuery(datasource, sql string) (sqlparser.Statement, error) {
	configured, err := r.PolicyFor(datasource)
	if err != nil {
		return nil, err
	}
	return configured.ValidateReadQuery(sql)
}

// ValidateReadQueryForSchemas applies both raw read-query validation and the
// datasource's configured schema boundary.
func (r *Registry) ValidateReadQueryForSchemas(
	datasource, sql, defaultDB string,
	allowed []string,
) (sqlparser.Statement, error) {
	configured, err := r.PolicyFor(datasource)
	if err != nil {
		return nil, err
	}
	return configured.ValidateReadQueryForSchemas(sql, defaultDB, allowed)
}

// ValidateCommand applies raw DML/DDL expression safety with the named
// datasource's version-aware parser. Schema validation remains a separate call
// because default_database and allowed_schemas belong to datasource config.
func (r *Registry) ValidateCommand(datasource, sql string) (Classification, error) {
	configured, err := r.PolicyFor(datasource)
	if err != nil {
		return Classification{}, err
	}
	return configured.ValidateCommand(sql)
}

// ValidateExplain applies the named datasource's EXPLAIN policy.
func (r *Registry) ValidateExplain(datasource, sql string) (*sqlparser.ExplainStmt, error) {
	configured, err := r.PolicyFor(datasource)
	if err != nil {
		return nil, err
	}
	return configured.ValidateExplain(sql)
}

func datasourceKey(name string) (string, error) {
	key := strings.TrimSpace(name)
	if key == "" {
		return "", ErrEmptyDatasource
	}
	return key, nil
}
