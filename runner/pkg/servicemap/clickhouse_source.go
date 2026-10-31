package servicemap

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	chclient "github.com/nudgebee/nudgebee-agent/pkg/clickhouse"
)

// The ClickHouse source builds the same map[string][]promResult that the
// PromQL fetch produces, so build() and render() — and everything downstream
// of the service_map action — cannot tell the two apart. Only the eBPF edge
// and container metrics come from here: edges, latency and coverage from the
// per-minute summary tables (summary.go), container → workload from the
// hourly one, container health from the raw
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

// viewsLogTTLRe reads the retention out of the TTL clause of
// system.query_views_log's engine_full, e.g. "event_time + toIntervalDay(8)".
// It must match the whole clause: in "event_time + toIntervalDay(30) TO VOLUME
// 'cold', event_time + toIntervalDay(1)" the first rule only moves rows, and
// the rule that deletes them comes later.
var viewsLogTTLRe = regexp.MustCompile(`^event_(date|time) \+ toInterval(Second|Minute|Hour|Day|Week|Month|Quarter|Year)\((\d+)\)$`)

// viewsLogRetention is how far back system.query_views_log is sure to still
// hold rows, from its engine_full: 0 with ok for no TTL (kept forever), ok
// false for a TTL it cannot read: anything but one plain delete rule, since a
// move or conditional rule may sit before the rule that deletes failure rows.
// Months and years count at their shortest; a TTL on event_date expires a
// whole day at once, so it keeps a day less.
func viewsLogRetention(engineFull string) (keep time.Duration, ok bool) {
	_, ttl, found := strings.Cut(engineFull, " TTL ")
	if !found {
		return 0, true
	}
	ttl, _, _ = strings.Cut(ttl, " SETTINGS ")
	m := viewsLogTTLRe.FindStringSubmatch(ttl)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n <= 0 {
		return 0, false
	}
	unit := map[string]time.Duration{
		"Second": time.Second, "Minute": time.Minute, "Hour": time.Hour, "Day": 24 * time.Hour,
		"Week": 7 * 24 * time.Hour, "Month": 28 * 24 * time.Hour, "Quarter": 89 * 24 * time.Hour, "Year": 365 * 24 * time.Hour,
	}[m[2]]
	keep = time.Duration(n) * unit
	if m[1] == "date" {
		keep -= 24 * time.Hour
	}
	if keep <= 0 {
		return 0, false
	}
	return keep, true
}

