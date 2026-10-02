// Package servicemap implements the `map/` service-map builder (Group I in
// the deprecation plan). It queries in-cluster Prometheus for metrics emitted
// by the Coroot eBPF node agent, builds a topology of applications and their
// connections, and renders a wire format the backend consumes.
//
// MVP fidelity note: this implementation covers the orchestration + query
// catalog + core graph-build logic. Several finer-grained features
// (per-protocol detection across upstreams, NaN handling for
// throttle/restart sums, application-category classification) are simplified
// or deferred. Phase-4 shadow-diff against the legacy output will guide
// where to deepen.
package servicemap

import "strings"

// edgeGroupBy is the label set service-map edges are keyed on — it mirrors
// the labels edgeFromConnectionLabels (build.go) reads. Connection/request
// metrics carry high-cardinality labels (status, method, instance, pod, le,
// …) that the map builder discards: it collapses everything down to these
// edge labels via la.<field> += r.Last. container_http_requests_total alone
// is ~20–40k raw series in a busy cluster; decoding all of it into
// map[string]string + [][]any was the dominant heap consumer (~1GB per
// Build, captured in a live heap profile).
//
// We aggregate server-side by the same labels so Prometheus returns one
// series per edge (hundreds) instead of the full raw cardinality. The result
// is identical — build.go already summed across the collapsed series — at a
// fraction of the memory.
const edgeGroupBy = "src_workload_kind, src_kind, src_workload_name, " +
	"src_workload_namespace, destination_workload_kind, " +
	"destination_workload_name, destination_workload_namespace, destination_ip"

// nodeQueries are the per-pod and per-workload series build.go reads. They
// stay cluster-wide when a workload filter is set: render drops every
// application that ends up without an edge, so filtering them would only
// lose pods of the filtered workload's peers.
//
// Every query is evaluated once, at the end of the window: pods, readiness
// and replica counts are the state at that moment, while restarts, OOM kills
// and throttling cover the whole window. Reading pod state over the whole
// window instead would count pods that completed during it as failed.
var nodeQueries = map[string]string{
	// Completed pods stay in kube-state-metrics with ready=0; they are
	// history, not failed instances.
	"kube_pod_info":                 `kube_pod_info{__CLUSTER__} unless on (cluster, namespace, pod) (kube_pod_status_phase{__CLUSTER__ phase="Succeeded"} == 1)`,
	"pod_workload":                  "count by (container_id, src_workload_kind, src_workload_name, src_workload_namespace) (container_net_tcp_bytes_sent_total{__CLUSTER__})",
	"kube_pod_labels":               "kube_pod_labels{__CLUSTER__}",
	"kube_pod_status_ready":         `kube_pod_status_ready{__CLUSTER__ condition="true"}`,
	"kube_service_info":             "kube_service_info{__CLUSTER__}",
	"kube_deployment_spec_replicas": "kube_deployment_spec_replicas{__CLUSTER__}",
	"kube_daemonset_status_desired_number_scheduled": "kube_daemonset_status_desired_number_scheduled{__CLUSTER__}",
	"kube_statefulset_replicas":                      "kube_statefulset_replicas{__CLUSTER__}",
	"container_oom_kills_total":                      "increase(container_oom_kills_total{__CLUSTER__}[$RANGE]) % 10000000",
	"container_restarts":                             "increase(container_restarts_total{__CLUSTER__}[$RANGE]) % 10000000",
	"container_throttled_time":                       "rate(container_resources_cpu_throttled_seconds_total{__CLUSTER__}[$RANGE])",
	"container_volume_size":                          "container_resources_disk_size_bytes{__CLUSTER__}",
	"container_volume_used":                          "container_resources_disk_used_bytes{__CLUSTER__}",
}

// l7Protocol is one application protocol the eBPF agent decodes. Each has a
// request counter with a `status` label and, for request/response
// protocols, a latency histogram, all carrying the same source/destination
// labels as the TCP metrics.
type l7Protocol struct {
	name     string // the link's Protocol; lower-cased by consumers to classify the upstream
	requests string // request counter
	latency  string // latency histogram base name; "" when the agent records none
	failed   func(status string) bool
}

