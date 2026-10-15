package servicemap

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	chclient "github.com/nudgebee/nudgebee-agent/pkg/clickhouse"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

func TestParseSource(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Source
	}{
		{"", SourcePrometheus}, {"prometheus", SourcePrometheus}, {" ClickHouse ", SourceClickHouse}, {"shadow", SourceShadow},
	} {
		if got, ok := ParseSource(tc.in); !ok || got != tc.want {
			t.Errorf("ParseSource(%q) = %q, %v; want %q, true", tc.in, got, ok, tc.want)
		}
	}
	if got, ok := ParseSource("victoria"); ok || got != SourcePrometheus {
		t.Errorf("unknown source = %q, %v; want prometheus, false", got, ok)
	}
}

// Every edge key build() reads from PromQL must have a ClickHouse source, and
// every counter and histogram queries.go names must be rolled up — otherwise a
// protocol silently vanishes from the ClickHouse map.
func TestClickHouseCoversEveryEdgeKey(t *testing.T) {
	counters := map[string]bool{}
	for _, m := range rollupCounterMetrics() {
		counters[m] = true
	}
	latencies := map[string]bool{}
	for _, m := range rollupLatencyMetrics() {
		latencies[m] = true
	}
	for _, m := range edgeMetrics {
		switch {
		case tcpEdgeMetrics[m.key] != "":
			if !counters[tcpEdgeMetrics[m.key]] {
				t.Errorf("%s: counter %s not rolled up", m.key, tcpEdgeMetrics[m.key])
			}
		case strings.HasPrefix(m.key, "l7_requests:") || strings.HasPrefix(m.key, "l7_latency:"):
		default:
			t.Errorf("edge key %s has no ClickHouse source", m.key)
		}
	}
	for _, p := range l7Protocols {
		if !counters[p.requests] {
			t.Errorf("%s: %s not rolled up", p.name, p.requests)
		}
		if p.latency != "" && !latencies[p.latency] {
			t.Errorf("%s: %s not rolled up", p.name, p.latency)
		}
	}
}

// The views must not read columns the exporter has renamed between versions:
// a failing view fails the insert into its source table.
func TestRollupDDLReadsOnlyStableColumns(t *testing.T) {
	ddl := rollupDDL("default")
	if len(ddl) != 3 {
		t.Fatalf("got %d statements, want 3", len(ddl))
	}
	for _, stmt := range ddl {
		if !strings.Contains(stmt, "IF NOT EXISTS") {
			t.Errorf("statement is not idempotent:\n%s", stmt)
		}
		for _, unstable := range []string{"AggTemp", "AggregationTemporality", "ServiceName", "ScopeName", "ResourceAttributes"} {
			if strings.Contains(stmt, unstable) {
				t.Errorf("statement reads %s:\n%s", unstable, stmt)
			}
		}
	}
	if !strings.Contains(ddl[1], "'container_http_requests_total'") || !strings.Contains(ddl[2], "'container_http_requests_duration_seconds_total'") {
		t.Error("views do not filter on the generated metric lists")
	}
}

func TestFilterSQL(t *testing.T) {
	if got := filterSQL(FilterParams{}); got != "" {
		t.Errorf("no filter: got %q", got)
	}
	got := filterSQL(FilterParams{WorkloadName: "api%", WorkloadNamespace: "sh'op"})
	for _, want := range []string{
		`match(src_workload_name, '^(?:api.*)$')`,
		`match(src_workload_namespace, '^(?:sh\'op)$')`,
		`match(destination_workload_name, '^(?:api.*)$')`,
		" OR ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("filterSQL missing %q in %q", want, got)
		}
	}
}

func TestCompareMaps(t *testing.T) {
	app := func(rate float64, upstream string) Application {
		return Application{ID: ApplicationID{Name: "web", Kind: "Deployment", Namespace: "shop"},
			Upstreams: []UpstreamLink{{ID: upstream, Protocol: "HTTP", RequestCount: rate}}}
	}
	d := compareMaps(
		[]Application{app(100, "shop:Deployment:cart"), app(1, "shop:Deployment:db")},
		[]Application{app(97, "shop:Deployment:cart"), app(5, "shop:Deployment:cache")},
	)
	if d.both != 1 || d.onlyProm != 1 || d.onlyCH != 1 || d.reqWithin5 != 1 || d.reqCompared != 1 {
		t.Errorf("unexpected diff %+v", d)
	}
}

// fakeClickHouse answers the runner's ClickHouse queries in JSONCompact.
type fakeClickHouse struct {
	covered    bool
	edgeCalls  atomic.Int64
	coverCalls atomic.Int64
}

func (f *fakeClickHouse) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	sql := string(body)
	var data [][]any
	switch {
	case strings.Contains(sql, "EXISTS TABLE"):
		data = [][]any{{1}}
	case strings.Contains(sql, "count()"):
		f.coverCalls.Add(1)
		if f.covered {
			data = [][]any{{float64(1e9), float64(4e9), "1000"}}
		} else {
			data = [][]any{{float64(0), float64(0), "0"}}
		}
	case strings.Contains(sql, "GROUP BY metric"):
		f.edgeCalls.Add(1)
		row := func(metric, status string, value, latSum float64, latCount string) []any {
			return []any{metric, "Deployment", "", "frontend", "shop", "Deployment", "backend", "shop", "", status, value, latSum, latCount}
		}
		data = [][]any{
			row("container_http_requests_total", "200", 3240, 0, "0"),
			row("container_http_requests_total", "503", 360, 0, "0"),
			row("container_http_requests_duration_seconds_total", "", 0, 36, "3600"),
			row("container_net_tcp_successful_connects_total", "", 720, 0, "0"),
		}
	case strings.Contains(sql, "UNION ALL"):
		data = [][]any{
			{"container_restarts", "/k8s/shop/backend-def-1/app", "", "", "", float64(2)},
			{"pod_workload", "/k8s/shop/backend-def-1/app", "Deployment", "backend", "shop", float64(1)},
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"meta": []map[string]string{}, "data": data})
}