// checkViewsLog passes only when system.query_views_log can show every
// summary view failure since start: the log exists (the chart keeps it; an
// operator's own clickhouse.extraOverrides or ClickHouse may not), already
// existed at start (a recreated log lost the failures before it), and its TTL
// has not expired rows from start yet.
func checkViewsLog(ctx context.Context, c *chclient.Client, start time.Time) error {
	rows, err := chRows(ctx, c, fmt.Sprintf(`SELECT engine_full, toUnixTimestamp(metadata_modification_time)
		FROM system.tables WHERE database = 'system' AND name = 'query_views_log' %s`, querySettings(ctx)))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return fmt.Errorf("%w: system.query_views_log is disabled, so summary view failures cannot be checked", errNotCovered)
	}
	if len(rows[0]) != 2 {
		return fmt.Errorf("%w: unreadable system.query_views_log", errNotCovered)
	}
	created := num(rows[0][1])
	if math.IsNaN(created) {
		return fmt.Errorf("%w: unreadable system.query_views_log", errNotCovered)
	}
	if since := time.Unix(int64(created), 0); start.Before(since) {
		return fmt.Errorf("%w: system.query_views_log starts at %s, after the window does", errNotCovered, since.UTC().Format(time.RFC3339))
	}
	keep, ok := viewsLogRetention(text(rows[0][0]))
	if !ok {
		return fmt.Errorf("%w: cannot read the TTL of system.query_views_log", errNotCovered)
	}
	if keep > 0 && start.Before(time.Now().Add(-keep)) {
		return fmt.Errorf("%w: system.query_views_log keeps %s, less than the window goes back", errNotCovered, keep)
	}
	return nil
}

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
	// The summary tables hold whole minutes: edges and latency read the
	// minutes in [from, to), and their rates divide by that span.
	from, to := start.Truncate(time.Minute), end.Truncate(time.Minute)
	minuteWindow := to.Sub(from).Seconds()
	if window <= 0 || minuteWindow <= 0 {
		return nil, errors.New("servicemap: empty window")
	}
	where := fmt.Sprintf("TimeUnix > toDateTime64(%d, 9) AND TimeUnix <= toDateTime64(%d, 9)", start.Unix(), end.Unix())
	minutes := fmt.Sprintf("minute >= toDateTime(%d) AND minute < toDateTime(%d)", from.Unix(), to.Unix())
	by := strings.Join(edgeLabels, ", ")

	if err := checkCoverage(ctx, c, from, to, nodeAgents); err != nil {
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
	rows, err := chRows(ctx, c, fmt.Sprintf(`SELECT MetricName, %s, status, sum(value)
		FROM %s WHERE MetricName IN (%s) AND %s%s
		GROUP BY MetricName, %s, status
		HAVING sum(value) > 0 %s`,
		by, table(db, summaryEdgesTable), sqlList(counterMetrics()), minutes, filterSQL(p), by, querySettings(ctx)))
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if len(r) != len(edgeLabels)+3 {
			continue
		}
		metric, labels := text(r[0]), edgeRow(r)
		rate := num(r[len(r)-1]) / minuteWindow
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
	// is one label set from one node agent; the summary keeps one row per
	// series and minute for exactly this.
	rows, err = chRows(ctx, c, fmt.Sprintf(`SELECT MetricName, %s, max(series_mean)
		FROM (
		  SELECT MetricName, %s, sum(total) / sum(samples) AS series_mean
		  FROM %s WHERE MetricName IN (%s) AND %s%s
		  GROUP BY MetricName, %s, series
		  HAVING sum(samples) > 0
		)
		GROUP BY MetricName, %s
		HAVING max(series_mean) > 0 %s`,
		by, by, table(db, summaryLatencyTable), sqlList(latencyMetrics()), minutes, filterSQL(p), by, by, querySettings(ctx)))
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
		SELECT 'pod_workload', container_id, src_workload_kind, src_workload_name, src_workload_namespace, 1
		FROM %s
		WHERE hour >= toStartOfHour(toDateTime(%d)) AND hour < toDateTime(%d)
		GROUP BY container_id, src_workload_kind, src_workload_name, src_workload_namespace
		) %s`,
		window, table(db, rawSumTable), deltaCounters, where, table(db, rawGaugeTable), where,
		table(db, summaryPodsTable), from.Unix(), to.Unix(), querySettings(ctx)))
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

// checkCoverage passes only when every summary table and MV exists and the
// newest was created before the window (its first minute is partial; a
// recreated table lost everything before it), no MV run failed while the
// window's rows were being inserted (each MV fails on its own under
// materialized_views_ignore_errors, losing only its own table's rows; an MV
// whose table is missing never runs and logs nothing, which the existence
// check covers; checkViewsLog makes sure the log can show it), the agents summary holds node-agent rows for every
// coverageBucket fully inside the window (no collector outage), every node
// agent has rows in every bucket from its first to its last (none stopped
// being scraped mid-window, while one whose node left or joined is fine), and
// — when nodeAgents is set — the last bucket holds every node agent that was
// already running (none never scraped, or stopped and still running).
func checkCoverage(ctx context.Context, c *chclient.Client, start, end time.Time, nodeAgents NodeAgentCounter) error {
	views := summaryViews()
	objects := append(summaryTables(), views...)
	rows, err := chRows(ctx, c, fmt.Sprintf(`SELECT count(), toUnixTimestamp(max(metadata_modification_time))
		FROM system.tables WHERE database = %s AND name IN (%s) %s`,
		sqlString(c.Database), sqlList(objects), querySettings(ctx)))
	if err != nil {
		return err
	}
	if len(rows) != 1 || len(rows[0]) != 2 {
		return fmt.Errorf("%w: unreadable summary tables", errNotCovered)
	}
	if n := num(rows[0][0]); n != float64(len(objects)) {
		return fmt.Errorf("%w: %v of %d summary tables and views exist", errNotCovered, n, len(objects))
	}
	created := num(rows[0][1])
	if math.IsNaN(created) {
		return fmt.Errorf("%w: unreadable summary start", errNotCovered)
	}
	if first := time.Unix(int64(created), 0).Truncate(time.Minute); start.Before(first.Add(time.Minute)) {
		return fmt.Errorf("%w: summary starts at %s, after the window does", errNotCovered, first.UTC().Format(time.RFC3339))
	}

	if err := checkViewsLog(ctx, c, start); err != nil {
		return err
	}

	// A minute's rows are inserted up to coverageSlack after it, so failures
	// logged that long past the window's end still count. The log is flushed
	// every few seconds; a failure in the last seconds lands in the window's
	// tail, which may still be in flight anyway.
	qualified := make([]string, len(views))
	for i, v := range views {
		qualified[i] = c.Database + "." + v
	}
	rows, err = chRows(ctx, c, fmt.Sprintf(`SELECT view_name, count() FROM system.query_views_log
		WHERE event_date >= toDate(toDateTime(%d)) AND event_time >= toDateTime(%d) AND event_time < toDateTime(%d)
		  AND view_name IN (%s) AND status != 'QueryFinish'
		GROUP BY view_name %s`,
		start.Unix(), start.Unix(), end.Add(coverageSlack).Unix(), sqlList(qualified), querySettings(ctx)))
	if err != nil {
		return fmt.Errorf("servicemap: read summary view failures: %w", err)
	}
	if len(rows) > 0 && len(rows[0]) == 2 {
		return fmt.Errorf("%w: summary view %s failed %v times in the window", errNotCovered, text(rows[0][0]), num(rows[0][1]))
	}
	if len(rows) > 0 {
		return fmt.Errorf("%w: unreadable summary view failures", errNotCovered)
	}

	agentsTable := table(c.Database, summaryAgentsTable)
	rows, err = chRows(ctx, c, fmt.Sprintf(`SELECT agent,
		       groupUniqArray(toUnixTimestamp(toStartOfInterval(minute, INTERVAL %d SECOND))) AS buckets
		FROM %s
		WHERE minute >= toDateTime(%d) AND minute < toDateTime(%d)
		GROUP BY agent %s`,
		int(coverageBucket.Seconds()), agentsTable, start.Unix(), end.Unix(), querySettings(ctx)))
	if err != nil {
		return err
	}
	agents := map[string][]int64{}
	for _, r := range rows {
		if len(r) < 2 {
			return fmt.Errorf("%w: unreadable coverage row", errNotCovered)
		}
		bs, ok := r[1].([]any)
		if !ok {
			return fmt.Errorf("%w: unreadable coverage row", errNotCovered)
		}
		agent := text(r[0])
		for _, v := range bs {
			b := num(v)
			if math.IsNaN(b) {
				return fmt.Errorf("%w: unreadable coverage row", errNotCovered)
			}
			agents[agent] = append(agents[agent], int64(b))
		}
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

	pos := make(map[int64]int, len(required))
	for i, b := range required {
		pos[b] = i
	}
	bucketTime := func(i int) string { return time.Unix(required[i], 0).UTC().Format(time.RFC3339) }
	perBucket := make([]int, len(required))
	for agent, bs := range agents {
		idx := make([]int, 0, len(bs))
		for _, b := range bs {
			if i, ok := pos[b]; ok {
				idx = append(idx, i)
			}
		}
		sort.Ints(idx)
		idx = slices.Compact(idx)
		for k := 1; k < len(idx); k++ {
			if idx[k] != idx[k-1]+1 {
				// Agents are keyed IP/pod uid. A row without a uid (a
				// scrape job without pod discovery) cannot tell a pod that
				// reused a gone pod's IP from a gap, so say why.
				if ip, ok := strings.CutSuffix(agent, "/"); ok {
					return fmt.Errorf("%w: node agent %s has no rows for %s and no pod uid, so a reused IP cannot be told from a gap",
						errNotCovered, ip, bucketTime(idx[k-1]+1))
				}
				return fmt.Errorf("%w: node agent %s has no rows for %s", errNotCovered, agent, bucketTime(idx[k-1]+1))
			}
		}
		for _, i := range idx {
			perBucket[i]++
		}
	}
	for i, n := range perBucket {
		if n == 0 {
			return fmt.Errorf("%w: no node-agent rows for %s", errNotCovered, bucketTime(i))
		}
	}

	if nodeAgents != nil {
		last := len(required) - 1
		want, err := nodeAgents(ctx, time.Unix(required[last], 0))
		if err != nil {
			return fmt.Errorf("%w: cannot count node agents: %v", errNotCovered, err)
		}
		if got := perBucket[last]; got < want {
			return fmt.Errorf("%w: %d of %d node agents in ClickHouse at %s", errNotCovered, got, want, bucketTime(last))
		}
	}
	return nil
}
