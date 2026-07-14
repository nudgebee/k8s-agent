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

// PrometheusRetention queries /api/v1/status/flags and returns the retention
// value. It reads Prometheus' `storage.tsdb.retention.time`, then the compat
// `retentionTime`, then VictoriaMetrics' `-retentionPeriod` (vmsingle exposes
// its flags under CLI-style keys). When the endpoint is unavailable (vmsingle
// 404s it) or none of those keys are present, it falls back to the caller-
// supplied value (PROMETHEUS_RETENTION_TIME). Empty string when nothing
// resolves.
func PrometheusRetention(ctx context.Context, c *prometheus.Client, fallback string, logger *slog.Logger) string {
	if c == nil || c.BaseURL == "" {
		return fallback
	}
	raw, err := c.Flags(ctx)
	if err != nil {
		logger.Debug("prometheus flags probe failed; using retention fallback", "err", err)
		return fallback
	}
	var resp prometheusFlagsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fallback
	}
	for _, k := range []string{"storage.tsdb.retention.time", "retentionTime", "-retentionPeriod"} {
		if v := resp.Data[k]; v != "" {
			return v
		}
	}
	return fallback
}
