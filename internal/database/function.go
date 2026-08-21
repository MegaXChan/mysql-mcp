package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type FunctionEffect string

const (
	FunctionEffectRead  FunctionEffect = "read"
	FunctionEffectWrite FunctionEffect = "write"
)

// FunctionPolicy is an explicit allow-list entry. SQL_DATA_ACCESS declarations
// in MySQL are advisory and therefore never replace this application policy or
// the database account's privileges.
type FunctionPolicy struct {
	Schema       string         `json:"schema" yaml:"schema"`
	Name         string         `json:"name" yaml:"name"`
	Effect       FunctionEffect `json:"effect" yaml:"effect"`
	AllowDefiner bool           `json:"allow_definer" yaml:"allow_definer"`
}

type FunctionInfo struct {
	Schema           string         `json:"schema"`
	Name             string         `json:"name"`
	DataType         string         `json:"data_type"`
	DTDIdentifier    string         `json:"dtd_identifier"`
	Deterministic    bool           `json:"deterministic"`
	SQLDataAccess    string         `json:"sql_data_access"`
	SecurityType     string         `json:"security_type"`
	RoutineComment   string         `json:"comment,omitempty"`
	ConfiguredEffect FunctionEffect `json:"configured_effect"`
	AllowDefiner     bool           `json:"allow_definer"`
}

type FunctionParameter struct {
	OrdinalPosition int64   `json:"ordinal_position"`
	Mode            *string `json:"mode,omitempty"`
	Name            *string `json:"name,omitempty"`
	DataType        string  `json:"data_type"`
	DTDIdentifier   string  `json:"dtd_identifier"`
	CharacterSet    *string `json:"character_set,omitempty"`
	Collation       *string `json:"collation,omitempty"`
}

type FunctionDescription struct {
	Function   FunctionInfo        `json:"function"`
	Return     *FunctionParameter  `json:"return,omitempty"`
	Parameters []FunctionParameter `json:"parameters"`
}

type FunctionService struct {
	reader   *sql.DB
	writer   *sql.DB
	policies map[string]FunctionPolicy
	ordered  []FunctionPolicy
	limits   Limits
}

const (
	functionInfoColumns = `ROUTINE_SCHEMA, ROUTINE_NAME, DATA_TYPE, DTD_IDENTIFIER,
       IS_DETERMINISTIC, SQL_DATA_ACCESS, SECURITY_TYPE, ROUTINE_COMMENT`

	describeFunctionSQL = `SELECT ` + functionInfoColumns + `
FROM INFORMATION_SCHEMA.ROUTINES
WHERE ROUTINE_SCHEMA = ? AND ROUTINE_NAME = ? AND ROUTINE_TYPE = 'FUNCTION'`

	functionParametersSQL = `SELECT ORDINAL_POSITION, PARAMETER_MODE, PARAMETER_NAME,
       DATA_TYPE, DTD_IDENTIFIER, CHARACTER_SET_NAME, COLLATION_NAME
FROM INFORMATION_SCHEMA.PARAMETERS
WHERE SPECIFIC_SCHEMA = ? AND SPECIFIC_NAME = ? AND ROUTINE_TYPE = 'FUNCTION'
ORDER BY ORDINAL_POSITION`
)

func NewFunctionService(reader, writer *sql.DB, policies []FunctionPolicy, limits Limits) (*FunctionService, error) {
	if reader == nil {
		return nil, invalid("new function service", "nil reader database")
	}
	normalized, err := limits.WithDefaults()
	if err != nil {
		return nil, err
	}
	service := &FunctionService{
		reader:   reader,
		writer:   writer,
		policies: make(map[string]FunctionPolicy, len(policies)),
		ordered:  make([]FunctionPolicy, 0, len(policies)),
		limits:   normalized,
	}
	for _, policy := range policies {
		if err := validateIdentifier("function schema", policy.Schema); err != nil {
			return nil, err
		}
		if err := validateIdentifier("function name", policy.Name); err != nil {
			return nil, err
		}
		switch policy.Effect {
		case FunctionEffectRead:
		case FunctionEffectWrite:
			if writer == nil {
				return nil, invalid("new function service", "write policy requires writer database")
			}
		default:
			return nil, invalid("new function service", fmt.Sprintf("invalid effect %q", policy.Effect))
		}
		key := functionKey(policy.Schema, policy.Name)
		if _, exists := service.policies[key]; exists {
			return nil, invalid("new function service", "duplicate function policy "+key)
		}
		service.policies[key] = policy
		service.ordered = append(service.ordered, policy)
	}
	return service, nil
}