func newTestService(t *testing.T, ch *fakeClickHouse, src Source) (*Service, *atomic.Int64) {
	t.Helper()
	var promCalls atomic.Int64
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		promCalls.Add(1)
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "kube_pod_info"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"pod":"backend-def-1","namespace":"shop","pod_ip":"10.0.0.2","created_by_kind":"ReplicaSet","created_by_name":"backend-5d8c7b9f6"},"value":[1,"1"]}]}}`))
		case strings.Contains(q, "rate(container_http_requests_total{"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"src_workload_name":"frontend","src_workload_namespace":"shop","destination_workload_name":"backend","destination_workload_namespace":"shop","status":"200"},"value":[1,"7"]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	t.Cleanup(prom.Close)
	chSrv := httptest.NewServer(ch)
	t.Cleanup(chSrv.Close)

	s := New(prometheus.New(prom.URL, &http.Client{Timeout: 5 * time.Second}), "")
	s.CH = chclient.New(chclient.Config{Host: chSrv.URL})
	s.Source = src
	return s, &promCalls
}

func frontendLink(t *testing.T, apps []Application) UpstreamLink {
	t.Helper()
	for _, a := range apps {
		if a.ID.Name == "frontend" && len(a.Upstreams) > 0 {
			return a.Upstreams[0]
		}
	}
	t.Fatal("no frontend upstream in the map")
	return UpstreamLink{}
}

// A one-hour window ending on a whole minute.
var testParams = FilterParams{EndTime: "2026-10-15T10:00:00Z", Duration: 60}

func TestBuild_ClickHouseSource(t *testing.T) {
	ch := &fakeClickHouse{covered: true}
	s, promCalls := newTestService(t, ch, SourceClickHouse)

	apps, err := s.Build(context.Background(), testParams)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := promCalls.Load(), int64(len(kubeQueries(Queries))); got != want {
		t.Errorf("prometheus queries = %d; want %d (kube_* only)", got, want)
	}
	l := frontendLink(t, apps)
	// 3240 + 360 requests over 3600 s; 360 of them 5xx; 36 s over 3600 requests.
	if math.Abs(l.RequestCount-1) > 1e-9 || math.Abs(l.FailureCount-0.1) > 1e-9 || math.Abs(l.Latency-0.01) > 1e-9 {
		t.Errorf("link = req %v fail %v latency %v; want 1, 0.1, 0.01", l.RequestCount, l.FailureCount, l.Latency)
	}
	if l.Protocol != "HTTP" {
		t.Errorf("protocol = %q; want HTTP", l.Protocol)
	}
}

// When the rollup does not cover the window, the map must come from
// Prometheus in full, never from part of the window.
func TestBuild_ClickHouseNotCoveredFallsBack(t *testing.T) {
	ch := &fakeClickHouse{covered: false}
	s, promCalls := newTestService(t, ch, SourceClickHouse)

	apps, err := s.Build(context.Background(), testParams)
	if err != nil {
		t.Fatal(err)
	}
	if got := promCalls.Load(); got != int64(len(Queries)) {
		t.Errorf("prometheus queries = %d; want all %d", got, len(Queries))
	}
	if ch.edgeCalls.Load() != 0 {
		t.Error("edges read from an uncovered rollup")
	}
	if l := frontendLink(t, apps); l.RequestCount != 7 {
		t.Errorf("request rate = %v; want the PromQL 7", l.RequestCount)
	}
}

// Shadow serves the PromQL map and compares in the background at most once
// per interval for the same window.
func TestBuild_ShadowServesPrometheusAndRateLimits(t *testing.T) {
	ch := &fakeClickHouse{covered: true}
	s, promCalls := newTestService(t, ch, SourceShadow)

	for i := 0; i < 3; i++ {
		apps, err := s.Build(context.Background(), testParams)
		if err != nil {
			t.Fatal(err)
		}
		if l := frontendLink(t, apps); l.RequestCount != 7 {
			t.Fatalf("shadow served request rate %v; want the PromQL 7", l.RequestCount)
		}
	}
	if got := promCalls.Load(); got != 3*int64(len(Queries)) {
		t.Errorf("prometheus queries = %d; want %d", got, 3*len(Queries))
	}
	deadline := time.Now().Add(2 * time.Second)
	for ch.edgeCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if got := ch.coverCalls.Load(); got != 1 {
		t.Errorf("clickhouse comparisons = %d; want 1 within the shadow interval", got)
	}
}

// Without a ClickHouse client every source is plain PromQL.
func TestBuild_NoClickHouseIsPrometheus(t *testing.T) {
	ch := &fakeClickHouse{covered: true}
	s, promCalls := newTestService(t, ch, SourceClickHouse)
	s.CH = nil
	if _, err := s.Build(context.Background(), testParams); err != nil {
		t.Fatal(err)
	}
	if got := promCalls.Load(); got != int64(len(Queries)) {
		t.Errorf("prometheus queries = %d; want %d", got, len(Queries))
	}
	if ch.coverCalls.Load() != 0 {
		t.Error("clickhouse queried without a client")
	}
}
