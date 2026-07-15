package jaeger

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(handler http.HandlerFunc) (*Client, *httptest.Server) {
	srv := httptest.NewServer(handler)
	return New(srv.URL, &http.Client{Timeout: 5 * time.Second}), srv
}

func TestTraces_PassesQueryString(t *testing.T) {
	var path string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	defer srv.Close()
	if _, err := c.Traces(context.Background(), map[string]any{
		"service":   "frontend",
		"operation": "GET /",
		"limit":     20,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, "/api/traces?") {
		t.Errorf("path = %q", path)
	}
	if !strings.Contains(path, "service=frontend") || !strings.Contains(path, "limit=20") {
		t.Errorf("query string missing params: %q", path)
	}
}

func TestTraceByID_RequiresID(t *testing.T) {
	c := New("http://x", nil)
	if _, err := c.TraceByID(context.Background(), ""); err == nil {
		t.Error("expected error")
	}
}

func TestTraceByID_BuildsPath(t *testing.T) {
	var path string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	defer srv.Close()
	if _, err := c.TraceByID(context.Background(), "abc-123"); err != nil {
		t.Fatal(err)
	}
	if path != "/api/traces/abc-123" {
		t.Errorf("path = %q", path)
	}
}

func TestServices_GetsEndpoint(t *testing.T) {
	var path string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"data":["a"]}`))
	})
	defer srv.Close()
	if _, err := c.Services(context.Background()); err != nil {
		t.Fatal(err)
	}
	if path != "/api/services" {
		t.Errorf("path = %q", path)
	}
}

func TestOperations_RequiresService(t *testing.T) {
	c := New("http://x", nil)
	if _, err := c.Operations(context.Background(), ""); err == nil {
		t.Error("expected error")
	}
}

func TestOperations_BuildsPath(t *testing.T) {
	var path string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	defer srv.Close()
	if _, err := c.Operations(context.Background(), "frontend"); err != nil {
		t.Fatal(err)
	}
	if path != "/api/services/frontend/operations" {
		t.Errorf("path = %q", path)
	}
}

// TestMetrics_FansOutAndRemaps verifies the SPM fan-out: four endpoints are
// hit (calls, errors, latencies@0.95, latencies@0.99), the composer's plural
// services/spanKinds are remapped to singular service/spanKind, metric_type is
// dropped, and the responses are assembled under the expected keys.
func TestMetrics_FansOutAndRemaps(t *testing.T) {
	hits := map[string]string{} // metric path -> raw query
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path] = r.URL.RawQuery
		// Echo the metric name so we can assert the assembly mapping.
		_, _ = w.Write([]byte(`{"path":"` + r.URL.Path + `"}`))
	})
	defer srv.Close()

	raw, err := c.Metrics(context.Background(), map[string]any{
		"services":    "frontend",
		"spanKinds":   "SPAN_KIND_SERVER",
		"metric_type": "should-be-dropped",
		"endTs":       1700000000,
	})
	if err != nil {
		t.Fatal(err)
	}

	// All four endpoints hit.
	for _, p := range []string{"/api/metrics/calls", "/api/metrics/errors", "/api/metrics/latencies"} {
		if _, ok := hits[p]; !ok {
			t.Errorf("missing request to %s (hits: %v)", p, hits)
		}
	}
	// Remap: singular service/spanKind present, plurals + metric_type gone.
	q := hits["/api/metrics/calls"]
	if !strings.Contains(q, "service=frontend") || !strings.Contains(q, "spanKind=SPAN_KIND_SERVER") {
		t.Errorf("calls query = %q; want remapped service/spanKind", q)
	}
	if strings.Contains(q, "services=") || strings.Contains(q, "spanKinds=") || strings.Contains(q, "metric_type=") {
		t.Errorf("calls query = %q; plural/metric_type should be dropped", q)
	}
	if !strings.Contains(q, "endTs=1700000000") {
		t.Errorf("calls query = %q; want endTs passed through", q)
	}

	// Assembled shape.
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("assembled result not JSON object: %v", err)
	}
	for _, k := range []string{"calls", "errors", "latencies_p95", "latencies_p99"} {
		if _, ok := out[k]; !ok {
			t.Errorf("assembled result missing key %q (got %v)", k, out)
		}
	}
	// The two latency sub-queries carry the right quantiles.
	if lat := hits["/api/metrics/latencies"]; !strings.Contains(lat, "quantile=0.9") {
		t.Errorf("latencies query = %q; want a quantile", lat)
	}
}

// TestMetrics_SPMNotAvailable: a 404 from Jaeger (SPM storage not wired up)
// surfaces the friendly legacy message, not a raw HTTP 404.
func TestMetrics_SPMNotAvailable(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`404 page not found`))
	})
	defer srv.Close()
	_, err := c.Metrics(context.Background(), map[string]any{"services": "frontend"})
	if err == nil || !strings.Contains(err.Error(), "SPM metrics not available") {
		t.Errorf("expected friendly SPM-unavailable error, got %v", err)
	}
}

func TestParamsToQuery_TypeCoercion(t *testing.T) {
	got := paramsToQuery(map[string]any{
		"s":  "x",
		"n":  42,
		"f":  1.5,
		"b":  true,
		"sl": []any{"a", "b", 1},
		"ss": []string{"c"},
		"e":  "",
	})
	if got.Get("s") != "x" || got.Get("n") != "42" || got.Get("f") != "1.5" || got.Get("b") != "true" {
		t.Errorf("scalar coercion: %v", got)
	}
	if vs := got["sl"]; len(vs) != 2 || vs[0] != "a" || vs[1] != "b" {
		t.Errorf("slice coercion: %v", vs)
	}
	if got.Has("e") {
		t.Errorf("empty string should be omitted: %v", got)
	}
}

func TestPropagatesHTTPErrors(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("oops"))
	})
	defer srv.Close()
	if _, err := c.Services(context.Background()); err == nil {
		t.Error("expected HTTP 500 error")
	}
}

func TestHandlers_AllRegistered(t *testing.T) {
	hs := Handlers(New("http://x", nil))
	for _, want := range []string{
		"jaeger_query_traces", "jaeger_query_services", "jaeger_query_trace_by_id",
		"jaeger_query_operations", "jaeger_query_metrics",
	} {
		if _, ok := hs[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
}

func TestHandlers_TraceByID_Roundtrip(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/traces/abc" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	defer srv.Close()
	hs := Handlers(c)
	if _, err := hs["jaeger_query_trace_by_id"](context.Background(), map[string]any{"trace_id": "abc"}); err != nil {
		t.Fatal(err)
	}
}

func TestToken_SetsBearer(t *testing.T) {
	var auth string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	defer srv.Close()
	c.Token = "tok123"
	if _, err := c.Services(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok123" {
		t.Errorf("Authorization = %q", auth)
	}
}

func TestToken_AbsentWhenUnset(t *testing.T) {
	var auth string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	defer srv.Close()
	if _, err := c.Services(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		t.Errorf("unexpected Authorization = %q", auth)
	}
}
