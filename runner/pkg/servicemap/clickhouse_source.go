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
// and container metrics come from here, read straight from the raw
// otel_metrics_* tables the OTel collector writes (as deltas, via its
// cumulative_to_delta processor); the kube_* state still comes from
// Prometheus.
//
// The ClickHouse must hold this cluster's metrics only: unlike the PromQL
// path there is no cluster label to filter on, because the collector stamps
// none. The chart's bundled ClickHouse is per cluster.

const (
	rawSumTable       = "otel_metrics_sum"
	rawHistogramTable = "otel_metrics_histogram"
	rawGaugeTable     = "otel_metrics_gauge"
	// coverageMetric is scraped from every node agent on every scrape; its
	// rows show which part of the window ClickHouse holds.
	coverageMetric = "container_net_tcp_bytes_sent_total"
	// coverageSlack is the tail of the window that may still be in flight
	// (collector batching, inserts); coverageBucket is the gap the coverage
	// check can see.
	coverageSlack  = 5 * time.Minute
	coverageBucket = 5 * time.Minute
	// coverageAgentDrop: a bucket with fewer node agents than this share of
	// the window's median means some agents stopped being scraped mid-window.
	coverageAgentDrop = 0.75

	// Only delta rows are summed: a cumulative row (cumulative_to_delta
	// missing, or another pipeline writing the same names) would add up
	// running totals and inflate rates by orders of magnitude.
	deltaCounters   = "AggregationTemporality = 1 AND IsMonotonic = 1"
	deltaHistograms = "AggregationTemporality = 1"

	// maxQuerySeconds caps every query; querySettings lowers it to what is
	// left of the caller's budget. A query that hits it fails and Build falls
	// back to Prometheus instead of hanging.
	maxQuerySeconds = 60
	maxQueryMemory  = 2000000000
)

// querySettings bounds a query by the time left in ctx (at most
// maxQuerySeconds), so ClickHouse stops working once the caller has moved on.
func querySettings(ctx context.Context) string {
	limit := maxQuerySeconds
	if dl, ok := ctx.Deadline(); ok {
		if left := int(math.Ceil(time.Until(dl).Seconds())); left < limit {
			limit = max(left, 1)
		}
	}
	return fmt.Sprintf("SETTINGS max_execution_time = %d, max_memory_usage = %d", limit, maxQueryMemory)
}

// tcpEdgeMetrics maps each TCP edge key build() reads to the counter that
// feeds it — the same counters edgeMetrics wraps in rate().
var tcpEdgeMetrics = map[string]string{
	"container_net_tcp_successful_connects": "container_net_tcp_successful_connects_total",
	"container_net_tcp_bytes_sent":          "container_net_tcp_bytes_sent_total",
	"container_net_tcp_bytes_received":      "container_net_tcp_bytes_received_total",
}

