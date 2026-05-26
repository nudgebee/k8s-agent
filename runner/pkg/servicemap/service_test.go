package servicemap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

// TestService_Build_Integration_HTTP wires the Service to an httptest server
// that responds to /api/v1/query_range with a synthetic Prometheus payload.
// Verifies parallel fetching, world build, and rendering all run end-to-end.
func TestService_Build_FetchesAndRenders(t *testing.T) {
	var fetchCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount.Add(1)
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		// Return synthetic data for the queries we care about; empty for others.
		switch {
		case strings.Contains(q, "kube_pod_info"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{"pod":"frontend-abc-1","namespace":"shop","pod_ip":"10.0.0.1","created_by_kind":"ReplicaSet","created_by_name":"frontend-abc"},"values":[[1,"1"]]},
				{"metric":{"pod":"backend-def-1","namespace":"shop","pod_ip":"10.0.0.2","created_by_kind":"ReplicaSet","created_by_name":"backend-def"},"values":[[1,"1"]]}
			]}}`))
		case strings.Contains(q, "container_net_tcp_successful_connects"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{"src_workload_name":"frontend","src_workload_namespace":"shop","destination_workload_name":"backend","destination_workload_namespace":"shop"},"values":[[1,"5"]]}
			]}}`))
		// HTTP requests count: rate(container_http_requests_total{...}[$RANGE])
		case strings.Contains(q, "rate(container_http_requests_total{"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
				{"metric":{"src_workload_name":"frontend","src_workload_namespace":"shop","destination_workload_name":"backend","destination_workload_namespace":"shop"},"values":[[1,"100"]]}
			]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
		}
	}))
	defer srv.Close()

	prom := prometheus.New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	s := New(prom, "prod-east")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	apps, err := s.Build(ctx, FilterParams{})
	if err != nil {
		t.Fatal(err)
	}

	// We sent 65+ queries (len(Queries) ≈ 65); each one fetched once.
	if fetchCount.Load() < 50 {
		t.Errorf("fetchCount = %d; expected ≥50 parallel fetches", fetchCount.Load())
	}

	// Should produce at least frontend + backend.
	names := map[string]bool{}
	for _, a := range apps {
		names[a.ID.Name] = true
	}
	if !names["frontend"] || !names["backend"] {
		t.Errorf("expected frontend + backend in output; got %v", names)
	}

	// Frontend should have an upstream link to backend with HTTP+req=100.
	for _, a := range apps {
		if a.ID.Name != "frontend" {
			continue
		}
		if len(a.Upstreams) == 0 {
			t.Fatal("frontend has no upstreams")
		}
		l := a.Upstreams[0]
		// Upstream Id is a string `namespace:kind:name`.
		if !strings.Contains(l.ID, ":backend") {
			t.Errorf("upstream target Id = %q; want contains :backend", l.ID)
		}
		if l.RequestCount <= 0 {
			t.Errorf("RequestCount = %v", l.RequestCount)
		}
		if l.Protocol != "HTTP" {
			t.Errorf("Protocol = %q", l.Protocol)
		}
	}
}

func TestService_Build_AppliesWorkloadFilter(t *testing.T) {
	// When workload_name is set, ApplicationQueries is used (not Queries),
	// and the SRC_FILTER / DST_FILTER are populated. Verify the filter
	// substitution by inspecting the outgoing PromQL.
	var sawFilteredQuery atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if strings.Contains(q, `src_workload_name=~"frontend"`) {
			sawFilteredQuery.Store(true)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer srv.Close()

	prom := prometheus.New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	s := New(prom, "prod-east")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Build(ctx, FilterParams{WorkloadName: "frontend", WorkloadNamespace: "shop"}); err != nil {
		t.Fatal(err)
	}
	if !sawFilteredQuery.Load() {
		t.Error("expected at least one query with src_workload_name filter")
	}
}

func TestService_Build_RequiresPrometheus(t *testing.T) {
	s := &Service{}
	_, err := s.Build(context.Background(), FilterParams{})
	if err == nil {
		t.Error("expected error when prometheus client unset")
	}
}

func TestService_Build_TolerantOfPartialFetchErrors(t *testing.T) {
	// One failing query should not kill the whole map — the world is built
	// from whatever did succeed.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("query"), "container_oom_kills_total") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer srv.Close()

	prom := prometheus.New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	s := New(prom, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.Build(ctx, FilterParams{})
	if err != nil {
		t.Errorf("partial failure should NOT propagate: %v", err)
	}
}

func TestHandlers_Roundtrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer srv.Close()
	prom := prometheus.New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	hs := Handlers(New(prom, "c1"), "test-account")
	for _, want := range []string{"service_map", "service_map_enricher", "traces_dependency_map"} {
		if _, ok := hs[want]; !ok {
			t.Errorf("handler missing: %s", want)
		}
	}

	// service_map: UI-shaped {data: [...]} wrapper.
	got, err := hs["service_map"](context.Background(), map[string]any{"duration": float64(60)})
	if err != nil {
		t.Fatal(err)
	}
	wrapper, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("service_map returned %T; want map[string]any with `data` key", got)
	}
	if _, hasData := wrapper["data"].([]Application); !hasData {
		t.Errorf("service_map response missing `data: []Application` shape; got %+v", wrapper)
	}

	// service_map_enricher: Finding envelope.
	got, err = hs["service_map_enricher"](context.Background(), map[string]any{"duration": float64(60)})
	if err != nil {
		t.Fatal(err)
	}
	env, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("service_map_enricher returned %T; want Finding envelope", got)
	}
	findings, ok := env["findings"].([]any)
	if !ok || len(findings) == 0 {
		t.Errorf("service_map_enricher missing findings[]: %+v", env)
	}
}

func TestHandlers_ParseFilterParams_NestedWorkloadFilter(t *testing.T) {
	got := parseFilterParams(map[string]any{
		"workload_filter": map[string]any{
			"workload_name":      "frontend",
			"workload_namespace": "shop",
		},
		"duration": float64(120),
	})
	if got.WorkloadName != "frontend" || got.WorkloadNamespace != "shop" || got.Duration != 120 {
		t.Errorf("nested filter not extracted: %+v", got)
	}
}

func TestHandlers_NilService(t *testing.T) {
	hs := Handlers(nil, "")
	if hs != nil {
		t.Error("Handlers(nil, ...) should return nil map")
	}
}
