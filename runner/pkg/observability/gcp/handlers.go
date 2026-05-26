package gcp

import (
	"context"
	"encoding/json"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Handlers wires the GCP primitive actions. defaultProjectID is used when
// the action params omit `project_id` (most callers).
func Handlers(c *Client, defaultProjectID string) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"gke_logs": func(ctx context.Context, p map[string]any) (any, error) {
			project := strOrDefault(p, "project_id", defaultProjectID)
			limit := 0
			if v, ok := p["limit"].(float64); ok {
				limit = int(v)
			}
			raw, err := c.FetchNodePoolLogs(ctx, project, str(p, "zone"), limit)
			if err != nil {
				return nil, err
			}
			return raw, nil
		},
		"gke_traces": func(ctx context.Context, p map[string]any) (any, error) {
			project := strOrDefault(p, "project_id", defaultProjectID)
			raw, err := c.QueryBigQuery(ctx, project, str(p, "query"))
			if err != nil {
				return nil, err
			}
			return raw, nil
		},
	}
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

func strOrDefault(m map[string]any, k, fallback string) string {
	if v := str(m, k); v != "" {
		return v
	}
	return fallback
}

// MustEncodeJSON keeps the import set tidy in case future handlers need it.
var _ = json.RawMessage(nil)
