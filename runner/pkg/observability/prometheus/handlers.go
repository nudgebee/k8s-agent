package prometheus

import (
	"context"
	"encoding/json"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Handlers returns the dispatch.Handler map for all Prometheus primitive
// actions, keyed by action_name. Each handler reads its parameters from the
// untyped params map and returns the raw Prometheus JSON response.
//
// Action names match the standard Prometheus HTTP API endpoints; the
// backend's enricher composers call these primitives to assemble
// higher-level findings.
func Handlers(c *Client) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"prometheus_query":       wrap(c.handleQuery),
		"prometheus_query_range": wrap(c.handleQueryRange),
		// Raw /api/v1/labels (list of all label NAMES). The
		// `prometheus_labels` action is a label-VALUES query wrapped in
		// a Finding (registered in cmd/agent/main.go after Handlers
		// merges) — the raw passthrough kept its old key under a more
		// accurate name so nothing accidentally hits /api/v1/labels
		// expecting a Finding.
		"prometheus_label_names":  wrap(c.handleLabels),
		"prometheus_label_values": wrap(c.handleLabelValues),
		"prometheus_series":       wrap(c.handleSeries),
		"prometheus_alerts":       wrap(c.handleAlerts),
	}
}

func wrap(fn func(ctx context.Context, params map[string]any) (json.RawMessage, error)) dispatch.Handler {
	return func(ctx context.Context, params map[string]any) (any, error) {
		raw, err := fn(ctx, params)
		if err != nil {
			return nil, err
		}
		// Return raw JSON so the response body matches what Prometheus emitted
		// (no double-encoding, no field renaming). The dispatcher passes this
		// straight through to the relay response envelope's `data` field.
		return raw, nil
	}
}

func (c *Client) handleQuery(ctx context.Context, p map[string]any) (json.RawMessage, error) {
	return c.Query(ctx, str(p, "query"), str(p, "time"), str(p, "timeout"))
}

func (c *Client) handleQueryRange(ctx context.Context, p map[string]any) (json.RawMessage, error) {
	return c.QueryRange(ctx, str(p, "query"), str(p, "start"), str(p, "end"), str(p, "step"), str(p, "timeout"))
}

func (c *Client) handleLabels(ctx context.Context, p map[string]any) (json.RawMessage, error) {
	return c.Labels(ctx, str(p, "start"), str(p, "end"), strSlice(p, "match[]"))
}

func (c *Client) handleLabelValues(ctx context.Context, p map[string]any) (json.RawMessage, error) {
	return c.LabelValues(ctx, str(p, "label"), str(p, "start"), str(p, "end"), strSlice(p, "match[]"))
}

func (c *Client) handleSeries(ctx context.Context, p map[string]any) (json.RawMessage, error) {
	return c.Series(ctx, strSlice(p, "match[]"), str(p, "start"), str(p, "end"))
}

func (c *Client) handleAlerts(ctx context.Context, _ map[string]any) (json.RawMessage, error) {
	return c.Alerts(ctx)
}

// str extracts a string param; returns "" for missing or wrong-type entries.
// The Prometheus HTTP API returns 4xx with a clear message for missing
// required params, so we don't need to validate here — Prometheus will.
func str(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	s, _ := p[key].(string)
	return s
}

// strSlice extracts a []string from a param that could be a Go []string,
// []any of strings, or a single string (which we promote to a one-element slice).
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