// l7Protocols lists every protocol the eBPF agent reports. Querying one that
// is absent from the cluster is an index lookup that returns nothing.
var l7Protocols = []l7Protocol{
	{"HTTP", "container_http_requests_total", "container_http_requests_duration_seconds_total", httpFailed},
	{"Postgres", "container_postgres_queries_total", "container_postgres_queries_duration_seconds_total", statusFailed},
	{"MySQL", "container_mysql_queries_total", "container_mysql_queries_duration_seconds_total", statusFailed},
	{"MongoDB", "container_mongo_queries_total", "container_mongo_queries_duration_seconds_total", statusFailed},
	{"Redis", "container_redis_queries_total", "container_redis_queries_duration_seconds_total", statusFailed},
	{"Memcached", "container_memcached_queries_total", "container_memcached_queries_duration_seconds_total", statusFailed},
	{"Cassandra", "container_cassandra_queries_total", "container_cassandra_queries_duration_seconds_total", statusFailed},
	{"ClickHouse", "container_clickhouse_queries_total", "container_clickhouse_queries_duration_seconds_total", statusFailed},
	{"FoundationDB", "container_foundationdb_requests_total", "container_foundationdb_requests_duration_seconds_total", statusFailed},
	{"Zookeeper", "container_zookeeper_requests_total", "container_zookeeper_requests_duration_seconds_total", statusFailed},
	{"Kafka", "container_kafka_requests_total", "container_kafka_requests_duration_seconds_total", statusFailed},
	{"RabbitMQ", "container_rabbitmq_messages_total", "", statusFailed},
	{"NATS", "container_nats_messages_total", "", statusFailed},
	{"Dubbo", "container_dubbo_requests_total", "container_dubbo_requests_duration_seconds_total", statusFailed},
	{"DNS", "container_dns_requests_total", "container_dns_requests_duration_seconds_total", dnsFailed},
}

// httpFailed treats 4xx and 5xx responses as failures.
func httpFailed(status string) bool {
	return len(status) == 3 && (status[0] == '4' || status[0] == '5')
}

// statusFailed is the agent's verdict for non-HTTP protocols: "ok",
// "failed" or "unknown".
func statusFailed(status string) bool { return status == "failed" }

// dnsFailed excludes NXDOMAIN: resolvers walking the search path produce it
// for most lookups that eventually succeed.
func dnsFailed(status string) bool { return status != "ok" && status != "nxdomain" }

func l7RequestsKey(p l7Protocol) string { return "l7_requests:" + p.name }
func l7LatencyKey(p l7Protocol) string  { return "l7_latency:" + p.name }

// edgeMetric is one per-edge connection or request series build.go reads.
// expr writes every selector as {__EDGE__} or {__EDGE__ extra="matcher"};
// the token becomes the cluster filter, plus the src or dst workload filter
// when the map is scoped to a workload or namespace.
//
// agg is the server-side aggregation over edgeGroupBy: sum mirrors the
// additive `la.x += r.Last` accumulation in build.go, max mirrors the latency
// `if r.Last > la.latency` projection. by names labels kept on top of the
// edge labels.
type edgeMetric struct {
	key  string
	agg  string
	by   string
	expr string
}

// edgeMetrics is the single list both query sets are generated from, so the
// filtered map cannot silently lose a metric the unfiltered map has.
var edgeMetrics = func() []edgeMetric {
	out := make([]edgeMetric, 0, 3+2*len(l7Protocols))
	out = append(out,
		edgeMetric{key: "container_net_tcp_successful_connects", agg: "sum", expr: "rate(container_net_tcp_successful_connects_total{__EDGE__}[$RANGE])"},
		edgeMetric{key: "container_net_tcp_bytes_sent", agg: "sum", expr: "rate(container_net_tcp_bytes_sent_total{__EDGE__}[$RANGE])"},
		edgeMetric{key: "container_net_tcp_bytes_received", agg: "sum", expr: "rate(container_net_tcp_bytes_received_total{__EDGE__}[$RANGE])"},
	)
	for _, p := range l7Protocols {
		// Requests stay split by status so one query yields both the request
		// rate and the failure rate.
		out = append(out, edgeMetric{key: l7RequestsKey(p), agg: "sum", by: "status", expr: "rate(" + p.requests + "{__EDGE__}[$RANGE])"})
		if p.latency != "" {
			// Divide only where requests were counted. With none in the
			// window rate(_count) is exactly 0, but rate(_sum) need not be:
			// the TSDB can return a stored float sum rounded two different
			// ways, and that 1-ulp wobble divided by 0 is +Inf, not NaN.
			out = append(out, edgeMetric{key: l7LatencyKey(p), agg: "max", expr: "rate(" + p.latency + "_sum{__EDGE__}[$RANGE]) / (rate(" + p.latency + "_count{__EDGE__}[$RANGE]) > 0)"})
		}
	}
	return out
}()

