package loki

import (
	"context"
	"encoding/json"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Handlers returns the action handler map for Loki primitives. Names mirror
// Prometheus's pattern: action_name = `loki_<endpoint>`. Backend composers
// invoke these.
func Handlers(c *Client) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"loki_query":        wrap(c.handleQuery),
		"loki_query_range":  wrap(c.handleQueryRange),
		"loki_labels":       wrap(c.handleLabels),
		"loki_label_values": wrap(c.handleLabelValues),
		"loki_series":       wrap(c.handleSeries),
	}
}

func wrap(fn func(ctx context.Context, p map[string]any) (json.RawMessage, error)) dispatch.Handler {
	return func(ctx context.Context, params map[string]any) (any, error) {
		raw, err := fn(ctx, params)
		if err != nil {
			return nil, err
		}
		return raw, nil
	}
}

func (c *Client) handleQuery(ctx context.Context, p map[string]any) (json.RawMessage, error) {
	return c.Query(ctx, str(p, "query"), str(p, "time"), str(p, "limit"))
}

func (c *Client) handleQueryRange(ctx context.Context, p map[string]any) (json.RawMessage, error) {
	return c.QueryRange(ctx, str(p, "query"), str(p, "start"), str(p, "end"), str(p, "step"), str(p, "direction"), str(p, "limit"))
}

func (c *Client) handleLabels(ctx context.Context, p map[string]any) (json.RawMessage, error) {
	return c.Labels(ctx, str(p, "start"), str(p, "end"), str(p, "query"))
}

func (c *Client) handleLabelValues(ctx context.Context, p map[string]any) (json.RawMessage, error) {
	return c.LabelValues(ctx, str(p, "label"), str(p, "start"), str(p, "end"), str(p, "query"))
}

func (c *Client) handleSeries(ctx context.Context, p map[string]any) (json.RawMessage, error) {
	return c.Series(ctx, strSlice(p, "match[]"), str(p, "start"), str(p, "end"))
}

func str(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	s, _ := p[key].(string)
	return s
}

func strSlice(p map[string]any, key string) []string {
	if p == nil {
		return nil
	}
	switch v := p[key].(type) {
	case nil:
		return nil
	case string:
		return []string{v}
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
