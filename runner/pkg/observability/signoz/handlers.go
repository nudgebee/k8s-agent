package signoz

import (
	"context"
	"encoding/json"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Handlers wires Signoz primitive actions. action_params is forwarded as the
// HTTP body unchanged.
func Handlers(c *Client) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"signoz_query_range":   wrap(c.QueryRange),
		"signoz_label_suggest": wrap(c.LabelSuggest),
		"signoz_value_suggest": wrap(c.ValueSuggest),
	}
}

func wrap(fn func(context.Context, any) (json.RawMessage, error)) dispatch.Handler {
	return func(ctx context.Context, params map[string]any) (any, error) {
		raw, err := fn(ctx, params)
		if err != nil {
			return nil, err
		}
		return raw, nil
	}
}
