package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	chclient "github.com/nudgebee/nudgebee-agent/pkg/clickhouse"
	"github.com/nudgebee/nudgebee-agent/pkg/config"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
	"github.com/nudgebee/nudgebee-agent/pkg/redact"
)

// Values of the health report's prometheusBackend.
const (
	promBackendPrometheus = "prometheus"
	promBackendClickHouse = "clickhouse"
)

// clickhousePromQLPath is where the chart serves ClickHouse's Prometheus query
// API (prometheus_api_v1 handler), before /api/v1.
const clickhousePromQLPath = "/prometheus"

// clickhousePromQLProbe passes only when the TimeSeries table holds node-agent
// series, so a handler over an empty table, or one with no collector writing
// to it, is not taken for a working backend. The table has no scrape-made
// `up` series: the collector remote-writes node-agent's own metrics only.
const clickhousePromQLProbe = "count(container_net_tcp_bytes_sent_total)"

// clickhousePromQLURL is the URL of ClickHouse's Prometheus query API that
// PromQL may fall back to, or "" when it cannot: a Prometheus is configured
// or was discovered at startup, or ClickHouse is not configured.
func clickhousePromQLURL(cfg *config.Config) string {
	if cfg.PrometheusURL != "" || !cfg.ClickHouseEnabled {
		return ""
	}
	c := chclient.New(chclient.Config{Host: cfg.ClickHouseHost, Port: cfg.ClickHousePort, SSLEnabled: cfg.ClickHouseSSL})
	if c == nil {
		return ""
	}
	return c.BaseURL + clickhousePromQLPath
}

// clickhousePromQLAuth gives requests to chURL basic auth as the ClickHouse
// user and leaves the client's configured authorizer for any Prometheus that
// SetURL points it at later. Set once, at startup: the URL changes at
// runtime, the authorizer does not.
func clickhousePromQLAuth(c *prometheus.Client, chURL, user, password string) {
	c.Auth = prometheus.ForURL(chURL, prometheus.NewBasicAuth(user, password), c.Auth)
}

// probeClickHousePromQL reports whether ClickHouse's Prometheus query API at
// chURL answers the probe with at least one series, using the client's HTTP
// client, headers and authorizer.
func probeClickHousePromQL(ctx context.Context, c *prometheus.Client, chURL string) bool {
	p := prometheus.New(chURL, c.HTTP)
	p.ExtraHeaders, p.Auth = c.ExtraHeaders, c.Auth
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := p.Query(cctx, clickhousePromQLProbe, "", "")
	if err != nil {
		return false
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	return json.Unmarshal(raw, &resp) == nil && resp.Status == "success" && len(resp.Data.Result) > 0
}

// watchClickHousePromQL probes ClickHouse's Prometheus query API now and then
// every interval, and points the client at it once the probe passes, but only
// while the client has no URL: a Prometheus that discovery finds first keeps
// it, and one found later replaces ClickHouse. After an upgrade the handler
// and table may appear only after the runner started, hence the re-probe.
func watchClickHousePromQL(ctx context.Context, c *prometheus.Client, chURL string, interval time.Duration, logger *slog.Logger) {
	for {
		if c.URL() != "" {
			return
		}
		if probeClickHousePromQL(ctx, c, chURL) && c.SetURLIfEmpty(chURL) {
			logger.Info("prometheus not found; PromQL goes to ClickHouse until one is discovered", "url", redact.URL(chURL))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// promBackend says where the client's PromQL goes now: "clickhouse" while it
// is on the fallback, "prometheus" once one is configured or discovered, ""
// with neither.
func promBackend(c *prometheus.Client, chURL string) string {
	switch u := c.URL(); {
	case u == "":
		return ""
	case chURL != "" && u == chURL:
		return promBackendClickHouse
	}
	return promBackendPrometheus
}

// prometheusProxyURL is the upstream for the Grafana /prometheus-v2 proxy:
// the client's URL, but "" (the proxy answers 503) while that is the
// ClickHouse fallback, as the proxy sends Grafana's credentials and
// PROMETHEUS_HEADERS, not the client's authorizer.
func prometheusProxyURL(c *prometheus.Client, chURL string) func() string {
	return func() string {
		if promBackend(c, chURL) == promBackendClickHouse {
			return ""
		}
		return c.URL()
	}
}

// nodeAgentCountQuery returns one series per node agent on the given backend.
// A Prometheus has the scrape-made `up` series; ClickHouse's TimeSeries table
// does not, so there each node agent is one instance of its own metrics.
func nodeAgentCountQuery(backend string) string {
	if backend == promBackendClickHouse {
		return `count by (instance) (container_net_tcp_bytes_sent_total)`
	}
	return `up{job=~"(.+/)?nudgebee(-.*)?-node-agent"}`
}
