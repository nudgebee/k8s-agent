package enrichers

import (
	"context"

	"github.com/nudgebee/nudgebee-agent/pkg/clickhouse"
	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// QueryData implements the `query_data` action — runs a ClickHouse SQL
// query and returns `{data, columns, column_types, error}`.
//
// Callers read `response.data.data` (rows), `response.data.columns`, etc.
//
// Example response:
//
//	{
//	  "success": true,
//	  "data":    {"data": [[1]], "columns":["count"], "column_types":["UInt64"], "error": null},
//	  "request_id": "..."
//	}
func QueryData(ch *clickhouse.Client) dispatch.Handler {
	return func(ctx context.Context, params map[string]any) (any, error) {
		query, _ := params["query"].(string)
		// The action accepts a `values` list for parameterized queries; api-server
		// callers never send it. We pass through whatever is provided so the
		// ClickHouse client can reject with a clear error if non-empty.
		var values []any
		if raw, ok := params["values"].([]any); ok {
			values = raw
		}
		result, err := ch.Query(ctx, query, values)
		if err != nil {
			// Hard error path (unexpected). Log and return success:false.
			return map[string]any{
				"success": false,
				"data":    nil,
				"msg":     err.Error(),
			}, nil
		}
		return map[string]any{
			"success": true,
			"data":    result,
		}, nil
	}
}
