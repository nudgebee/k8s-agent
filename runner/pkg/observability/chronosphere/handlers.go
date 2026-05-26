package chronosphere

import (
	"context"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

func Handlers(c *Client) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"chronosphere_query_traces": func(ctx context.Context, p map[string]any) (any, error) {
			raw, err := c.QueryTraces(ctx, p)
			if err != nil {
				return nil, err
			}
			return raw, nil
		},
	}
}