// List returns only stored functions present in the explicit policy allow
// list. Loadable/native UDFs are excluded because the query is restricted to
// INFORMATION_SCHEMA.ROUTINES and ROUTINE_TYPE='FUNCTION'.
func (s *FunctionService) List(ctx context.Context, schema string) ([]FunctionInfo, error) {
	if schema != "" {
		if err := validateIdentifier("function schema", schema); err != nil {
			return nil, err
		}
	}
	selected := make([]FunctionPolicy, 0, len(s.ordered))
	for _, policy := range s.ordered {
		if schema == "" || policy.Schema == schema {
			selected = append(selected, policy)
		}
	}
	if len(selected) == 0 {
		return []FunctionInfo{}, nil
	}

	var predicate strings.Builder
	args := make([]any, 0, len(selected)*2)
	for i, policy := range selected {
		if i > 0 {
			predicate.WriteString(" OR ")
		}
		predicate.WriteString("(ROUTINE_SCHEMA = ? AND ROUTINE_NAME = ?)")
		args = append(args, policy.Schema, policy.Name)
	}
	statement := `SELECT ` + functionInfoColumns + `
FROM INFORMATION_SCHEMA.ROUTINES
WHERE ROUTINE_TYPE = 'FUNCTION' AND (` + predicate.String() + `)
ORDER BY ROUTINE_SCHEMA, ROUTINE_NAME`

	queryContext, cancel, err := serviceContext(ctx, s.limits.QueryTimeout, "list functions")
	if err != nil {
		return nil, err
	}
	defer cancel()
	rows, err := s.reader.QueryContext(queryContext, statement, args...)
	if err != nil {
		return nil, wrapDatabaseError("list functions", err)
	}
	defer rows.Close()

	functions := make([]FunctionInfo, 0, len(selected))
	for rows.Next() {
		function, err := scanFunctionInfo(rows)
		if err != nil {
			return nil, wrapDatabaseError("scan function metadata", err)
		}
		policy, allowed := s.policies[functionKey(function.Schema, function.Name)]
		if !allowed {
			// The SQL predicate should make this impossible. Keeping the check is
			// defense-in-depth against unexpected collation behavior.
			continue
		}
		applyFunctionPolicy(&function, policy)
		functions = append(functions, function)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDatabaseError("iterate function metadata", err)
	}
	return functions, nil
}

func (s *FunctionService) Describe(ctx context.Context, schema, name string) (FunctionDescription, error) {
	policy, err := s.policy(schema, name)
	if err != nil {
		return FunctionDescription{}, err
	}
	queryContext, cancel, err := serviceContext(ctx, s.limits.QueryTimeout, "describe function")
	if err != nil {
		return FunctionDescription{}, err
	}
	defer cancel()
	description, err := describeFunction(queryContext, s.reader, schema, name)
	if err != nil {
		return FunctionDescription{}, err
	}
	applyFunctionPolicy(&description.Function, policy)
	return description, nil
}

