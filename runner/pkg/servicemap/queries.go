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

// Queries is the per-query map. Each value is a PromQL expression with
// $RANGE, $SRC_FILTER, $DST_FILTER, $POD_FILTER, $NAMESPACE_FILTER
// placeholders and a __CLUSTER__ token (replaced at fetch time with the
// cluster filter).
var Queries = map[string]string{
	"node_info":                                      "node_info{__CLUSTER__}",
	"kube_node_info":                                 "kube_node_info{__CLUSTER__}",
	"kube_service_info":                              "kube_service_info{__CLUSTER__}",
	"kube_pod_info":                                  "kube_pod_info{__CLUSTER__}",
	"kube_pod_labels":                                "kube_pod_labels{__CLUSTER__}",
	"kube_pod_status_phase":                          "kube_pod_status_phase{__CLUSTER__}",
	"kube_pod_status_ready":                          `kube_pod_status_ready{__CLUSTER__ condition="true"}`,
	"kube_pod_status_scheduled":                      `kube_pod_status_scheduled{__CLUSTER__ condition="true"} > 0`,
	"kube_deployment_spec_replicas":                  "kube_deployment_spec_replicas{__CLUSTER__}",
	"kube_daemonset_status_desired_number_scheduled": "kube_daemonset_status_desired_number_scheduled{__CLUSTER__}",
	"kube_statefulset_replicas":                      "kube_statefulset_replicas{__CLUSTER__}",
	"container_info":                                 "container_info{__CLUSTER__}",
	"container_application_type":                     "container_application_type{__CLUSTER__}",
	"container_net_tcp_successful_connects":          "rate(container_net_tcp_successful_connects_total{__CLUSTER__}[$RANGE]) > 0",
	"container_net_tcp_failed_connects":              "rate(container_net_tcp_failed_connects_total{__CLUSTER__}[$RANGE]) > 0",
	"container_net_tcp_active_connections":           "container_net_tcp_active_connections{__CLUSTER__} > 0",
	"container_net_tcp_retransmits":                  "rate(container_net_tcp_retransmits_total{__CLUSTER__}[$RANGE]) > 0",
	"container_http_requests_total_count":            "increase(container_http_requests_total{__CLUSTER__}[$RANGE])",
	"container_http_requests_count":                  "rate(container_http_requests_total{__CLUSTER__}[$RANGE])",
	"container_http_requests_failure_count":          `rate(container_http_requests_total{__CLUSTER__ status=~"4..|5.."}[$RANGE]) > 0`,
	"container_http_requests_latency":                "(rate(container_http_requests_duration_seconds_total_sum{__CLUSTER__}[$RANGE]) / rate(container_http_requests_duration_seconds_total_count{__CLUSTER__}[$RANGE])) > 0",
	"container_postgres_requests_count":              "rate(container_postgres_queries_total{__CLUSTER__}[$RANGE])",
	"container_postgres_requests_total_count":        "increase(container_postgres_queries_total{__CLUSTER__}[$RANGE]) > 0",
	"container_postgres_requests_latency":            "(rate(container_postgres_queries_duration_seconds_total_sum{__CLUSTER__}[$RANGE]) / rate(container_postgres_queries_duration_seconds_total_count{__CLUSTER__}[$RANGE])) > 0",
	"container_mysql_queries_total_count":            "increase(container_mysql_queries_total{__CLUSTER__}[$RANGE]) > 0",
	"container_mysql_requests_latency":               "(rate(container_mysql_queries_duration_seconds_total_sum{__CLUSTER__}[$RANGE]) / rate(container_mysql_queries_duration_seconds_total_count{__CLUSTER__}[$RANGE])) > 0",
	"container_mysql_queries_count":                  "rate(container_mysql_queries_total{__CLUSTER__}[$RANGE])",
	"container_redis_queries_count":                  "rate(container_redis_queries_total{__CLUSTER__}[$RANGE])",
	"container_redis_queries_total_count":            "increase(container_redis_queries_total{__CLUSTER__}[$RANGE]) > 0",
	"container_redis_requests_latency":               "(rate(container_redis_queries_duration_seconds_total_sum{__CLUSTER__}[$RANGE]) / rate(container_redis_queries_duration_seconds_total_count{__CLUSTER__}[$RANGE])) > 0",
	"container_kafka_requests_count":                 "rate(container_kafka_requests_total{__CLUSTER__}[$RANGE])",
	"container_kafka_requests_total_count":           "increase(container_kafka_requests_total{__CLUSTER__}[$RANGE]) > 0",
	"container_kafka_requests_latency":               "(rate(container_kafka_requests_duration_seconds_total_sum{__CLUSTER__}[$RANGE]) / rate(container_kafka_requests_duration_seconds_total_count{__CLUSTER__}[$RANGE])) > 0",
	"container_memcached_queries_count":              "rate(container_memcached_queries_total{__CLUSTER__}[$RANGE])",
	"container_memcached_queries_total_count":        "increase(container_memcached_queries_total{__CLUSTER__}[$RANGE])",
	"container_memcached_requests_latency":           "rate(container_memcached_queries_duration_seconds_total_sum{__CLUSTER__}[$RANGE]) / rate(container_memcached_queries_duration_seconds_total_count{__CLUSTER__}[$RANGE])",
	"container_mongo_queries_count":                  "rate(container_mongo_queries_total{__CLUSTER__}[$RANGE])",
	"container_mongo_queries_total_count":            "increase(container_mongo_queries_total{__CLUSTER__}[$RANGE]) > 0",
	"container_mongo_requests_latency":               "(rate(container_mongo_queries_duration_seconds_total_sum{__CLUSTER__}[$RANGE]) / rate(container_mongo_queries_duration_seconds_total_count{__CLUSTER__}[$RANGE])) > 0",
	"container_cassandra_queries_count":              "rate(container_cassandra_queries_total{__CLUSTER__}[$RANGE])",
	"container_cassandra_queries_total_count":        "increase(container_cassandra_queries_total{__CLUSTER__}[$RANGE]) > 0",
	"container_cassandra_requests_latency":           "(rate(container_cassandra_queries_duration_seconds_total_sum{__CLUSTER__}[$RANGE]) / rate(container_cassandra_queries_duration_seconds_total_count{__CLUSTER__}[$RANGE])) > 0",
	"container_clickhouse_queries_count":             "rate(container_clickhouse_queries_total{__CLUSTER__}[$RANGE])",
	"container_clickhouse_queries_total_count":       "increase(container_clickhouse_queries_total{__CLUSTER__}[$RANGE]) > 0",
	"container_clickhouse_requests_latency":          "(rate(container_clickhouse_queries_duration_seconds_total_sum{__CLUSTER__}[$RANGE]) / rate(container_clickhouse_queries_duration_seconds_total_count{__CLUSTER__}[$RANGE])) > 0",
	"container_zookeeper_requests_total":             "rate(container_zookeeper_requests_total{__CLUSTER__}[$RANGE])",
	"container_zookeeper_requests_total_count":       "increase(container_zookeeper_requests_total{__CLUSTER__}[$RANGE]) > 0",
	"container_zookeeper_requests_latency":           "(rate(container_zookeeper_requests_duration_seconds_total_sum{__CLUSTER__}[$RANGE]) / rate(container_zookeeper_requests_duration_seconds_total_count{__CLUSTER__}[$RANGE])) > 0",
	"container_rabbitmq_messages_total_count":        "increase(container_rabbitmq_messages_total{__CLUSTER__}[$RANGE]) > 0",
	"container_nats_messages_total":                  "increase(container_nats_messages_total{__CLUSTER__}[$RANGE]) > 0",
	"container_rabbitmq_messages":                    "rate(container_rabbitmq_messages_total{__CLUSTER__}[$RANGE])",
	"container_nats_messages":                        "rate(container_nats_messages_total{__CLUSTER__}[$RANGE])",
	"ip_to_fqdn":                                     "sum by(fqdn, ip) (ip_to_fqdn{__CLUSTER__})",
	"container_net_tcp_bytes_sent":                   "rate(container_net_tcp_bytes_sent_total{__CLUSTER__}[$RANGE]) > 0",
	"container_net_tcp_bytes_received":               "rate(container_net_tcp_bytes_received_total{__CLUSTER__}[$RANGE]) > 0",
	"container_cpu_usage":                            "rate(container_resources_cpu_usage_seconds_total{__CLUSTER__}[$RANGE])",
	"container_cpu_delay":                            "rate(container_resources_cpu_delay_seconds_total{__CLUSTER__}[$RANGE])",
	"container_throttled_time":                       "rate(container_resources_cpu_throttled_seconds_total{__CLUSTER__}[$RANGE])",
	"container_memory_limit":                         "container_resources_memory_limit_bytes{__CLUSTER__}",
	"container_memory_rss":                           "container_resources_memory_rss_bytes{__CLUSTER__}",
	"container_memory_cache":                         "container_resources_memory_cache_bytes{__CLUSTER__}",
	"container_oom_kills_total":                      "increase(container_oom_kills_total{__CLUSTER__}[$RANGE]) % 10000000",
	"container_restarts":                             "increase(container_restart_count_total{__CLUSTER__}[$RANGE]) % 10000000",
	"container_volume_size":                          "container_resources_disk_size_bytes{__CLUSTER__}",
	"container_volume_used":                          "container_resources_disk_used_bytes{__CLUSTER__}",
	"kube_service_status_load_balancer_ingress":      "kube_service_status_load_balancer_ingress",
}

