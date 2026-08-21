package mcpserver

import (
	"context"

	"github.com/MegaXChan/mysql-mcp/internal/database"
	"github.com/MegaXChan/mysql-mcp/internal/datasource"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type functionListInput struct {
	Schema string `json:"schema,omitempty" jsonschema:"Optional allowed schema filter."`
}

type functionInput struct {
	Schema string `json:"schema" jsonschema:"Schema from the configured stored-function allow list."`
	Name   string `json:"name" jsonschema:"Function name from the configured allow list."`
}

type functionCallInput struct {
	Schema string `json:"schema" jsonschema:"Schema from the configured stored-function allow list."`
	Name   string `json:"name" jsonschema:"Function name from the configured allow list."`
	Args   []any  `json:"args,omitempty" jsonschema:"Scalar function arguments in declared order; integers outside JavaScript's exact range must be decimal strings. Values are always bound as placeholders."`
}

type functionsOutput struct {
	Functions []database.FunctionInfo `json:"functions"`
}

func registerFunctionTools(server *mcp.Server, source *datasource.Source) {
	service := source.Services.Functions
	if service == nil || source.FunctionCount == 0 {
		return
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.function.list",
		Description: "List stored SQL functions present in the explicit datasource allow list; loadable UDFs are excluded.",
		Annotations: readOnlyAnnotations(),
	}, guarded(source, func(ctx context.Context, input functionListInput) (functionsOutput, error) {
		if input.Schema != "" {
			if err := requireAllowedSchema(source, input.Schema); err != nil {
				return functionsOutput{}, err
			}
		}
		functions, err := service.List(ctx, input.Schema)
		return functionsOutput{Functions: functions}, err
	}))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.function.describe",
		Description: "Describe one allow-listed stored function, including parameters, SQL data access, and security type.",
		Annotations: readOnlyAnnotations(),
	}, guarded(source, func(ctx context.Context, input functionInput) (database.FunctionDescription, error) {
		if err := requireAllowedSchema(source, input.Schema); err != nil {
			return database.FunctionDescription{}, err
		}
		return service.Describe(ctx, input.Schema, input.Name)
	}))

	annotations := readOnlyAnnotations()
	if source.HasWriteFunction {
		annotations = writeAnnotations()
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.function.call",
		Description: "Call one allow-listed stored function with placeholder-bound scalar arguments. Function metadata and DEFINER policy are rechecked inside the transaction.",
		Annotations: annotations,
	}, guarded(source, func(ctx context.Context, input functionCallInput) (database.QueryResult, error) {
		if err := requireAllowedSchema(source, input.Schema); err != nil {
			return database.QueryResult{}, err
		}
		if err := validateScalarArguments(input.Args); err != nil {
			return database.QueryResult{}, err
		}
		return service.Call(ctx, input.Schema, input.Name, input.Args)
	}))
}
