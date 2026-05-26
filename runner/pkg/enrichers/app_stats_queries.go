package enrichers

// ApplicationMonitoringQueries is the legacy
// APPLICATION_MONITORING_QUERIES dict. The agent uses these as defaults
// when the caller doesn't pass a `queries` map.
//
// Placeholders the agent substitutes at execution time:
//
//	__CLUSTER__       — replaced upstream by relay (request.go:131) with the
//	                    cluster's Prometheus filter; if the agent receives a
//	                    request that still has the literal it falls back to
//	                    empty — relay is the authoritative substitution point.
//	$POD_FILTER       — replaced from `applications.pod_filter`
//	$WORKLOAD_FILTER  — replaced from `applications.workload_filter`
//	$CONTAINER_FILTER — replaced from `applications.container_filter`
//	$RANGE            — replaced with the step duration ("60s" by default)
var ApplicationMonitoringQueries = map[string]string{
	"pod_cpu_usage_p99": `quantile_over_time(0.99,sum by (container, pod, namespace,job) (rate (` +
		`container_cpu_usage_seconds_total{__CLUSTER__ $POD_FILTER}))[1d:])`,
	"pod_cpu_usage_p50": `quantile_over_time(0.50,sum by (container, pod, namespace,job) (rate (` +
		`container_cpu_usage_seconds_total{__CLUSTER__ $POD_FILTER}))[1d:])`,
	"pod_memory_max_usage": `max_over_time(max(container_memory_working_set_bytes{__CLUSTER__ $POD_FILTER}) by (container, ` +
		`pod, job, namespace)[1h:])`,
	"pod_memory_usage_p99": `quantile_over_time(0.99,sum by (container, pod, namespace,job)(` +
		`container_memory_working_set_bytes{__CLUSTER__ $POD_FILTER, container!="POD", container!=""})[` +
		`1h:])`,
	"pod_memory_usage_p50": `quantile_over_time(0.50,sum by(container, pod, namespace,job)(` +
		`container_memory_working_set_bytes{__CLUSTER__ $POD_FILTER, container!="POD", container!=""})[` +
		`1h:])`,
	"pod_cpu_max_usage": `max_over_time(max(container_memory_working_set_bytes{__CLUSTER__ $POD_FILTER}) by (container, ` +
		`pod, job, namespace)[1h:])`,
	"container_http_requests_total_count": `sum by (destination_workload_name,destination_workload_namespace) (` +
		`increase(container_http_requests_total{__CLUSTER__ $WORKLOAD_FILTER}[1h]))`,
	"container_http_requests_failure_count": `sum by (destination_workload_name,destination_workload_namespace) (` +
		`increase(container_http_requests_total{__CLUSTER__ $WORKLOAD_FILTER, ` +
		`status="500"}[1h]))`,
	"container_log_messages": `increase(container_log_messages_total{__CLUSTER__ $CONTAINER_FILTER,level=~"error|critical"}[` +
		`$RANGE])`,
	"container_cpu_request": `max_over_time(max(kube_pod_container_resource_requests{__CLUSTER__ resource="cpu", $POD_FILTER}) ` +
		`by (container, pod, job, namespace)[1h:])`,
	"container_memory_request": `max_over_time(max(kube_pod_container_resource_requests{__CLUSTER__ resource="memory", ` +
		`$POD_FILTER})` +
		`by (container, pod, job, namespace)[1h:])`,
	"container_cpu_limit": `max_over_time(max(kube_pod_container_resource_limits{__CLUSTER__ resource="cpu", $POD_FILTER}) ` +
		`by (container, pod, job, namespace)[1h:])`,
	"container_memory_limit": `max_over_time(max(kube_pod_container_resource_limits{__CLUSTER__ resource="memory", ` +
		`$POD_FILTER})` +
		`by (container, pod, job, namespace)[1h:])`,
	"container_http_requests_latency_p99": `histogram_quantile(0.99, sum(rate(` +
		`container_http_requests_duration_seconds_total_bucket{__CLUSTER__ $WORKLOAD_FILTER}[` +
		`$RANGE]))` +
		`by (job,container_id, le))`,
}
