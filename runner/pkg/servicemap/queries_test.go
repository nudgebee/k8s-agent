package servicemap

import (
	"strings"
	"testing"
)

func TestQueries_HasExpectedKeys(t *testing.T) {
	want := []string{
		"kube_pod_info", "kube_pod_labels", "kube_service_info",
		"kube_deployment_spec_replicas", "container_net_tcp_successful_connects",
		"l7_requests:HTTP", "l7_latency:HTTP", "l7_requests:Postgres", "l7_latency:Postgres",
		"container_oom_kills_total", "container_restarts",
		"l7_requests:RabbitMQ", "container_net_tcp_bytes_sent",
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
	m := edgeMetric{key: "failures", agg: "sum", expr: `rate(container_http_requests_total{__EDGE__ status=~"5.."}[$RANGE])`}
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

// Requests keep the status label so build.go can split out failures; the
// other edge metrics aggregate to the edge alone.
func TestEdgeQuery_ExtraGroupBy(t *testing.T) {
	q := Queries[l7RequestsKey(l7Protocols[0])]
	if !strings.HasPrefix(q, "sum by ("+edgeGroupBy+", status) (") {
		t.Errorf("requests query should group by edge and status: %s", q)
	}
	if strings.Contains(Queries["container_net_tcp_bytes_sent"], "status") {
		t.Errorf("bytes query should not group by status: %s", Queries["container_net_tcp_bytes_sent"])
	}
}

// Protocols without a latency histogram must not get a latency query, and
// every other protocol must.
func TestL7Protocols_LatencyQueries(t *testing.T) {
	for _, p := range l7Protocols {
		_, has := Queries[l7LatencyKey(p)]
		if has != (p.latency != "") {
			t.Errorf("%s: latency query present=%v, histogram=%q", p.name, has, p.latency)
		}
	}
}

func TestFailedStatus(t *testing.T) {
	cases := []struct {
		f      func(string) bool
		status string
		want   bool
	}{
		{httpFailed, "200", false},
		{httpFailed, "302", false},
		{httpFailed, "404", true},
		{httpFailed, "503", true},
		{httpFailed, "", false},
		{statusFailed, "ok", false},
		{statusFailed, "unknown", false},
		{statusFailed, "failed", true},
		{dnsFailed, "ok", false},
		{dnsFailed, "nxdomain", false},
		{dnsFailed, "servfail", true},
	}
	for _, c := range cases {
		if got := c.f(c.status); got != c.want {
			t.Errorf("status %q: failed=%v, want %v", c.status, got, c.want)
		}
	}
}

// Pod inventory and readiness are the state at the end of the window. Read
// over the whole window, a pod that completed during it reports ready=0 and
// is counted as a failed instance.
func TestNodeQueries_PodStateAtWindowEnd(t *testing.T) {
	for _, k := range []string{"kube_pod_info", "kube_pod_labels", "kube_pod_status_ready", "pod_workload"} {
		if strings.Contains(Queries[k], "$RANGE") {
			t.Errorf("%s should be read at the end of the window, got: %s", k, Queries[k])
		}
	}
}
