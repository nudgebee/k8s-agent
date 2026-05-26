package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

// PrometheusFlagsResponse is the subset of `/api/v1/status/flags` we care
// about. Prometheus returns `{status, data: {<flag-name>: <value>, ...}}`;
// we only read `storage.tsdb.retention.time` (Prom 2.x) and `retentionTime`
// (some compat backends expose the legacy name).
type prometheusFlagsResponse struct {
	Status string            `json:"status"`
	Data   map[string]string `json:"data"`
}

// PrometheusRetention queries /api/v1/status/flags and returns the
// `storage.tsdb.retention.time` value. Empty string on any error.
// VictoriaMetrics' vmsingle returns 404 for this endpoint — caller must
// tolerate the empty return.
func PrometheusRetention(ctx context.Context, c *prometheus.Client, logger *slog.Logger) string {
	if c == nil || c.BaseURL == "" {
		return ""
	}
	raw, err := c.Flags(ctx)
	if err != nil {
		logger.Debug("prometheus flags probe failed", "err", err)
		return ""
	}
	var resp prometheusFlagsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ""
	}
	if v := resp.Data["storage.tsdb.retention.time"]; v != "" {
		return v
	}
	if v := resp.Data["retentionTime"]; v != "" {
		return v
	}
	return ""
}
