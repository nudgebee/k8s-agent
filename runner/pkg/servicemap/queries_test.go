package servicemap

import (
	"strings"
	"testing"
)

func TestQueries_HasExpectedKeys(t *testing.T) {
	want := []string{
		"kube_pod_info", "kube_pod_labels", "kube_service_info",
		"kube_deployment_spec_replicas", "container_net_tcp_successful_connects",
		"container_http_requests_count", "container_http_requests_latency",
		"container_oom_kills_total", "container_restarts",
		"ip_to_fqdn",
	}
	for _, k := range want {
		if _, ok := Queries[k]; !ok {
			t.Errorf("Queries missing %q", k)
		}
	}
}

func TestApplicationQueries_HasFilteredVariants(t *testing.T) {
	for _, k := range []string{"container_net_tcp_successful_connects", "container_http_requests_count"} {
		q := ApplicationQueries[k]
		if !strings.Contains(q, "$SRC_FILTER") || !strings.Contains(q, "$DST_FILTER") {
			t.Errorf("%s should reference $SRC_FILTER and $DST_FILTER, got: %s", k, q)
		}
	}
}

func TestExpandPlaceholders(t *testing.T) {
	q := `rate(container_http_requests_total{__CLUSTER__ $SRC_FILTER}[$RANGE])`
	got := expandPlaceholders(q, "60s",
		`src_workload_name=~"frontend"`,
		`destination_workload_name=~"frontend"`,
		`pod=~".*"`, ``, `cluster="us-east1",`)
	want := `rate(container_http_requests_total{cluster="us-east1", src_workload_name=~"frontend"}[60s])`
	if got != want {
		t.Errorf("expand mismatch:\n got:  %s\n want: %s", got, want)
	}
}

func TestDictToPrometheusFilter_LikeAndExact(t *testing.T) {
	cases := []struct {
		in   map[string]string
		want []string // unordered substrings
	}{
		{nil, []string{""}},
		{map[string]string{}, []string{""}},
		{map[string]string{"src_workload_name": "frontend"}, []string{`src_workload_name=~"frontend"`}},
		{map[string]string{"pod": "frontend%"}, []string{`pod=~"frontend.*"`}}, // % → .*
	}
	for _, c := range cases {
		got := dictToPrometheusFilter(c.in)
		for _, sub := range c.want {
			if !strings.Contains(got, sub) {
				t.Errorf("dictToPrometheusFilter(%v) = %q; want substring %q", c.in, got, sub)
			}
		}
	}
}
