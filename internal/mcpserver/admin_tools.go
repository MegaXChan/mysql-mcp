package mcpserver

import (
	"context"

	"github.com/MegaXChan/mysql-mcp/internal/datasource"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type killQueryInput struct {
	ConnectionID uint64 `json:"connection_id" jsonschema:"Positive MySQL connection identifier returned by mysql.monitor.sessions."`
}

type killQueryOutput struct {
	ConnectionID uint64 `json:"connection_id"`
	Killed       bool   `json:"killed"`
}

func registerAdminTools(server *mcp.Server, source *datasource.Source) {
	if source.Services.Admin == nil {
		return
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.admin.kill_query",
		Description: "Cancel the statement running on one positive MySQL connection ID without exposing arbitrary administrative SQL.",
		Annotations: writeAnnotations(),
	}, guarded(source, func(ctx context.Context, input killQueryInput) (killQueryOutput, error) {
		if err := source.Services.Admin.KillQuery(ctx, input.ConnectionID); err != nil {
			return killQueryOutput{}, err
		}
		return killQueryOutput{ConnectionID: input.ConnectionID, Killed: true}, nil
	}))
}