// edgeQuery renders one edgeMetric. Unfiltered, it aggregates every series.
// Filtered, it takes the series whose source OR destination matches the
// filter, so the map shows both what the workload calls and what calls it;
// `or` keeps a single copy of a series that matches both sides.
//
// The trailing `> 0` drops edges with no traffic in the window. Without it a
// connection seen once, days ago, still draws an edge as long as its counter
// series exists. It also drops NaN latencies from 0/0 ratios.
func edgeQuery(m edgeMetric, filtered bool) string {
	inner := strings.ReplaceAll(m.expr, "__EDGE__", "__CLUSTER__")
	if filtered {
		src := strings.ReplaceAll(m.expr, "__EDGE__", "__CLUSTER__ $SRC_FILTER,")
		dst := strings.ReplaceAll(m.expr, "__EDGE__", "__CLUSTER__ $DST_FILTER,")
		inner = "(" + src + ") or (" + dst + ")"
	}
	by := edgeGroupBy
	if m.by != "" {
		by += ", " + m.by
	}
	return m.agg + " by (" + by + ") (" + inner + ") > 0"
}

func buildQueries(filtered bool) map[string]string {
	out := make(map[string]string, len(nodeQueries)+len(edgeMetrics))
	for k, q := range nodeQueries {
		out[k] = q
	}
	for _, m := range edgeMetrics {
		out[m.key] = edgeQuery(m, filtered)
	}
	return out
}

// Queries is the per-query map for the whole-cluster map. Each value is a
// PromQL expression with a $RANGE placeholder (the selected window) and a
// __CLUSTER__ token (replaced at fetch time with the cluster filter).
var Queries = buildQueries(false)

// ApplicationQueries is used when service_map is invoked with a workload or
// namespace filter. It differs from Queries only in the edge metrics, which
// also carry $SRC_FILTER / $DST_FILTER.
var ApplicationQueries = buildQueries(true)

// expandPlaceholders substitutes $RANGE/$SRC_FILTER/$DST_FILTER/$POD_FILTER/
// $NAMESPACE_FILTER and the __CLUSTER__ token in one PromQL string.
//
// Expansion logic for the placeholders.
func expandPlaceholders(query, rangeStep, srcFilter, dstFilter, podFilter, nsFilter, clusterFilter string) string {
	q := strings.ReplaceAll(query, "$RANGE", rangeStep)
	q = strings.ReplaceAll(q, "$SRC_FILTER", srcFilter)
	q = strings.ReplaceAll(q, "$DST_FILTER", dstFilter)
	q = strings.ReplaceAll(q, "$POD_FILTER", podFilter)
	q = strings.ReplaceAll(q, "$NAMESPACE_FILTER", nsFilter)
	q = strings.ReplaceAll(q, "__CLUSTER__", clusterFilter)
	return q
}

// dictToPrometheusFilter ports the backend — turns a
// {key: value} map into a comma-separated PromQL label-filter string.
// Values containing '%' get treated as LIKE (regex .*); otherwise =~ exact.
func dictToPrometheusFilter(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		if strings.Contains(v, "%") {
			v = strings.ReplaceAll(v, "%", ".*")
		}
		parts = append(parts, k+`=~"`+v+`"`)
	}
	return strings.Join(parts, ",")
}
