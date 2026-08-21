package mcpserver

import (
	"context"
	"fmt"

	"github.com/MegaXChan/mysql-mcp/internal/database"
	"github.com/MegaXChan/mysql-mcp/internal/datasource"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type schemaInput struct {
	Schema string `json:"schema" jsonschema:"MySQL schema name."`
}

type tableInput struct {
	Schema string `json:"schema" jsonschema:"MySQL schema name."`
	Table  string `json:"table" jsonschema:"MySQL table or view name."`
}

type schemasOutput struct {
	Schemas   []database.SchemaInfo `json:"schemas"`
	Truncated bool                  `json:"truncated"`
}

type tablesOutput struct {
	Tables    []database.TableInfo `json:"tables"`
	Truncated bool                 `json:"truncated"`
}

type indexesOutput struct {
	Indexes   []database.IndexInfo `json:"indexes"`
	Truncated bool                 `json:"truncated"`
}

type constraintsOutput struct {
	Constraints []database.ConstraintInfo `json:"constraints"`
	Truncated   bool                      `json:"truncated"`
}

type tableDescriptionOutput struct {
	Table     database.TableInfo    `json:"table"`
	Columns   []database.ColumnInfo `json:"columns"`
	Truncated bool                  `json:"truncated"`
}

func registerMetadataTools(server *mcp.Server, source *datasource.Source) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.metadata.schemas",
		Description: "List visible schemas filtered by this datasource's exact-name and Glob allow-list policy.",
		Annotations: readOnlyAnnotations(),
	}, guarded(source, func(ctx context.Context, _ struct{}) (schemasOutput, error) {
		var (
			schemas []database.SchemaInfo
			err     error
		)
		if len(source.AllowedSchemas) == 0 && len(source.AllowedSchemaPatterns) == 0 {
			schemas, err = source.Services.Metadata.ListSchemas(ctx)
		} else {
			schemas, err = source.Services.Metadata.ListSchemasAllowed(
				ctx, source.AllowedSchemas, source.AllowedSchemaPatterns,
			)
		}
		if err != nil {
			return schemasOutput{}, err
		}
		filtered := make([]database.SchemaInfo, 0, len(schemas))
		for _, schema := range schemas {
			if source.SchemaAllowed(schema.Name) {
				filtered = append(filtered, schema)
			}
		}
		filtered, truncated := trimMetadataRows(filtered, source.MaxRows)
		return schemasOutput{Schemas: filtered, Truncated: truncated}, nil
	}))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.metadata.tables",
		Description: "List tables and views in one allowed schema.",
		Annotations: readOnlyAnnotations(),
	}, guarded(source, func(ctx context.Context, input schemaInput) (tablesOutput, error) {
		if err := requireAllowedSchema(source, input.Schema); err != nil {
			return tablesOutput{}, err
		}
		tables, err := source.Services.Metadata.ListTables(ctx, input.Schema)
		tables, truncated := trimMetadataRows(tables, source.MaxRows)
		return tablesOutput{Tables: tables, Truncated: truncated}, err
	}))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.metadata.describe_table",
		Description: "Describe one allowed table or view and all of its columns.",
		Annotations: readOnlyAnnotations(),
	}, guarded(source, func(ctx context.Context, input tableInput) (tableDescriptionOutput, error) {
		if err := requireAllowedSchema(source, input.Schema); err != nil {
			return tableDescriptionOutput{}, err
		}
		description, err := source.Services.Metadata.DescribeTable(ctx, input.Schema, input.Table)
		if err != nil {
			return tableDescriptionOutput{}, err
		}
		columns, truncated := trimMetadataRows(description.Columns, source.MaxRows)
		return tableDescriptionOutput{Table: description.Table, Columns: columns, Truncated: truncated}, nil
	}))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.metadata.indexes",
		Description: "List index columns for one allowed table or view.",
		Annotations: readOnlyAnnotations(),
	}, guarded(source, func(ctx context.Context, input tableInput) (indexesOutput, error) {
		if err := requireAllowedSchema(source, input.Schema); err != nil {
			return indexesOutput{}, err
		}
		indexes, err := source.Services.Metadata.ListIndexes(ctx, input.Schema, input.Table)
		indexes, truncated := trimMetadataRows(indexes, source.MaxRows)
		return indexesOutput{Indexes: indexes, Truncated: truncated}, err
	}))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "mysql.metadata.constraints",
		Description: "List primary, unique, check, and foreign-key constraints for one allowed table.",
		Annotations: readOnlyAnnotations(),
	}, guarded(source, func(ctx context.Context, input tableInput) (constraintsOutput, error) {
		if err := requireAllowedSchema(source, input.Schema); err != nil {
			return constraintsOutput{}, err
		}
		constraints, err := source.Services.Metadata.ListConstraints(ctx, input.Schema, input.Table)
		constraints, truncated := trimMetadataRows(constraints, source.MaxRows)
		return constraintsOutput{Constraints: constraints, Truncated: truncated}, err
	}))
}

func trimMetadataRows[Value any](values []Value, limit int) ([]Value, bool) {
	if len(values) <= limit {
		return values, false
	}
	return values[:limit], true
}

func requireAllowedSchema(source *datasource.Source, schema string) error {
	if schema == "" {
		return fmt.Errorf("schema cannot be empty")
	}
	if !source.SchemaAllowed(schema) {
		return fmt.Errorf("schema %q is not allowed for datasource %q", schema, source.Name)
	}
	return nil
}
