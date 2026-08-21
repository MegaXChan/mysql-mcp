package mcpserver

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/MegaXChan/mysql-mcp/internal/database"
	"github.com/MegaXChan/mysql-mcp/internal/datasource"
	"github.com/MegaXChan/mysql-mcp/internal/policy"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type queryInput struct {
	SQL     string `json:"sql" jsonschema:"A single SELECT or UNION statement. Stored functions are not allowed here."`
	Args    []any  `json:"args,omitempty" jsonschema:"Scalar values bound to question-mark placeholders in order. Integers outside JavaScript's exact range must be decimal strings."`
	MaxRows int    `json:"max_rows,omitempty" jsonschema:"Optional response row limit. Zero uses the configured default."`
}

type explainInput struct {
	SQL  string `json:"sql" jsonschema:"A single SELECT or UNION statement without an EXPLAIN prefix."`
	Args []any  `json:"args,omitempty" jsonschema:"Scalar values bound to question-mark placeholders in order. Integers outside JavaScript's exact range must be decimal strings."`
}

type executeInput struct {
	SQL  string `json:"sql" jsonschema:"One DML or DDL statement authorized by the server feature flags."`
	Args []any  `json:"args,omitempty" jsonschema:"Scalar values bound to question-mark placeholders in order. Integers outside JavaScript's exact range must be decimal strings."`
}

func registerQueryTools(server *mcp.Server, source *datasource.Source) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.query",
		Description: "Run one Vitess-parsed, schema-restricted SELECT/UNION in a database read-only transaction.",
		Annotations: readOnlyAnnotations(),
	}, guarded(source, func(ctx context.Context, input queryInput) (database.QueryResult, error) {
		if err := validateSQLInput(source, input.SQL, input.Args); err != nil {
			return database.QueryResult{}, err
		}
		if _, err := source.Policy.ValidateReadQueryForSchemas(input.SQL, source.DefaultDatabase, source.AllowedSchemas); err != nil {
			return database.QueryResult{}, err
		}
		limit, err := responseRowLimit(source, input.MaxRows)
		if err != nil {
			return database.QueryResult{}, err
		}
		result, err := source.Services.Query.QueryWithMaxRows(ctx, limit, input.SQL, input.Args...)
		if err != nil {
			return result, err
		}
		return trimRows(result, limit), nil
	}))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.explain",
		Description: "Explain one safe SELECT/UNION. The server adds EXPLAIN; EXPLAIN ANALYZE is never accepted.",
		Annotations: readOnlyAnnotations(),
	}, guarded(source, func(ctx context.Context, input explainInput) (database.QueryResult, error) {
		if err := validateSQLInput(source, input.SQL, input.Args); err != nil {
			return database.QueryResult{}, err
		}
		if _, err := source.Policy.ValidateReadQueryForSchemas(input.SQL, source.DefaultDatabase, source.AllowedSchemas); err != nil {
			return database.QueryResult{}, err
		}
		return source.Services.Query.Query(ctx, "EXPLAIN "+input.SQL, input.Args...)
	}))

	if source.Services.Command == nil {
		return
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.execute",
		Description: "Execute one authorized DML in a read-write transaction or one MySQL implicit-commit DDL statement. Session, transaction, stored-program, and arbitrary admin SQL are rejected.",
		Annotations: writeAnnotations(),
	}, guarded(source, func(ctx context.Context, input executeInput) (database.CommandResult, error) {
		if err := validateSQLInput(source, input.SQL, input.Args); err != nil {
			return database.CommandResult{}, err
		}
		classification, err := source.Policy.ValidateCommand(input.SQL)
		if err != nil {
			return database.CommandResult{}, err
		}
		switch classification.Class {
		case policy.ClassWrite:
			if !source.Features.DML {
				return database.CommandResult{}, fmt.Errorf("DML is disabled for datasource %q", source.Name)
			}
		case policy.ClassDDL:
			if !source.Features.DDL {
				return database.CommandResult{}, fmt.Errorf("DDL is disabled for datasource %q", source.Name)
			}
		default:
			return database.CommandResult{}, fmt.Errorf("statement class %q is not accepted by mysql.execute", classification.Class)
		}
		if err := policy.ValidateCommandForSchemas(classification.Statement, source.DefaultDatabase, source.AllowedSchemas); err != nil {
			return database.CommandResult{}, err
		}
		if classification.Class == policy.ClassDDL {
			return source.Services.Command.ExecDDL(ctx, input.SQL, input.Args...)
		}
		return source.Services.Command.Exec(ctx, input.SQL, input.Args...)
	}))
}

func validateSQLInput(source *datasource.Source, statement string, args []any) error {
	if strings.TrimSpace(statement) == "" {
		return fmt.Errorf("SQL statement cannot be empty")
	}
	if int64(len(statement)) > source.MaxSQLBytes {
		return fmt.Errorf("SQL statement exceeds the configured %d-byte limit", source.MaxSQLBytes)
	}
	return validateScalarArguments(args)
}

func validateScalarArguments(args []any) error {
	for index, argument := range args {
		switch value := argument.(type) {
		case nil, bool, string, []byte,
			int, int8, int16, int32, int64,
			uint, uint8, uint16, uint32, uint64,
			float32:
		case float64:
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("argument %d is not a finite JSON number", index)
			}
			const maxExactJSONInteger = float64(1<<53 - 1)
			if math.Trunc(value) == value && math.Abs(value) > maxExactJSONInteger {
				return fmt.Errorf("argument %d is an integer outside the exact JSON number range; pass it as a decimal string", index)
			}
		default:
			return fmt.Errorf("argument %d has unsupported non-scalar type %T", index, argument)
		}
	}
	return nil
}

func responseRowLimit(source *datasource.Source, requested int) (int, error) {
	if requested < 0 {
		return 0, fmt.Errorf("max_rows cannot be negative")
	}
	if requested == 0 {
		return source.DefaultRows, nil
	}
	if requested > source.MaxRows {
		return 0, fmt.Errorf("max_rows %d exceeds the configured maximum %d", requested, source.MaxRows)
	}
	return requested, nil
}

func trimRows(result database.QueryResult, limit int) database.QueryResult {
	if limit >= 0 && len(result.Rows) > limit {
		result.Rows = result.Rows[:limit]
		result.Truncated = true
		if result.Reason == "" {
			result.Reason = database.TruncatedMaxRows
		}
	}
	return result
}
