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
		"container_http_requests_failure_count", "container_net_tcp_bytes_sent",
	}
	for _, k := range want {
		if _, ok := Queries[k]; !ok {
			t.Errorf("Queries missing %q", k)
		}
	}
}

// Every query the builder reads must exist in both sets: a key missing from
// ApplicationQueries silently zeroes that field whenever the UI scopes the
// map to a namespace, which is how latency went missing.
func TestQuerySets_SameKeys(t *testing.T) {
	if len(Queries) != len(ApplicationQueries) {
		t.Errorf("len(Queries)=%d, len(ApplicationQueries)=%d", len(Queries), len(ApplicationQueries))
	}
	for k := range Queries {
		if _, ok := ApplicationQueries[k]; !ok {
			t.Errorf("ApplicationQueries missing %q", k)
		}
	}
}

func TestApplicationQueries_EdgeMetricsFiltered(t *testing.T) {
	for _, m := range edgeMetrics {
		q := ApplicationQueries[m.key]
		if !strings.Contains(q, "$SRC_FILTER") || !strings.Contains(q, "$DST_FILTER") {
			t.Errorf("%s should reference $SRC_FILTER and $DST_FILTER, got: %s", m.key, q)
		}
		if strings.Contains(Queries[m.key], "$SRC_FILTER") {
			t.Errorf("unfiltered %s should not reference $SRC_FILTER: %s", m.key, Queries[m.key])
		}
		if !strings.HasSuffix(q, " > 0") || !strings.HasSuffix(Queries[m.key], " > 0") {
			t.Errorf("%s should drop zero-traffic edges with > 0", m.key)
		}
	}
}

// kube_pod_status_ready has one series per condition; without the
// condition="true" matcher the false/unknown series (value 0) mark every
// pod as failed.
func TestQuerySets_PodReadyOnlyTrueCondition(t *testing.T) {
	for name, qs := range map[string]map[string]string{"Queries": Queries, "ApplicationQueries": ApplicationQueries} {
		if !strings.Contains(qs["kube_pod_status_ready"], `condition="true"`) {
			t.Errorf("%s kube_pod_status_ready lacks condition=\"true\": %s", name, qs["kube_pod_status_ready"])
		}
	}
}

func TestEdgeQuery_Expansion(t *testing.T) {
	m := edgeMetric{"failures", "sum", `rate(container_http_requests_total{__EDGE__ status=~"5.."}[$RANGE])`}
	src := `src_workload_namespace=~"shop"`
	dst := `destination_workload_namespace=~"shop"`

	got := expandPlaceholders(edgeQuery(m, false), "60s", src, dst, "", "", "")
	want := `sum by (` + edgeGroupBy + `) (rate(container_http_requests_total{ status=~"5.."}[60s])) > 0`
	if got != want {
		t.Errorf("unfiltered:\n got:  %s\n want: %s", got, want)
	}

	got = expandPlaceholders(edgeQuery(m, true), "60s", src, dst, "", "", `cluster="c1",`)
	want = `sum by (` + edgeGroupBy + `) ((rate(container_http_requests_total{cluster="c1", ` + src + `, status=~"5.."}[60s]))` +
		` or (rate(container_http_requests_total{cluster="c1", ` + dst + `, status=~"5.."}[60s]))) > 0`
	if got != want {
		t.Errorf("filtered:\n got:  %s\n want: %s", got, want)
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
