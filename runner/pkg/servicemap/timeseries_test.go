package servicemap

import (
	"context"
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

const emptyVector = `{"status":"success","data":{"resultType":"vector","result":[]}}`

// recorder is an httptest handler that records every PromQL query it gets and
// answers from respond (empty vector when respond returns "").
type recorder struct {
	mu      sync.Mutex
	queries []string
	paths   []string
	auth    []string
	respond func(q string) (status int, body string)
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query().Get("query")
	r.mu.Lock()
	r.queries = append(r.queries, q)
	r.paths = append(r.paths, req.URL.Path)
	user, pass, _ := req.BasicAuth()
	r.auth = append(r.auth, user+":"+pass)
	r.mu.Unlock()
	status, body := http.StatusOK, ""
	if r.respond != nil {
		status, body = r.respond(q)
	}
	if body == "" {
		body = emptyVector
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

// edgeResponse answers the HTTP request query with one frontend → backend edge.
func edgeResponse(q string) (int, string) {
	if strings.Contains(q, "rate(container_http_requests_total{") {
		return http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"src_workload_name":"frontend","src_workload_namespace":"shop","destination_workload_name":"backend","destination_workload_namespace":"shop","status":"200"},"value":[1,"42"]}
		]}}`
	}
	return http.StatusOK, ""
}

func newTSService(t *testing.T, prom, ts http.Handler) *Service {
	t.Helper()
	promSrv := httptest.NewServer(prom)
	t.Cleanup(promSrv.Close)
	tsSrv := httptest.NewServer(ts)
	t.Cleanup(tsSrv.Close)
	s := New(prometheus.New(promSrv.URL, &http.Client{Timeout: 5 * time.Second}), "prod-east")
	s.Source = SourceTimeSeries
	s.TS = prometheus.New(tsSrv.URL, &http.Client{Timeout: 5 * time.Second})
	return s
}

func TestParseSource_TimeSeries(t *testing.T) {
	got, ok := ParseSource(" TimeSeries ")
	if !ok || got != SourceTimeSeries {
		t.Fatalf("ParseSource = %q, %v; want timeseries, true", got, ok)
	}
}

// Edge and container queries go to the TimeSeries API without a cluster
// filter; only the kube_* inventory queries go to Prometheus.
func TestBuild_TimeSeries_SplitsQueries(t *testing.T) {
	prom := &recorder{}
	ts := &recorder{respond: edgeResponse}
	s := newTSService(t, prom, ts)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	apps, err := s.Build(ctx, FilterParams{})
	if err != nil {
		t.Fatal(err)
	}

	kube := kubeKeys(Queries)
	if got, want := len(prom.all()), len(kube); got != want {
		t.Errorf("prometheus got %d queries; want %d (kube_* only)", got, want)
	}
	for _, q := range prom.all() {
		if !strings.Contains(q, "kube_") {
			t.Errorf("non-inventory query sent to prometheus: %s", q)
		}
	}
	if got, want := len(ts.all()), len(Queries)-len(kube); got != want {
		t.Errorf("timeseries got %d queries; want %d", got, want)
	}
	for _, q := range ts.all() {
		if strings.Contains(q, "kube_") {
			t.Errorf("inventory query sent to timeseries: %s", q)
		}
		if strings.Contains(q, `cluster="`) {
			t.Errorf("timeseries query carries a cluster filter: %s", q)
		}
	}

	found := false
	for _, a := range apps {
		if a.ID.Name == "frontend" && len(a.Upstreams) > 0 && a.Upstreams[0].RequestCount == 42 {
			found = true
		}
	}
	if !found {
		t.Errorf("frontend → backend edge with 42 requests not in the map: %+v", apps)
	}
}

// A query that fails on the TimeSeries API is retried on Prometheus.
func TestBuild_TimeSeries_RetriesFailedQueriesOnPrometheus(t *testing.T) {
	prom := &recorder{respond: edgeResponse}
	ts := &recorder{respond: func(q string) (int, string) {
		if strings.Contains(q, "rate(container_http_requests_total{") {
			return http.StatusInternalServerError, `{"status":"error","error":"boom"}`
		}
		return http.StatusOK, ""
	}}
	s := newTSService(t, prom, ts)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	apps, err := s.Build(ctx, FilterParams{})
	if err != nil {
		t.Fatal(err)
	}
	retried := false
	for _, q := range prom.all() {
		if strings.Contains(q, "rate(container_http_requests_total{") {
			retried = true
			if !strings.Contains(q, `cluster="prod-east"`) {
				t.Errorf("retry on prometheus lost the cluster filter: %s", q)
			}
		}
	}
	if !retried {
		t.Fatal("failed timeseries query was not retried on prometheus")
	}
	found := false
	for _, a := range apps {
		if a.ID.Name == "frontend" && len(a.Upstreams) > 0 {
			found = true
		}
	}
	if !found {
		t.Error("edge from the prometheus retry is missing from the map")
	}
}

// Without a TimeSeries client the source falls back to Prometheus.
func TestSource_TimeSeriesWithoutClientIsPrometheus(t *testing.T) {
	s := New(prometheus.New("http://prom", nil), "")
	s.Source = SourceTimeSeries
	if got := s.source(); got != SourcePrometheus {
		t.Errorf("source() = %q; want prometheus", got)
	}
	s.TS = prometheus.New("http://ch/prometheus", nil)
	if got := s.source(); got != SourceTimeSeries {
		t.Errorf("source() = %q; want timeseries", got)
	}
}

// UseTimeSeries targets <ClickHouse>/<path>/api/v1 with the ClickHouse
// user's credentials.
func TestUseTimeSeries_PathAndAuth(t *testing.T) {
	ts := &recorder{}
	srv := httptest.NewServer(ts)
	defer srv.Close()

	s := New(prometheus.New("", nil), "")
	s.Source = SourceTimeSeries
	s.UseTimeSeries(&chclient.Client{BaseURL: srv.URL, User: "reader", Password: "secret"}, "/prometheus/")
	if _, err := s.TS.Query(context.Background(), "up", "1", ""); err != nil {
		t.Fatal(err)
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.paths) != 1 || ts.paths[0] != "/prometheus/api/v1/query" {
		t.Errorf("paths = %v; want [/prometheus/api/v1/query]", ts.paths)
	}
	if ts.auth[0] != "reader:secret" {
		t.Errorf("basic auth = %q; want reader:secret", ts.auth[0])
	}

	s.UseTimeSeries(nil, "/prometheus")
	if s.TS == nil {
		t.Error("UseTimeSeries(nil) cleared the existing client")
	}
}

// The timeseries source creates nothing in ClickHouse.
func TestStartSummary_TimeSeriesCreatesNothing(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	s := New(prometheus.New("", nil), "")
	s.UseClickHouse(&chclient.Client{BaseURL: srv.URL, HTTP: srv.Client()}, SourceTimeSeries, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartSummary(ctx)
	time.Sleep(100 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Errorf("StartSummary sent %d requests to ClickHouse; want 0", n)
	}
}
