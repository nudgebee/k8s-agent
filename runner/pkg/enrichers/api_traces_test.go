package enrichers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nudgebee/nudgebee-agent/pkg/clickhouse"
)

// TestApiTraces_NoClickHouseReturnsEmpty covers the "CH not configured"
// path. The legacy get_application_traces would crash on the run_query
// call in that case; we surface a clean empty Finding with `error` set
// so the api-server caller can render the empty state.
func TestApiTraces_NoClickHouseReturnsEmpty(t *testing.T) {
	a := NewAPITracesEnricher(nil, "acc-1")
	resp, err := a.Handler()(context.Background(), map[string]any{
		"destination_workload_name":      "web",
		"destination_workload_namespace": "shop",
	})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.(map[string]any)
	if r["success"] != true {
		t.Fatalf("expected success=true; got %+v", r)
	}
	body := unwrapJSONBlock(t, r)
	if body["name"] != "api_traces_enricher" {
		t.Errorf("name = %v", body["name"])
	}
	if rows := body["data"].([]any); len(rows) != 0 {
		t.Errorf("data = %v; want []", rows)
	}
	if body["error"] != "clickhouse: not configured" {
		t.Errorf("error = %v", body["error"])
	}
}

// TestApiTraces_BuildsTimeWindowQuery stands up a fake ClickHouse and
// confirms the agent issues a query with the time-bounded SELECT shape, the
// destination/source workload filters, and a row-major result mapping back
// into per-row dicts.
func TestApiTraces_BuildsTimeWindowQuery(t *testing.T) {
	var seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		seenBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		// Two rows, two columns — ClickHouse JSONCompact shape.
		_, _ = w.Write([]byte(`{"meta":[{"name":"TraceId","type":"String"},{"name":"SpanName","type":"String"}],"data":[["abc","GET /api"],["def","POST /api"]]}`))
	}))
	defer srv.Close()

	c := clickhouse.New(clickhouse.Config{Host: strings.TrimPrefix(srv.URL, "http://"), Database: "default"})
	a := NewAPITracesEnricher(c, "acc-1")
	resp, err := a.Handler()(context.Background(), map[string]any{
		"destination_workload_name":      "web",
		"destination_workload_namespace": "shop",
		"duration_minutes":               5,
		"max_traces":                     50,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(seenBody, "FROM otel_traces") {
		t.Errorf("query missing otel_traces select: %s", seenBody)
	}
	if !strings.Contains(seenBody, "parseDateTimeBestEffort") {
		t.Errorf("query missing time filter: %s", seenBody)
	}
	if !strings.Contains(seenBody, "LIMIT 50") {
		t.Errorf("query missing limit: %s", seenBody)
	}
	body := unwrapJSONBlock(t, resp.(map[string]any))
	rows := body["data"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows = %d; want 2", len(rows))
	}
	first := rows[0].(map[string]any)
	if first["TraceId"] != "abc" || first["SpanName"] != "GET /api" {
		t.Errorf("row[0] = %+v", first)
	}
}

// TestApiTraces_TraceIDFastPath: with trace_id set we skip the
// destination-workload validation and run a direct lookup. Mirrors
func TestApiTraces_TraceIDFastPath(t *testing.T) {
	var seenBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		seenBody = string(buf[:n])
		_, _ = w.Write([]byte(`{"meta":[{"name":"TraceId","type":"String"}],"data":[]}`))
	}))
	defer srv.Close()
	c := clickhouse.New(clickhouse.Config{Host: strings.TrimPrefix(srv.URL, "http://")})
	a := NewAPITracesEnricher(c, "acc-1")
	_, err := a.Handler()(context.Background(), map[string]any{
		"trace_id": "00-deadbeefdeadbeefdeadbeefdeadbeef-1111111111111111-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seenBody, "TraceId = 'deadbeefdeadbeefdeadbeefdeadbeef'") {
		t.Errorf("traceparent not unwrapped: %s", seenBody)
	}
}

func TestApiTraces_RejectsUnsafeOrderBy(t *testing.T) {
	cases := map[string]bool{
		"Timestamp desc":         true,
		"Timestamp ASC":          true,
		"col1, col2 asc":         true,
		"foo; DROP TABLE traces": false,
		"col1; DELETE":           false,
		"col(1)":                 false,
	}
	for in, want := range cases {
		if got := isSafeOrderBy(in); got != want {
			t.Errorf("isSafeOrderBy(%q) = %v; want %v", in, got, want)
		}
	}
}

// unwrapJSONBlock walks Finding → evidence[0].data → JSON-parse → array →
// first block → JSON-parse the inner string. The api-server caller goes
// through this same dance via FormatEvidenceResponseFromAgent.
func unwrapJSONBlock(t *testing.T, finding map[string]any) map[string]any {
	t.Helper()
	ev := finding["findings"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(ev["data"].(string)), &blocks); err != nil {
		t.Fatalf("evidence.data not JSON array: %v", err)
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(blocks[0]["data"].(string)), &inner); err != nil {
		t.Fatalf("inner JsonBlock data not JSON: %v", err)
	}
	return inner
}
