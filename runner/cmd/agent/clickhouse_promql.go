package main

import (
	"strings"

	chclient "github.com/nudgebee/nudgebee-agent/pkg/clickhouse"
	"github.com/nudgebee/nudgebee-agent/pkg/config"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

// Values of the health report's prometheusBackend.
const (
	promBackendPrometheus = "prometheus"
	promBackendClickHouse = "clickhouse"
)

// clickhousePromQLURL is the URL of ClickHouse's Prometheus query API
// (prometheus_api_v1) that PromQL falls back to, or "" when it should not:
// a Prometheus is configured or was discovered at startup, the chart did not
// set CLICKHOUSE_PROMQL_PATH, or ClickHouse is not configured.
func clickhousePromQLURL(cfg *config.Config) string {
	if cfg.PrometheusURL != "" || cfg.ClickHousePromQLPath == "" || !cfg.ClickHouseEnabled {
		return ""
	}
	c := chclient.New(chclient.Config{Host: cfg.ClickHouseHost, Port: cfg.ClickHousePort, SSLEnabled: cfg.ClickHouseSSL})
	if c == nil {
		return ""
	}
	return c.BaseURL + "/" + strings.Trim(cfg.ClickHousePromQLPath, "/")
}

// useClickHousePromQL points the client at chURL with basic auth as the
// ClickHouse user. Its configured authorizer stays in place for any
// Prometheus that SetURL later points it at.
func useClickHousePromQL(c *prometheus.Client, chURL, user, password string) {
	c.Auth = prometheus.ForURL(chURL, prometheus.NewBasicAuth(user, password), c.Auth)
	c.SetURL(chURL)
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
