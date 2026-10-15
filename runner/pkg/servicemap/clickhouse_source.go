package servicemap

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	chclient "github.com/nudgebee/nudgebee-agent/pkg/clickhouse"
)

// The ClickHouse source builds the same map[string][]promResult that the
// PromQL fetch produces, so build() and render() — and everything downstream
// of the service_map action — cannot tell the two apart. Only the eBPF edge
// and container metrics come from here; the kube_* state still comes from
// Prometheus.
//
// Edges are read from a per-minute rollup the runner creates itself, not from
// the raw otel_metrics_* tables: a 24h map over raw rows reads ~160M rows on a
// 20-node cluster, the rollup ~8M.

// rollupTable holds one row per minute per edge, metric and status. The name
// is versioned: changing its columns, the views' metric lists or their
// filters means a new version, never an ALTER, because CREATE ... IF NOT
// EXISTS cannot update an existing view.
const (
	rollupTable             = "nb_service_map_v1"
	rollupCountersView      = "nb_service_map_v1_counters_mv"
	rollupLatencyView       = "nb_service_map_v1_latency_mv"
	rawSumTable             = "otel_metrics_sum"
	rawHistogramTable       = "otel_metrics_histogram"
	rawGaugeTable           = "otel_metrics_gauge"
	rollupCoverageSlack     = 5 * time.Minute
	clickhouseQuerySettings = "SETTINGS max_execution_time = 30, max_memory_usage = 2000000000"
)

// tcpEdgeMetrics maps each TCP edge key build() reads to the counter that
// feeds it — the same counters edgeMetrics wraps in rate().
var tcpEdgeMetrics = map[string]string{
	"container_net_tcp_successful_connects": "container_net_tcp_successful_connects_total",
	"container_net_tcp_bytes_sent":          "container_net_tcp_bytes_sent_total",
	"container_net_tcp_bytes_received":      "container_net_tcp_bytes_received_total",
}

// rollupCounterMetrics and rollupLatencyMetrics are generated from the same
// tables queries.go uses, so a protocol added there is rolled up here too.
func rollupCounterMetrics() []string {
	out := make([]string, 0, len(tcpEdgeMetrics)+len(l7Protocols))
	for _, m := range tcpEdgeMetrics {
		out = append(out, m)
	}
	for _, p := range l7Protocols {
		out = append(out, p.requests)
	}
	sort.Strings(out)
	return out
}

func rollupLatencyMetrics() []string {
	out := make([]string, 0, len(l7Protocols))
	for _, p := range l7Protocols {
		if p.latency != "" {
			out = append(out, p.latency)
		}
	}
	sort.Strings(out)
	return out
}

// edgeLabels are the edge columns of the rollup, in edgeGroupBy order.
var edgeLabels = strings.Split(strings.ReplaceAll(edgeGroupBy, " ", ""), ",")

func sqlList(values []string) string {
	q := make([]string, len(values))
	for i, v := range values {
		q[i] = sqlString(v)
	}
	return strings.Join(q, ", ")
}

// sqlString quotes s as a ClickHouse string literal.
func sqlString(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
}

