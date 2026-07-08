package jaeger

import (
	"context"
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

func TestMetrics_RequiresType(t *testing.T) {
	c := New("http://x", nil)
	if _, err := c.Metrics(context.Background(), "", map[string]any{}); err == nil {
		t.Error("expected error")
	}
}

func TestMetrics_BuildsPath(t *testing.T) {
	var path string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.RequestURI()
		_, _ = w.Write([]byte(`{}`))
	})
	defer srv.Close()
	if _, err := c.Metrics(context.Background(), "latencies", map[string]any{"service": "frontend"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, "/api/metrics/latencies") {
		t.Errorf("path = %q", path)
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