// Call invokes a stored function using a package-generated SELECT with one
// placeholder per argument. The function and schema identifiers come from the
// validated allow list; callers cannot inject an expression through args.
func (s *FunctionService) Call(ctx context.Context, schema, name string, args []any) (result QueryResult, err error) {
	started := time.Now()
	defer func() {
		result.Elapsed = time.Since(started)
		result.ElapsedMillis = result.Elapsed.Milliseconds()
	}()

	policy, err := s.policy(schema, name)
	if err != nil {
		return result, err
	}
	if err := validateFunctionArguments(args); err != nil {
		return result, err
	}
	callContext, cancel, err := serviceContext(ctx, s.limits.QueryTimeout, "call function")
	if err != nil {
		return result, err
	}
	defer cancel()

	db := s.reader
	readOnly := true
	if policy.Effect == FunctionEffectWrite {
		db = s.writer
		readOnly = false
	}
	tx, err := db.BeginTx(callContext, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return result, wrapDatabaseError("begin function transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	description, err := describeFunction(callContext, tx, schema, name)
	if err != nil {
		return result, err
	}
	if err := validateFunctionMetadata(description); err != nil {
		return result, err
	}
	if strings.EqualFold(description.Function.SecurityType, "DEFINER") && !policy.AllowDefiner {
		return result, policyDenied("call function", "SQL SECURITY DEFINER is not allowed")
	}
	if policy.Effect == FunctionEffectRead && strings.EqualFold(description.Function.SQLDataAccess, "MODIFIES SQL DATA") {
		return result, policyDenied("call function", "read policy conflicts with MODIFIES SQL DATA declaration")
	}
	if len(description.Parameters) != len(args) {
		return result, invalid(
			"call function",
			fmt.Sprintf("argument count mismatch: expected %d, got %d", len(description.Parameters), len(args)),
		)
	}

	quotedSchema, err := quoteIdentifier(schema)
	if err != nil {
		return result, err
	}
	quotedName, err := quoteIdentifier(name)
	if err != nil {
		return result, err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	statement := fmt.Sprintf("SELECT %s.%s(%s) AS `result`", quotedSchema, quotedName, placeholders)
	rows, err := tx.QueryContext(callContext, statement, args...)
	if err != nil {
		return result, wrapDatabaseError("execute stored function", err)
	}
	result, err = scanRows(rows, s.limits)
	if err != nil {
		return result, wrapDatabaseError("read stored function result", err)
	}

	if policy.Effect == FunctionEffectWrite {
		if err := tx.Commit(); err != nil {
			return result, wrapDatabaseError("commit stored function", err)
		}
	}
	return result, nil
}

type functionQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func describeFunction(ctx context.Context, queryer functionQueryer, schema, name string) (FunctionDescription, error) {
	function, err := scanFunctionInfo(queryer.QueryRowContext(ctx, describeFunctionSQL, schema, name))
	if errors.Is(err, sql.ErrNoRows) {
		return FunctionDescription{}, notFound("describe function", schema+"."+name)
	}
	if err != nil {
		return FunctionDescription{}, wrapDatabaseError("read function metadata", err)
	}

	rows, err := queryer.QueryContext(ctx, functionParametersSQL, schema, name)
	if err != nil {
		return FunctionDescription{}, wrapDatabaseError("list function parameters", err)
	}
	defer rows.Close()
	description := FunctionDescription{Function: function, Parameters: []FunctionParameter{}}
	for rows.Next() {
		var (
			parameter                               FunctionParameter
			mode, parameterName, charset, collation sql.NullString
		)
		if err := rows.Scan(
			&parameter.OrdinalPosition, &mode, &parameterName, &parameter.DataType,
			&parameter.DTDIdentifier, &charset, &collation,
		); err != nil {
			return FunctionDescription{}, wrapDatabaseError("scan function parameter", err)
		}
		parameter.Mode = stringPointer(mode)
		parameter.Name = stringPointer(parameterName)
		parameter.CharacterSet = stringPointer(charset)
		parameter.Collation = stringPointer(collation)
		if parameter.OrdinalPosition == 0 {
			copy := parameter
			description.Return = &copy
		} else {
			description.Parameters = append(description.Parameters, parameter)
		}
	}
	if err := rows.Err(); err != nil {
		return FunctionDescription{}, wrapDatabaseError("iterate function parameters", err)
	}
	return description, nil
}

type functionInfoScanner interface {
	Scan(dest ...any) error
}

func scanFunctionInfo(scanner functionInfoScanner) (FunctionInfo, error) {
	var (
		function      FunctionInfo
		deterministic string
	)
	if err := scanner.Scan(
		&function.Schema, &function.Name, &function.DataType, &function.DTDIdentifier,
		&deterministic, &function.SQLDataAccess, &function.SecurityType, &function.RoutineComment,
	); err != nil {
		return FunctionInfo{}, err
	}
	function.Deterministic = strings.EqualFold(deterministic, "YES")
	return function, nil
}

func applyFunctionPolicy(function *FunctionInfo, policy FunctionPolicy) {
	function.ConfiguredEffect = policy.Effect
	function.AllowDefiner = policy.AllowDefiner
}

func validateFunctionMetadata(description FunctionDescription) error {
	if description.Return == nil {
		return invalid("validate function metadata", "return parameter metadata is missing")
	}
	switch strings.ToUpper(description.Function.SecurityType) {
	case "INVOKER", "DEFINER":
	default:
		return policyDenied("validate function metadata", "unknown SQL SECURITY value")
	}
	switch strings.ToUpper(description.Function.SQLDataAccess) {
	case "NO SQL", "CONTAINS SQL", "READS SQL DATA", "MODIFIES SQL DATA":
	default:
		return policyDenied("validate function metadata", "unknown SQL DATA ACCESS value")
	}
	for index, parameter := range description.Parameters {
		if parameter.OrdinalPosition != int64(index+1) {
			return invalid("validate function metadata", "input parameter ordinals are not contiguous")
		}
		if parameter.Mode != nil && !strings.EqualFold(*parameter.Mode, "IN") {
			return invalid("validate function metadata", "stored function has a non-IN parameter")
		}
	}
	return nil
}

func (s *FunctionService) policy(schema, name string) (FunctionPolicy, error) {
	if err := validateIdentifier("function schema", schema); err != nil {
		return FunctionPolicy{}, err
	}
	if err := validateIdentifier("function name", name); err != nil {
		return FunctionPolicy{}, err
	}
	policy, ok := s.policies[functionKey(schema, name)]
	if !ok {
		return FunctionPolicy{}, policyDenied("function policy", schema+"."+name+" is not allowed")
	}
	return policy, nil
}

func functionKey(schema, name string) string {
	// Schema-name case sensitivity differs by platform and therefore remains
	// exact. MySQL routine names are case-insensitive within a schema, so fold
	// only that component; config duplicate detection uses identical semantics.
	return schema + "\x00" + strings.ToLower(name)
}

func validateFunctionArguments(args []any) error {
	for index, argument := range args {
		switch argument.(type) {
		case nil,
			bool,
			int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64,
			float32, float64,
			string, []byte, time.Time:
			// All accepted types are scalar driver values. In particular, maps and
			// slices cannot smuggle SQL expressions into the generated statement.
		default:
			return invalid("validate function arguments", fmt.Sprintf("argument %d has unsupported type %T", index, argument))
		}
	}
	return nil
}