// counterMetrics and latencyMetrics are generated from the same tables
// queries.go uses, so a protocol added there is read here too.
func counterMetrics() []string {
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

func latencyMetrics() []string {
	out := make([]string, 0, len(l7Protocols))
	for _, p := range l7Protocols {
		if p.latency != "" {
			out = append(out, p.latency)
		}
	}
	sort.Strings(out)
	return out
}

// edgeLabels are the labels an edge is keyed on, in edgeGroupBy order.
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

// attrColumns selects each edge label out of Attributes under its own name,
// so WHERE and GROUP BY can use the plain names.
func attrColumns() string {
	cols := make([]string, len(edgeLabels))
	for i, l := range edgeLabels {
		cols[i] = fmt.Sprintf("Attributes[%s] AS %s", sqlString(l), l)
	}
	return strings.Join(cols, ", ")
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
	if v == nil {
		return "" // a NULL must not become a "<nil>" workload name
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// errNotCovered means ClickHouse does not hold node-agent metrics for the
// whole requested window from every node agent — the collector pipeline is
// off, was enabled recently, had an outage, or does not scrape every node
// agent. Serving a map built from part of the data would make the knowledge
// graph retire edges that still exist.
var errNotCovered = errors.New("servicemap: clickhouse does not hold node-agent metrics for the whole window")

// filterSQL is the ClickHouse form of the src/dst filters Build passes to
// ApplicationQueries: the same label → value maps, matched as anchored RE2
// regexes like PromQL's =~ (with % meaning .*, as dictToPrometheusFilter does).
// It refers to the label aliases attrColumns defines.
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
// (start, end]. It fails with errNotCovered rather than return a map built
// from part of the window.
//
// nodeAgents, when set, returns how many node agents run now and started
// before a given time; the last bucket must hold that many.
func fetchClickHouse(ctx context.Context, c *chclient.Client, start, end time.Time, p FilterParams, nodeAgents NodeAgentCounter) (map[string][]promResult, error) {
	if c == nil {
		return nil, errors.New("servicemap: clickhouse client not configured")
	}
	db := c.Database
	window := end.Sub(start).Seconds()
	if window <= 0 {
		return nil, errors.New("servicemap: empty window")
	}
	where := fmt.Sprintf("TimeUnix > toDateTime64(%d, 9) AND TimeUnix <= toDateTime64(%d, 9)", start.Unix(), end.Unix())
	by := strings.Join(edgeLabels, ", ")

	if err := checkCoverage(ctx, c, start, end, nodeAgents); err != nil {
		return nil, err
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
	edgeRow := func(r []any) map[string]string {
		labels := make(map[string]string, len(edgeLabels)+1)
		for i, l := range edgeLabels {
			labels[l] = text(r[1+i])
		}
		return labels
	}

	// 1. Edge traffic: requests by status and TCP, as rates over the window —
	// sum by (edge, status) (rate(...)) > 0 in PromQL.
	rows, err := chRows(ctx, c, fmt.Sprintf(`SELECT MetricName, %s, Attributes['status'] AS status, sum(Value)
		FROM %s WHERE MetricName IN (%s) AND %s AND %s%s
		GROUP BY MetricName, %s, status
		HAVING sum(Value) > 0 %s`,
		attrColumns(), table(db, rawSumTable), sqlList(counterMetrics()), deltaCounters, where, filterSQL(p), by, querySettings(ctx)))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if len(r) != len(edgeLabels)+3 {
			continue
		}
		metric, labels := text(r[0]), edgeRow(r)
		rate := num(r[len(r)-1]) / window
		switch {
		case requestKey[metric] != "":
			labels["status"] = text(r[1+len(edgeLabels)])
			out[requestKey[metric]] = append(out[requestKey[metric]], promResult{Metric: labels, Last: rate, HasVal: true})
		case tcpKey[metric] != "":
			out[tcpKey[metric]] = append(out[tcpKey[metric]], promResult{Metric: labels, Last: rate, HasVal: true})
		}
	}

	// 2. Latency: each series' mean, then the slowest series per edge — the
	// same max by (edge) (rate(_sum) / rate(_count)) PromQL computes. A series
	// is one label set from one node agent.
	rows, err = chRows(ctx, c, fmt.Sprintf(`SELECT MetricName, %s, max(series_mean)
		FROM (
		  SELECT MetricName, %s, sum(Sum) / sum(Count) AS series_mean
		  FROM %s WHERE MetricName IN (%s) AND %s AND %s%s
		  GROUP BY MetricName, Attributes, ResourceAttributes['service.instance.id']
		  HAVING sum(Count) > 0
		)
		GROUP BY MetricName, %s
		HAVING max(series_mean) > 0 %s`,
		by, attrColumns(), table(db, rawHistogramTable), sqlList(latencyMetrics()), deltaHistograms, where, filterSQL(p), by, querySettings(ctx)))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if len(r) != len(edgeLabels)+2 {
			continue
		}
		if key := latencyKey[text(r[0])]; key != "" {
			out[key] = append(out[key], promResult{Metric: edgeRow(r), Last: num(r[len(r)-1]), HasVal: true})
		}
	}

	// 3. Container health and pod → workload, one row per series as PromQL
	// returns them. These stay cluster-wide under a workload filter, like
	// nodeQueries. The union is wrapped so its SETTINGS bind to the whole
	// query, not just the last SELECT; the outer * reads the union's columns
	// by name, so the first SELECT names every column uniquely.
	rows, err = chRows(ctx, c, fmt.Sprintf(`SELECT * FROM (
		SELECT multiIf(MetricName = 'container_oom_kills_total', 'container_oom_kills_total',
		               MetricName = 'container_restarts_total', 'container_restarts',
		               'container_throttled_time') AS key,
		       Attributes['container_id'] AS cid, '' AS kind, '' AS name, '' AS ns,
		       if(MetricName = 'container_resources_cpu_throttled_seconds_total', sum(Value) / %g, sum(Value)) AS value
		FROM %s
		WHERE MetricName IN ('container_oom_kills_total', 'container_restarts_total', 'container_resources_cpu_throttled_seconds_total') AND %s AND %s
		GROUP BY MetricName, Attributes
		UNION ALL
		SELECT if(MetricName = 'container_resources_disk_size_bytes', 'container_volume_size', 'container_volume_used'),
		       Attributes['container_id'], '', '', '', argMax(Value, TimeUnix)
		FROM %s
		WHERE MetricName IN ('container_resources_disk_size_bytes', 'container_resources_disk_used_bytes') AND %s
		GROUP BY MetricName, Attributes
		UNION ALL
		SELECT 'pod_workload', Attributes['container_id'] AS container_id, Attributes['src_workload_kind'] AS src_workload_kind,
		       Attributes['src_workload_name'] AS src_workload_name, Attributes['src_workload_namespace'] AS src_workload_namespace, 1
		FROM %s
		WHERE MetricName = 'container_net_tcp_bytes_sent_total' AND %s AND %s
		GROUP BY container_id, src_workload_kind, src_workload_name, src_workload_namespace
		) %s`,
		window, table(db, rawSumTable), deltaCounters, where, table(db, rawGaugeTable), where,
		table(db, rawSumTable), deltaCounters, where, querySettings(ctx)))
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

// NodeAgentCounter reports how many node agents are running and started
// before the given time — the agents the collector should be scraping.
type NodeAgentCounter func(ctx context.Context, startedBefore time.Time) (int, error)

// checkCoverage passes only when ClickHouse holds node-agent rows for every
// coverageBucket fully inside the window (no collector outage), every bucket
// has close to the window's usual number of node agents (none dropped out
// mid-window), and — when nodeAgents is set — the last bucket holds every
// node agent that was already running (none never scraped).
func checkCoverage(ctx context.Context, c *chclient.Client, start, end time.Time, nodeAgents NodeAgentCounter) error {
	rows, err := chRows(ctx, c, fmt.Sprintf(`SELECT toUnixTimestamp(toStartOfInterval(TimeUnix, INTERVAL %d SECOND)) AS bucket,
		       uniqExact(ResourceAttributes['service.instance.id']) AS agents
		FROM %s
		WHERE MetricName = %s AND %s AND TimeUnix > toDateTime64(%d, 9) AND TimeUnix <= toDateTime64(%d, 9)
		GROUP BY bucket %s`,
		int(coverageBucket.Seconds()), table(c.Database, rawSumTable), sqlString(coverageMetric), deltaCounters,
		start.Unix(), end.Unix(), querySettings(ctx)))
	if err != nil {
		return err
	}
	agents := map[int64]float64{}
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		b, n := num(r[0]), num(r[1])
		if math.IsNaN(b) || math.IsNaN(n) {
			continue // unreadable: counts as missing, so the check fails closed
		}
		agents[int64(b)] = n
	}

	// Buckets that lie fully inside (start, end - coverageSlack].
	var required []int64
	first := start.Truncate(coverageBucket)
	if first.Before(start) {
		first = first.Add(coverageBucket)
	}
	for b := first; !b.Add(coverageBucket).After(end.Add(-coverageSlack)); b = b.Add(coverageBucket) {
		required = append(required, b.Unix())
	}
	if len(required) == 0 {
		if len(agents) == 0 {
			return fmt.Errorf("%w: no node-agent rows", errNotCovered)
		}
		return nil
	}

	counts := make([]float64, len(required))
	for i, b := range required {
		n, ok := agents[b]
		if !ok || n == 0 {
			return fmt.Errorf("%w: no node-agent rows for %s", errNotCovered, time.Unix(b, 0).UTC().Format(time.RFC3339))
		}
		counts[i] = n
	}
	sorted := append([]float64(nil), counts...)
	sort.Float64s(sorted)
	median := sorted[len(sorted)/2]
	for i, n := range counts {
		if n < coverageAgentDrop*median {
			return fmt.Errorf("%w: %.0f node agents at %s, usually %.0f", errNotCovered, n,
				time.Unix(required[i], 0).UTC().Format(time.RFC3339), median)
		}
	}

	if nodeAgents != nil {
		lastBucket := time.Unix(required[len(required)-1], 0)
		want, err := nodeAgents(ctx, lastBucket)
		if err != nil {
			return fmt.Errorf("%w: cannot count node agents: %v", errNotCovered, err)
		}
		if got := counts[len(counts)-1]; got < float64(want) {
			return fmt.Errorf("%w: %.0f of %d node agents in ClickHouse at %s", errNotCovered, got, want,
				lastBucket.UTC().Format(time.RFC3339))
		}
	}
	return nil
}
