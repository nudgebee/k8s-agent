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
var nodeQueries = map[string]string{
	"kube_pod_info":                 "kube_pod_info{__CLUSTER__}",
	"pod_workload":                  "count by (container_id, src_workload_kind, src_workload_name, src_workload_namespace) (container_net_tcp_bytes_sent_total{__CLUSTER__})",
	"kube_pod_labels":               "kube_pod_labels{__CLUSTER__}",
	"kube_pod_status_ready":         `kube_pod_status_ready{__CLUSTER__ condition="true"}`,
	"kube_service_info":             "kube_service_info{__CLUSTER__}",
	"kube_deployment_spec_replicas": "kube_deployment_spec_replicas{__CLUSTER__}",
	"kube_daemonset_status_desired_number_scheduled": "kube_daemonset_status_desired_number_scheduled{__CLUSTER__}",
	"kube_statefulset_replicas":                      "kube_statefulset_replicas{__CLUSTER__}",
	"container_oom_kills_total":                      "increase(container_oom_kills_total{__CLUSTER__}[$RANGE]) % 10000000",
	"container_restarts":                             "increase(container_restart_count_total{__CLUSTER__}[$RANGE]) % 10000000",
	"container_throttled_time":                       "rate(container_resources_cpu_throttled_seconds_total{__CLUSTER__}[$RANGE])",
	"container_volume_size":                          "container_resources_disk_size_bytes{__CLUSTER__}",
	"container_volume_used":                          "container_resources_disk_used_bytes{__CLUSTER__}",
}

// edgeMetric is one per-edge connection or request series build.go reads.
// expr writes every selector as {__EDGE__} or {__EDGE__ extra="matcher"};
// the token becomes the cluster filter, plus the src or dst workload filter
// when the map is scoped to a workload or namespace.
//
// agg is the server-side aggregation over edgeGroupBy: sum mirrors the
// additive `la.x += r.Last` accumulation in build.go, max mirrors the latency
// `if r.Last > la.latency` projection.
type edgeMetric struct {
	key  string
	agg  string
	expr string
}

// edgeMetrics is the single list both query sets are generated from, so the
// filtered map cannot silently lose a metric the unfiltered map has.
var edgeMetrics = []edgeMetric{
	{"container_net_tcp_successful_connects", "sum", "rate(container_net_tcp_successful_connects_total{__EDGE__}[$RANGE])"},
	{"container_http_requests_count", "sum", "rate(container_http_requests_total{__EDGE__}[$RANGE])"},
	{"container_http_requests_failure_count", "sum", `rate(container_http_requests_total{__EDGE__ status=~"4..|5.."}[$RANGE])`},
	{"container_http_requests_latency", "max", "rate(container_http_requests_duration_seconds_total_sum{__EDGE__}[$RANGE]) / rate(container_http_requests_duration_seconds_total_count{__EDGE__}[$RANGE])"},
	{"container_net_tcp_bytes_sent", "sum", "rate(container_net_tcp_bytes_sent_total{__EDGE__}[$RANGE])"},
	{"container_net_tcp_bytes_received", "sum", "rate(container_net_tcp_bytes_received_total{__EDGE__}[$RANGE])"},
}

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
	return m.agg + " by (" + edgeGroupBy + ") (" + inner + ") > 0"
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
// PromQL expression with a $RANGE placeholder and a __CLUSTER__ token
// (replaced at fetch time with the cluster filter).
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