// ApplicationQueries is used when service_map is invoked with a workload
// filter.
var ApplicationQueries = map[string]string{
	"kube_pod_info":                                  "kube_pod_info{__CLUSTER__}",
	"kube_pod_labels":                                "kube_pod_labels{__CLUSTER__}",
	"kube_pod_status_phase":                          "kube_pod_status_phase{__CLUSTER__}",
	"kube_pod_status_ready":                          "kube_pod_status_ready{__CLUSTER__}",
	"kube_service_info":                              "kube_service_info{__CLUSTER__}",
	"kube_deployment_spec_replicas":                  "kube_deployment_spec_replicas{__CLUSTER__}",
	"kube_daemonset_status_desired_number_scheduled": "kube_daemonset_status_desired_number_scheduled{__CLUSTER__}",
	"kube_statefulset_replicas":                      "kube_statefulset_replicas{__CLUSTER__}",
	"kube_pod_status_scheduled":                      `kube_pod_status_scheduled{__CLUSTER__ condition="true"} > 0`,
	"container_net_tcp_successful_connects":          "(rate(container_net_tcp_successful_connects_total{__CLUSTER__ $SRC_FILTER}[$RANGE])) or (rate(container_net_tcp_successful_connects_total{__CLUSTER__ $DST_FILTER}[$RANGE]))",
	"container_net_tcp_failed_connects":              "(rate(container_net_tcp_failed_connects_total{__CLUSTER__ $SRC_FILTER}[$RANGE])) or (rate(container_net_tcp_failed_connects_total{__CLUSTER__ $DST_FILTER}[$RANGE]))",
	"container_net_tcp_retransmits":                  "(rate(container_net_tcp_retransmits_total{__CLUSTER__ $SRC_FILTER}[$RANGE])) or (rate(container_net_tcp_retransmits_total{__CLUSTER__ $DST_FILTER}[$RANGE]))",
	"container_http_requests_total_count":            "(increase(container_http_requests_total{__CLUSTER__ $SRC_FILTER}[$RANGE])) or (increase(container_http_requests_total{__CLUSTER__ $DST_FILTER}[$RANGE]))",
	"container_http_requests_count":                  "(rate(container_http_requests_total{__CLUSTER__ $SRC_FILTER}[$RANGE])) or (rate(container_http_requests_total{__CLUSTER__ $DST_FILTER}[$RANGE]))",
	"container_application_type":                     "container_application_type{__CLUSTER__}",
	"ip_to_fqdn":                                     "sum by(fqdn, ip) (ip_to_fqdn{__CLUSTER__})",
	"container_net_tcp_bytes_sent":                   "rate(container_net_tcp_bytes_sent_total{__CLUSTER__ $DST_FILTER}[$RANGE]) or rate(container_net_tcp_bytes_sent_total{__CLUSTER__ $SRC_FILTER}[$RANGE])",
	"container_net_tcp_bytes_received":               "rate(container_net_tcp_bytes_received_total{__CLUSTER__ $DST_FILTER}[$RANGE]) or rate(container_net_tcp_bytes_received_total{__CLUSTER__ $SRC_FILTER}[$RANGE])",
}

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
