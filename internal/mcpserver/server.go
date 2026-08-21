package mcpserver

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/MegaXChan/mysql-mcp/internal/datasource"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewServer creates an MCP server bound permanently to source. Tool inputs do
// not contain a datasource field, so a client connected to /name/mcp cannot
// pivot to another configured database.
func NewServer(source *datasource.Source, version string, logger *slog.Logger) (*mcp.Server, error) {
	if source == nil {
		return nil, fmt.Errorf("create MCP server: nil datasource")
	}
	if source.Policy == nil || source.Services.Query == nil || source.Services.Metadata == nil {
		return nil, fmt.Errorf("create MCP server for %q: datasource is not fully initialized", source.Name)
	}
	if version == "" {
		version = "development"
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:        "mysql-mcp-" + source.Name,
		Title:       "MySQL MCP (" + source.Name + ")",
		Description: "Version-aware, policy-controlled access to one MySQL datasource",
		Version:     version,
	}, &mcp.ServerOptions{
		Instructions: "This endpoint is permanently bound to datasource " + source.Name + ". Use mysql.query for SELECT/UNION, mysql.execute only when exposed, and mysql.function.call for configured stored functions. Never place secrets in SQL.",
		Logger:       logger,
	})

	registerInfoTool(server, source)
	registerQueryTools(server, source)
	registerMetadataTools(server, source)
	registerMonitorTools(server, source)
	registerFunctionTools(server, source)
	registerAdminTools(server, source)
	return server, nil
}

func guarded[Input, Output any](
	source *datasource.Source,
	handler func(context.Context, Input) (Output, error),
) mcp.ToolHandlerFor[Input, Output] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input Input) (*mcp.CallToolResult, Output, error) {
		var zero Output
		release, err := source.Acquire(ctx)
		if err != nil {
			return nil, zero, err
		}
		defer release()
		output, err := handler(ctx, input)
		return nil, output, err
	}
}

func readOnlyAnnotations() *mcp.ToolAnnotations {
	destructive := false
	openWorld := true
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: &destructive,
		OpenWorldHint:   &openWorld,
	}
}

func writeAnnotations() *mcp.ToolAnnotations {
	destructive := true
	openWorld := true
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		DestructiveHint: &destructive,
		OpenWorldHint:   &openWorld,
	}
}

type infoInput struct{}

type infoOutput struct {
	Datasource      string             `json:"datasource"`
	MySQLVersion    datasource.Version `json:"mysql_version"`
	ReadOnly        bool               `json:"read_only"`
	DefaultDatabase string             `json:"default_database,omitempty"`
	AllowedSchemas  []string           `json:"allowed_schemas,omitempty"`
	Features        map[string]bool    `json:"features"`
}

func registerInfoTool(server *mcp.Server, source *datasource.Source) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.info",
		Description: "Return the MySQL version and effective safety capabilities of this fixed datasource endpoint.",
		Annotations: readOnlyAnnotations(),
	}, guarded(source, func(_ context.Context, _ infoInput) (infoOutput, error) {
		return infoOutput{
			Datasource:      source.Name,
			MySQLVersion:    source.Version,
			ReadOnly:        source.ReadOnly,
			DefaultDatabase: source.DefaultDatabase,
			AllowedSchemas:  append([]string(nil), source.AllowedSchemas...),
			Features: map[string]bool{
				"dml":             source.Services.Command != nil && source.Features.DML,
				"ddl":             source.Services.Command != nil && source.Features.DDL,
				"monitoring":      source.Services.Monitor != nil,
				"stored_function": source.Services.Functions != nil && source.FunctionCount > 0,
				"function_write":  source.HasWriteFunction && !source.ReadOnly && source.Features.FunctionWrite,
				"admin":           source.Services.Admin != nil,
			},
		}, nil
	}))
}
