package servicemap

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
// every counter and histogram queries.go names must be queried — otherwise a
// protocol silently vanishes from the ClickHouse map.
func TestClickHouseCoversEveryEdgeKey(t *testing.T) {
	counters := map[string]bool{}
	for _, m := range counterMetrics() {
		counters[m] = true
	}
	latencies := map[string]bool{}
	for _, m := range latencyMetrics() {
		latencies[m] = true
	}
	for _, m := range edgeMetrics {
		switch {
		case tcpEdgeMetrics[m.key] != "":
			if !counters[tcpEdgeMetrics[m.key]] {
				t.Errorf("%s: counter %s not read from ClickHouse", m.key, tcpEdgeMetrics[m.key])
			}
		case strings.HasPrefix(m.key, "l7_requests:") || strings.HasPrefix(m.key, "l7_latency:"):
		default:
			t.Errorf("edge key %s has no ClickHouse source", m.key)
		}
	}
	for _, p := range l7Protocols {
		if !counters[p.requests] {
			t.Errorf("%s: %s not read from ClickHouse", p.name, p.requests)
		}
		if p.latency != "" && !latencies[p.latency] {
			t.Errorf("%s: %s not read from ClickHouse", p.name, p.latency)
		}
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

func TestText(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want string
	}{{nil, ""}, {"web", "web"}, {float64(2), "2"}} {
		if got := text(tc.in); got != tc.want {
			t.Errorf("text(%v) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestQuerySettings(t *testing.T) {
	if got := querySettings(context.Background()); !strings.Contains(got, "max_execution_time = 60") {
		t.Errorf("no deadline: %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if got := querySettings(ctx); !strings.Contains(got, "max_execution_time = 10") {
		t.Errorf("10 s left: %q", got)
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

// A one-hour window, 09:00–10:00 UTC.
var (
	testParams = FilterParams{EndTime: "2026-10-15T10:00:00Z", Duration: 60}
	testEnd    = time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
)

// fullBuckets is coverage for every 5-minute bucket of the hour before end.
func fullBuckets(end time.Time, agents float64) map[int64]float64 {
	out := map[int64]float64{}
	for b := end.Add(-time.Hour); b.Before(end); b = b.Add(coverageBucket) {
		out[b.Unix()] = agents
	}
	return out
}

// fakeClickHouse answers the runner's ClickHouse queries in JSONCompact.
type fakeClickHouse struct {
	buckets    map[int64]float64 // coverage: bucket start → node agents
	nanBuckets bool              // coverage rows come back unreadable
	delay      time.Duration     // every query takes this long
	block      chan struct{}     // when set, the coverage query waits for it
	edgeCalls  atomic.Int64
	coverCalls atomic.Int64

	mu   sync.Mutex
	sqls []string
}

func (f *fakeClickHouse) sql(contains string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sqls {
		if strings.Contains(s, contains) {
			return s
		}
	}
	return ""
}

func (f *fakeClickHouse) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	sql := string(body)
	f.mu.Lock()
	f.sqls = append(f.sqls, sql)
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	var data [][]any
	switch {
	case strings.Contains(sql, "toStartOfInterval"):
		f.coverCalls.Add(1)
		if f.block != nil {
			<-f.block
		}
		for b, n := range f.buckets {
			if f.nanBuckets {
				data = append(data, []any{"not-a-number", n})
			} else {
				data = append(data, []any{float64(b), float64(n)})
			}
		}
	case strings.Contains(sql, "HAVING sum(Value) > 0"):
		f.edgeCalls.Add(1)
		row := func(metric, status string, value float64) []any {
			return []any{metric, "Deployment", "", "frontend", "shop", "Deployment", "backend", "shop", "", status, value}
		}
		data = [][]any{
			row("container_http_requests_total", "200", 3240),
			row("container_http_requests_total", "503", 360),
			row("container_net_tcp_successful_connects_total", "", 720),
		}
	case strings.Contains(sql, "series_mean"):
		data = [][]any{{"container_http_requests_duration_seconds_total", "Deployment", "", "frontend", "shop", "Deployment", "backend", "shop", "", float64(0.01)}}
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

func TestBuild_ClickHouseSource(t *testing.T) {
	ch := &fakeClickHouse{buckets: fullBuckets(testEnd, 3)}
	s, promCalls := newTestService(t, ch, SourceClickHouse)
	s.NodeAgentCount = func(context.Context, time.Time) (int, error) { return 3, nil }

	apps, err := s.Build(context.Background(), testParams)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := promCalls.Load(), int64(len(kubeKeys(Queries))); got != want {
		t.Errorf("prometheus queries = %d; want %d (kube_* only)", got, want)
	}
	l := frontendLink(t, apps)
	// 3240 + 360 requests over 3600 s, 360 of them 5xx; slowest series mean 10 ms.
	if math.Abs(l.RequestCount-1) > 1e-9 || math.Abs(l.FailureCount-0.1) > 1e-9 || math.Abs(l.Latency-0.01) > 1e-9 {
		t.Errorf("link = req %v fail %v latency %v; want 1, 0.1, 0.01", l.RequestCount, l.FailureCount, l.Latency)
	}
	if l.Protocol != "HTTP" {
		t.Errorf("protocol = %q; want HTTP", l.Protocol)
	}
}

// Cumulative rows must never be summed, and the union's limits must bind to
// the whole query.
func TestClickHouseQueriesReadDeltasOnly(t *testing.T) {
	ch := &fakeClickHouse{buckets: fullBuckets(testEnd, 3)}
	if _, err := fetchClickHouse(context.Background(), newTestClient(t, ch), testEnd.Add(-time.Hour), testEnd, FilterParams{}, nil); err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{
		"coverage": ch.sql("toStartOfInterval"), "edges": ch.sql("HAVING sum(Value) > 0"), "health": ch.sql("UNION ALL"),
	} {
		if !strings.Contains(sql, deltaCounters) {
			t.Errorf("%s query does not filter on %q:\n%s", name, deltaCounters, sql)
		}
	}
	if !strings.Contains(ch.sql("series_mean"), deltaHistograms) {
		t.Errorf("latency query does not filter on %q", deltaHistograms)
	}
	health := strings.TrimSpace(ch.sql("UNION ALL"))
	if !strings.HasPrefix(health, "SELECT * FROM (") || !strings.Contains(health, ") SETTINGS") {
		t.Errorf("health union is not wrapped under one SETTINGS:\n%s", health)
	}
	// The outer * reads the union's columns by the first SELECT's names; three
	// unnamed '' columns would all resolve to the first one (seen on 24.12:
	// pod_workload came back as Deployment/Deployment/Deployment).
	if !strings.Contains(health, "'' AS kind, '' AS name, '' AS ns") {
		t.Errorf("health union's first SELECT does not name its columns uniquely:\n%s", health)
	}
}

func newTestClient(t *testing.T, ch *fakeClickHouse) *chclient.Client {
	t.Helper()
	srv := httptest.NewServer(ch)
	t.Cleanup(srv.Close)
	return chclient.New(chclient.Config{Host: srv.URL})
}

// Any gap, drop or missing node agent must refuse the window, not serve a
// map built from part of the data.
func TestCheckCoverage(t *testing.T) {
	gap := fullBuckets(testEnd, 3)
	delete(gap, testEnd.Add(-30*time.Minute).Unix())
	drop := fullBuckets(testEnd, 4)
	drop[testEnd.Add(-30*time.Minute).Unix()] = 2
	agents := func(n int, err error) NodeAgentCounter {
		return func(context.Context, time.Time) (int, error) { return n, err }
	}
	for _, tc := range []struct {
		name    string
		ch      *fakeClickHouse
		agents  NodeAgentCounter
		covered bool
	}{
		{"complete", &fakeClickHouse{buckets: fullBuckets(testEnd, 3)}, agents(3, nil), true},
		{"no kubernetes count", &fakeClickHouse{buckets: fullBuckets(testEnd, 3)}, nil, true},
		{"no rows", &fakeClickHouse{}, nil, false},
		{"outage mid-window", &fakeClickHouse{buckets: gap}, nil, false},
		{"agents dropped mid-window", &fakeClickHouse{buckets: drop}, nil, false},
		{"an agent never scraped", &fakeClickHouse{buckets: fullBuckets(testEnd, 3)}, agents(4, nil), false},
		{"cannot count agents", &fakeClickHouse{buckets: fullBuckets(testEnd, 3)}, agents(0, errors.New("forbidden")), false},
		{"unreadable rows", &fakeClickHouse{buckets: fullBuckets(testEnd, 3), nanBuckets: true}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCoverage(context.Background(), newTestClient(t, tc.ch), testEnd.Add(-time.Hour), testEnd, tc.agents)
			if tc.covered && err != nil {
				t.Errorf("err = %v; want covered", err)
			}
			if !tc.covered && !errors.Is(err, errNotCovered) {
				t.Errorf("err = %v; want errNotCovered", err)
			}
		})
	}
}

// When ClickHouse does not hold the whole window, the map must come from
// Prometheus in full, never from part of the window.
func TestBuild_ClickHouseNotCoveredFallsBack(t *testing.T) {
	ch := &fakeClickHouse{}
	s, promCalls := newTestService(t, ch, SourceClickHouse)

	apps, err := s.Build(context.Background(), testParams)
	if err != nil {
		t.Fatal(err)
	}
	if got := promCalls.Load(); got != int64(len(Queries)) {
		t.Errorf("prometheus queries = %d; want all %d", got, len(Queries))
	}
	if ch.edgeCalls.Load() != 0 {
		t.Error("edges read from ClickHouse for an uncovered window")
	}
	if l := frontendLink(t, apps); l.RequestCount != 7 {
		t.Errorf("request rate = %v; want the PromQL 7", l.RequestCount)
	}
}

// A slow ClickHouse must not use up the caller's deadline: it gets only part
// of it, and the PromQL fallback still answers in time.
func TestBuild_SlowClickHouseLeavesTimeForFallback(t *testing.T) {
	ch := &fakeClickHouse{buckets: fullBuckets(testEnd, 3), delay: 1500 * time.Millisecond}
	s, _ := newTestService(t, ch, SourceClickHouse)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	apps, err := s.Build(ctx, testParams)
	if err != nil {
		t.Fatalf("Build = %v; want the PromQL map", err)
	}
	if l := frontendLink(t, apps); l.RequestCount != 7 {
		t.Errorf("request rate = %v; want the PromQL 7", l.RequestCount)
	}
}

// A caller that has already given up gets an error, not an empty map.
func TestBuild_ExpiredContextIsAnError(t *testing.T) {
	ch := &fakeClickHouse{}
	s, _ := newTestService(t, ch, SourcePrometheus)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if apps, err := s.Build(ctx, testParams); err == nil {
		t.Errorf("Build on a cancelled context = %d apps, nil error; want an error", len(apps))
	}
}

// Shadow serves the PromQL map and compares in the background at most once
// per interval for the same window.
func TestBuild_ShadowServesPrometheusAndRateLimits(t *testing.T) {
	ch := &fakeClickHouse{buckets: fullBuckets(testEnd, 3)}
	s, promCalls := newTestService(t, ch, SourceShadow)

	for i := 0; i < 3; i++ {
		apps, err := s.Build(context.Background(), testParams)
		if err != nil {
			t.Fatal(err)
		}
		if l := frontendLink(t, apps); l.RequestCount != 7 {
			t.Fatalf("shadow served request rate %v; want the PromQL 7", l.RequestCount)
		}
		waitIdle(t, s)
	}
	if got := promCalls.Load(); got != 3*int64(len(Queries)) {
		t.Errorf("prometheus queries = %d; want %d", got, 3*len(Queries))
	}
	if got := ch.coverCalls.Load(); got != 1 {
		t.Errorf("clickhouse comparisons = %d; want 1 within the shadow interval", got)
	}
}

func waitIdle(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for s.shadowBusy.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// Only one shadow build runs at a time; others are skipped, not queued.
func TestShadow_OneBuildAtATime(t *testing.T) {
	ch := &fakeClickHouse{buckets: fullBuckets(testEnd, 3), block: make(chan struct{})}
	s, _ := newTestService(t, ch, SourceShadow)

	if _, err := s.Build(context.Background(), testParams); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for ch.coverCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	other := testParams
	other.WorkloadNamespace = "shop" // a different key: not rate-limited
	if _, err := s.Build(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := ch.coverCalls.Load(); got != 1 {
		t.Errorf("shadow builds started = %d while one runs; want 1", got)
	}
	close(ch.block)
	waitIdle(t, s)
}

// Rate-limit entries older than the interval are dropped, so the map stays
// bounded whatever windows callers ask for.
func TestShadow_PrunesOldKeys(t *testing.T) {
	ch := &fakeClickHouse{buckets: fullBuckets(testEnd, 3)}
	s, _ := newTestService(t, ch, SourceShadow)
	s.lastShadow = map[string]time.Time{"86400||": time.Now().Add(-2 * shadowInterval)}

	if _, err := s.Build(context.Background(), testParams); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	s.shadowMu.Lock()
	defer s.shadowMu.Unlock()
	if _, stale := s.lastShadow["86400||"]; stale || len(s.lastShadow) != 1 {
		t.Errorf("lastShadow = %v; want only the new key", s.lastShadow)
	}
}

// Without a ClickHouse client every source is plain PromQL.
func TestBuild_NoClickHouseIsPrometheus(t *testing.T) {
	ch := &fakeClickHouse{buckets: fullBuckets(testEnd, 3)}
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