func ident(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func table(db, name string) string { return ident(db) + "." + ident(name) }

func attrColumns() string {
	cols := make([]string, len(edgeLabels))
	for i, l := range edgeLabels {
		cols[i] = fmt.Sprintf("Attributes[%s] AS %s", sqlString(l), l)
	}
	return strings.Join(cols, ", ")
}

// rollupDDL returns the statements that create the rollup and the two views
// feeding it. The views read only MetricName, Attributes, TimeUnix and the
// value columns: a materialized view that fails also fails the insert into
// its source table, so it must not depend on columns the collector's
// exporter has renamed between versions. Values must be deltas — the
// collector's cumulative_to_delta processor provides that.
func rollupDDL(db string) []string {
	labelCols := make([]string, len(edgeLabels))
	for i, l := range edgeLabels {
		typ := "LowCardinality(String)"
		if l == "destination_workload_name" || l == "destination_ip" {
			typ = "String" // external hosts and IPs: too many distinct values
		}
		labelCols[i] = fmt.Sprintf("    %s %s", l, typ)
	}
	orderBy := append([]string{"minute", "metric"}, edgeLabels...)
	orderBy = append(orderBy, "status")
	by := strings.Join(edgeLabels, ", ")

	createTable := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
    minute DateTime CODEC(Delta, ZSTD(1)),
    metric LowCardinality(String),
%s,
    status LowCardinality(String),
    value Float64,
    lat_sum Float64,
    lat_count UInt64
) ENGINE = SummingMergeTree((value, lat_sum, lat_count))
PARTITION BY toDate(minute)
ORDER BY (%s)
TTL minute + INTERVAL 7 DAY
SETTINGS ttl_only_drop_parts = 1`, table(db, rollupTable), strings.Join(labelCols, ",\n"), strings.Join(orderBy, ", "))

	counters := fmt.Sprintf(`CREATE MATERIALIZED VIEW IF NOT EXISTS %s TO %s AS
SELECT toStartOfMinute(TimeUnix) AS minute, MetricName AS metric, %s, Attributes['status'] AS status,
       sum(Value) AS value, toFloat64(0) AS lat_sum, toUInt64(0) AS lat_count
FROM %s
WHERE MetricName IN (%s)
GROUP BY minute, metric, %s, status`,
		table(db, rollupCountersView), table(db, rollupTable), attrColumns(), table(db, rawSumTable),
		sqlList(rollupCounterMetrics()), by)

	latency := fmt.Sprintf(`CREATE MATERIALIZED VIEW IF NOT EXISTS %s TO %s AS
SELECT toStartOfMinute(TimeUnix) AS minute, MetricName AS metric, %s, Attributes['status'] AS status,
       toFloat64(0) AS value, sum(Sum) AS lat_sum, toUInt64(sum(Count)) AS lat_count
FROM %s
WHERE MetricName IN (%s)
GROUP BY minute, metric, %s, status`,
		table(db, rollupLatencyView), table(db, rollupTable), attrColumns(), table(db, rawHistogramTable),
		sqlList(rollupLatencyMetrics()), by)

	return []string{createTable, counters, latency}
}

// ensureRollup creates the rollup and its views once the collector has
// created the raw tables they read; it reports false while those are absent
// so the caller retries. Safe to run repeatedly and from several replicas.
func ensureRollup(ctx context.Context, c *chclient.Client) (bool, error) {
	for _, src := range []string{rawSumTable, rawHistogramTable} {
		rows, err := chRows(ctx, c, fmt.Sprintf("EXISTS TABLE %s", table(c.Database, src)))
		if err != nil {
			return false, err
		}
		if len(rows) == 0 || len(rows[0]) == 0 || num(rows[0][0]) != 1 {
			return false, nil
		}
	}
	for _, stmt := range rollupDDL(c.Database) {
		if _, err := chRows(ctx, c, stmt); err != nil {
			return false, err
		}
	}
	return true, nil
}

func chRows(ctx context.Context, c *chclient.Client, sql string) ([][]any, error) {
	res, err := c.Query(ctx, sql, nil)
	if err != nil {
		return nil, err
	}
	if res.Error != nil {
		return nil, errors.New(*res.Error)
	}
	return res.Data, nil
}

// num reads a JSONCompact number; ClickHouse quotes 64-bit integers.
func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err != nil {
			return math.NaN()
		}
		return f
	}
	return math.NaN()
}

func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// errNotCovered means the rollup does not hold the whole requested window —
// the view was created recently, or the collector stopped writing. Serving a
// partial map would make the knowledge graph retire edges that still exist.
var errNotCovered = errors.New("servicemap: clickhouse rollup does not cover the window")

// filterSQL is the ClickHouse form of the src/dst filters Build passes to
// ApplicationQueries: the same label → value maps, matched as anchored RE2
// regexes like PromQL's =~ (with % meaning .*, as dictToPrometheusFilter does).
func filterSQL(p FilterParams) string {
	var src, dst map[string]string
	switch {
	case p.WorkloadName != "":
		src = map[string]string{"src_workload_name": p.WorkloadName, "src_workload_namespace": p.WorkloadNamespace}
		dst = map[string]string{"destination_workload_name": p.WorkloadName, "destination_workload_namespace": p.WorkloadNamespace}
	case p.WorkloadNamespace != "":
		src = map[string]string{"src_workload_namespace": p.WorkloadNamespace}
		dst = map[string]string{"destination_workload_namespace": p.WorkloadNamespace}
	default:
		return ""
	}
	side := func(m map[string]string) string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		conds := make([]string, len(keys))
		for i, k := range keys {
			re := "^(?:" + strings.ReplaceAll(m[k], "%", ".*") + ")$"
			conds[i] = fmt.Sprintf("match(%s, %s)", k, sqlString(re))
		}
		return "(" + strings.Join(conds, " AND ") + ")"
	}
	return " AND (" + side(src) + " OR " + side(dst) + ")"
}

// fetchClickHouse returns every non-kube_* key build() reads, for the window
// [start, end). It fails with errNotCovered rather than return a map built
// from part of the window.
func fetchClickHouse(ctx context.Context, c *chclient.Client, start, end time.Time, p FilterParams) (map[string][]promResult, error) {
	if c == nil {
		return nil, errors.New("servicemap: clickhouse client not configured")
	}
	db := c.Database
	startMin, endMin := start.Truncate(time.Minute), end.Truncate(time.Minute)
	window := endMin.Sub(startMin).Seconds()
	if window <= 0 {
		return nil, errors.New("servicemap: empty window")
	}

	// Coverage: rows must reach back to the window start and up to its end.
	cov, err := chRows(ctx, c, fmt.Sprintf(`SELECT toUnixTimestamp(min(minute)), toUnixTimestamp(max(minute)), count()
		FROM %s WHERE minute >= toDateTime(%d) %s`,
		table(db, rollupTable), startMin.Add(-rollupCoverageSlack).Unix(), clickhouseQuerySettings))
	if err != nil {
		return nil, err
	}
	if len(cov) == 0 || len(cov[0]) < 3 || num(cov[0][2]) == 0 ||
		time.Unix(int64(num(cov[0][0])), 0).After(startMin.Add(rollupCoverageSlack)) ||
		time.Unix(int64(num(cov[0][1])), 0).Before(endMin.Add(-rollupCoverageSlack)) {
		return nil, errNotCovered
	}

	out := map[string][]promResult{}
	requestKey := map[string]string{}
	latencyKey := map[string]string{}
	for _, pr := range l7Protocols {
		requestKey[pr.requests] = l7RequestsKey(pr)
		if pr.latency != "" {
			latencyKey[pr.latency] = l7LatencyKey(pr)
		}
	}
	tcpKey := map[string]string{}
	for k, m := range tcpEdgeMetrics {
		tcpKey[m] = k
	}

	// 1. Edges: request rates by status, TCP rates, latency sums — one query.
	rows, err := chRows(ctx, c, fmt.Sprintf(`SELECT metric, %s, status, sum(value), sum(lat_sum), sum(lat_count)
		FROM %s WHERE minute >= toDateTime(%d) AND minute < toDateTime(%d)%s
		GROUP BY metric, %s, status %s`,
		strings.Join(edgeLabels, ", "), table(db, rollupTable), startMin.Unix(), endMin.Unix(), filterSQL(p),
		strings.Join(edgeLabels, ", "), clickhouseQuerySettings))
	if err != nil {
		return nil, err
	}
	type latAcc struct {
		labels   map[string]string
		sum, cnt float64
	}
	lat := map[string]map[string]*latAcc{} // build key → edge → accumulator
	for _, r := range rows {
		if len(r) != len(edgeLabels)+5 {
			continue
		}
		metric := text(r[0])
		labels := make(map[string]string, len(edgeLabels)+1)
		for i, l := range edgeLabels {
			labels[l] = text(r[1+i])
		}
		status := text(r[1+len(edgeLabels)])
		value, latSum, latCount := num(r[len(r)-3]), num(r[len(r)-2]), num(r[len(r)-1])

		switch {
		case requestKey[metric] != "":
			if value > 0 {
				labels["status"] = status
				out[requestKey[metric]] = append(out[requestKey[metric]], promResult{Metric: labels, Last: value / window, HasVal: true})
			}
		case tcpKey[metric] != "":
			if value > 0 {
				out[tcpKey[metric]] = append(out[tcpKey[metric]], promResult{Metric: labels, Last: value / window, HasVal: true})
			}
		case latencyKey[metric] != "":
			// Latency rows may split by status; the edge mean spans them all.
			key := latencyKey[metric]
			if lat[key] == nil {
				lat[key] = map[string]*latAcc{}
			}
			edge := strings.Join(rowLabels(labels), "\x00")
			a := lat[key][edge]
			if a == nil {
				a = &latAcc{labels: labels}
				lat[key][edge] = a
			}
			a.sum += latSum
			a.cnt += latCount
		}
	}
	for key, edges := range lat {
		for _, a := range edges {
			if a.cnt > 0 && a.sum > 0 {
				out[key] = append(out[key], promResult{Metric: a.labels, Last: a.sum / a.cnt, HasVal: true})
			}
		}
	}

	// 2. Container health and pod → workload, one row per series as PromQL
	// returns them. These stay cluster-wide under a workload filter, like
	// nodeQueries.
	where := fmt.Sprintf("TimeUnix > toDateTime64(%d, 9) AND TimeUnix <= toDateTime64(%d, 9)", start.Unix(), end.Unix())
	rawWindow := end.Sub(start).Seconds()
	rows, err = chRows(ctx, c, fmt.Sprintf(`
		SELECT multiIf(MetricName = 'container_oom_kills_total', 'container_oom_kills_total',
		               MetricName = 'container_restarts_total', 'container_restarts',
		               'container_throttled_time'),
		       Attributes['container_id'], '', '', '',
		       if(MetricName = 'container_resources_cpu_throttled_seconds_total', sum(Value) / %g, sum(Value))
		FROM %s
		WHERE MetricName IN ('container_oom_kills_total', 'container_restarts_total', 'container_resources_cpu_throttled_seconds_total') AND %s
		GROUP BY MetricName, Attributes
		UNION ALL
		SELECT if(MetricName = 'container_resources_disk_size_bytes', 'container_volume_size', 'container_volume_used'),
		       Attributes['container_id'], '', '', '', argMax(Value, TimeUnix)
		FROM %s
		WHERE MetricName IN ('container_resources_disk_size_bytes', 'container_resources_disk_used_bytes') AND %s
		GROUP BY MetricName, Attributes
		UNION ALL
		SELECT 'pod_workload', Attributes['container_id'], Attributes['src_workload_kind'],
		       Attributes['src_workload_name'], Attributes['src_workload_namespace'], 1
		FROM %s
		WHERE MetricName = 'container_net_tcp_bytes_sent_total' AND %s
		GROUP BY 2, 3, 4, 5
		%s`,
		rawWindow, table(db, rawSumTable), where, table(db, rawGaugeTable), where, table(db, rawSumTable), where,
		clickhouseQuerySettings))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if len(r) != 6 {
			continue
		}
		key := text(r[0])
		labels := map[string]string{"container_id": text(r[1])}
		if key == "pod_workload" {
			labels["src_workload_kind"], labels["src_workload_name"], labels["src_workload_namespace"] = text(r[2]), text(r[3]), text(r[4])
		}
		v := num(r[5])
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out[key] = append(out[key], promResult{Metric: labels, Last: v, HasVal: true})
	}
	return out, nil
}

func rowLabels(l map[string]string) []string {
	out := make([]string, len(edgeLabels))
	for i, k := range edgeLabels {
		out[i] = l[k]
	}
	return out
}
