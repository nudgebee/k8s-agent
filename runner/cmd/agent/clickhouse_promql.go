package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
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
// series of this cluster (its cluster label, when set), so a handler over an
// empty table, one with no collector writing to it, or one holding only
// another cluster's data, is not taken for a working backend. The table has
// no scrape-made `up` series: the collector remote-writes node-agent's own
// metrics only.
func clickhousePromQLProbe(cluster string) string {
	return "count(container_net_tcp_bytes_sent_total" + clusterSelector(cluster) + ")"
}

// clusterSelector selects the TimeSeries data's cluster label, or is "" with
// none.
func clusterSelector(cluster string) string {
	if cluster == "" {
		return ""
	}
	return "{cluster=" + strconv.Quote(cluster) + "}"
}

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

// clickhousePromQLRequests gives requests to chURL basic auth as the
// ClickHouse user and query limits (clickhouseQueryLimits), and leaves the
// client's configured authorizer for any Prometheus that SetURL points it at
// later. Set once, at startup: the URL changes at runtime, the authorizer
// does not.
func clickhousePromQLRequests(c *prometheus.Client, chURL, user, password string) {
	var maxWait time.Duration
	if c.HTTP != nil {
		maxWait = c.HTTP.Timeout
	}
	limits := &clickhouseQueryLimits{auth: prometheus.NewBasicAuth(user, password), maxWait: maxWait}
	c.Auth = prometheus.ForURL(chURL, limits, c.Auth)
}

// clickhousePromQLMaxMemory caps one PromQL query's memory in ClickHouse
// (max_memory_usage), so a broad query cannot take the memory that inserts
// and the other queries share (the chart's pod limit is 2000Mi).
const clickhousePromQLMaxMemory = 1_000_000_000

// clickhouseQueryLimits bounds each PromQL query ClickHouse runs. ClickHouse
// takes the Prometheus API's `timeout` for a setting it does not know and
// fails the query, so it is turned into max_execution_time. ClickHouse runs a
// PromQL query to the end even after the client has gone (it ignores
// cancel_http_readonly_queries_on_client_close there), so max_execution_time
// is also kept within the caller's deadline and the HTTP client's timeout:
// a query no one waits for any more stops. Then the auth is applied.
type clickhouseQueryLimits struct {
	auth    prometheus.Authorizer
	maxWait time.Duration // the HTTP client's timeout; 0 for none
}

func (l *clickhouseQueryLimits) Apply(ctx context.Context, req *http.Request) error {
	q := req.URL.Query()
	wait := l.maxWait
	if t, ok := parsePromDuration(q.Get("timeout")); ok && (wait == 0 || t < wait) {
		wait = t
	}
	if dl, ok := ctx.Deadline(); ok {
		// A deadline already passed still gets the shortest limit, not none.
		if left := max(time.Until(dl), time.Nanosecond); wait == 0 || left < wait {
			wait = left
		}
	}
	q.Del("timeout")
	if wait > 0 {
		// Whole seconds, at least 1: 0 would mean no limit.
		q.Set("max_execution_time", strconv.FormatInt(max(1, int64(math.Ceil(wait.Seconds()))), 10))
	}
	q.Set("max_memory_usage", strconv.Itoa(clickhousePromQLMaxMemory))
	req.URL.RawQuery = q.Encode()
	return l.auth.Apply(ctx, req)
}

// maxPromTimeout is the longest `timeout` parsePromDuration accepts; a longer
// one is ignored (the HTTP client's timeout applies), which also keeps a huge
// number of seconds from overflowing time.Duration.
const maxPromTimeout = 24 * time.Hour

// parsePromDuration parses a Prometheus API duration: a Go duration ("30s",
// "1m30s") or a number of seconds ("30", "1.5"). It reports false for
// anything not positive or longer than maxPromTimeout, and rounds a positive
// value below 1ns up, so that a parsed timeout always sets a limit.
func parsePromDuration(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 || d > maxPromTimeout {
			return 0, false
		}
		return d, true
	}
	// f > 0 is false for NaN; f <= the limit is false for +Inf.
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 && f <= maxPromTimeout.Seconds() {
		return max(time.Nanosecond, time.Duration(f*float64(time.Second))), true
	}
	return 0, false
}

// errNoNodeAgentSeries is the probe's reason when the query works but the
// table holds no node-agent data.
var errNoNodeAgentSeries = errors.New("no node-agent series")

// probeClickHousePromQL returns nil when ClickHouse's Prometheus query API at
// chURL answers the probe with at least one series, else why not. It uses the
// client's HTTP client, headers and authorizer.
func probeClickHousePromQL(ctx context.Context, c *prometheus.Client, chURL, cluster string) error {
	p := prometheus.New(chURL, c.HTTP)
	p.ExtraHeaders, p.Auth = c.ExtraHeaders, c.Auth
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := p.Query(cctx, clickhousePromQLProbe(cluster), "", "")
	if err != nil {
		return err
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if resp.Status != "success" {
		return errors.New("query status " + resp.Status)
	}
	if len(resp.Data.Result) == 0 {
		return errNoNodeAgentSeries
	}
	return nil
}

// watchClickHousePromQL probes ClickHouse's Prometheus query API now and then
// every interval, and points the client at it once the probe passes, but only
// while the client has no URL: a Prometheus that discovery finds first keeps
// it, and one found later replaces ClickHouse. After an upgrade the handler
// and table may appear only after the runner started, hence the re-probe.
// The first failed probe is logged at Info with its reason, later ones at
// Debug.
func watchClickHousePromQL(ctx context.Context, c *prometheus.Client, chURL, cluster string, interval time.Duration, logger *slog.Logger) {
	logged := false
	for {
		if c.URL() != "" {
			return
		}
		err := probeClickHousePromQL(ctx, c, chURL, cluster)
		switch {
		case err == nil:
			if c.SetURLIfEmpty(chURL) {
				logger.Info("prometheus not found; PromQL goes to ClickHouse until one is discovered", "url", redact.URL(chURL))
			}
			return
		case !logged:
			logger.Info("ClickHouse PromQL fallback not available yet; will retry", "reason", err.Error(), "url", redact.URL(chURL), "retry_interval", interval)
			logged = true
		default:
			logger.Debug("ClickHouse PromQL fallback not available yet", "reason", err.Error())
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
// does not, so there each node agent is one instance of its own metrics, of
// this cluster when the data carries a cluster label.
func nodeAgentCountQuery(backend, cluster string) string {
	if backend == promBackendClickHouse {
		return `count by (instance) (container_net_tcp_bytes_sent_total` + clusterSelector(cluster) + `)`
	}
	return `up{job=~"(.+/)?nudgebee(-.*)?-node-agent"}`
}
