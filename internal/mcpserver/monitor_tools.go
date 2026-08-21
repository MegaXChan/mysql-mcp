package mcpserver

import (
	"context"

	"github.com/MegaXChan/mysql-mcp/internal/database"
	"github.com/MegaXChan/mysql-mcp/internal/datasource"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func registerMonitorTools(server *mcp.Server, source *datasource.Source) {
	service := source.Services.Monitor
	if service == nil {
		return
	}
	addMonitorTool(server, source, "mysql.monitor.overview", "Return fixed server identity, read-only state, and connection-capacity metrics.", service.Overview)
	addMonitorTool(server, source, "mysql.monitor.storage", "Return fixed per-schema table and storage-size metrics.", service.Storage)
	if source.Monitoring.Sessions {
		addMonitorTool(server, source, "mysql.monitor.sessions", "List the longest-running server sessions using a fixed INFORMATION_SCHEMA query.", service.Sessions)
	}
	if source.Monitoring.Locks && service.SupportsLocks() {
		addMonitorTool(server, source, "mysql.monitor.locks", "List lock waits using the correct fixed query for MySQL 5.7 or 8.x.", service.Locks)
	}
	if source.Monitoring.TopQueries && service.SupportsTopQueries() {
		addMonitorTool(server, source, "mysql.monitor.top_queries", "Return top normalized statements from performance_schema using a fixed query.", service.TopQueries)
	}
	if source.Monitoring.Replication {
		addMonitorTool(server, source, "mysql.monitor.replication", "Return replication status using version-appropriate MySQL terminology.", service.Replication)
	}
	if source.Monitoring.InnoDBStatus {
		addMonitorTool(server, source, "mysql.monitor.innodb_status", "Return SHOW ENGINE INNODB STATUS using a fixed server-owned statement.", service.InnoDBStatus)
	}
}

func addMonitorTool(
	server *mcp.Server,
	source *datasource.Source,
	name, description string,
	handler func(context.Context) (database.QueryResult, error),
) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: readOnlyAnnotations(),
	}, guarded(source, func(ctx context.Context, _ struct{}) (database.QueryResult, error) {
		return handler(ctx)
	}))
}
