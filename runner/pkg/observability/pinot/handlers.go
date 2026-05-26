package pinot

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Handlers wires Pinot primitive actions to the dispatcher.
func Handlers(c *Client) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"pinot_query":  wrapQuery(c),
		"pinot_tables": wrapNoArg(c.Tables),
		"pinot_schema": wrapSchema(c),
	}
}

func wrapQuery(c *Client) dispatch.Handler {
	return func(ctx context.Context, params map[string]any) (any, error) {
		sql, _ := params["sql"].(string)
		if sql == "" {
			return nil, fmt.Errorf("pinot_query: sql param required")
		}
		return c.Query(ctx, sql)
	}
}

func wrapSchema(c *Client) dispatch.Handler {
	return func(ctx context.Context, params map[string]any) (any, error) {
		table, _ := params["table"].(string)
		if table == "" {
			return nil, fmt.Errorf("pinot_schema: table param required")
		}
		return c.Schema(ctx, table)
	}
}

func wrapNoArg(fn func(context.Context) (json.RawMessage, error)) dispatch.Handler {
	return func(ctx context.Context, _ map[string]any) (any, error) {
		return fn(ctx)
	}
}
