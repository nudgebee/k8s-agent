package prometheus

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
	c := New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	return c, srv
}

func TestQuery_PassesParamsAndReturnsRaw(t *testing.T) {
	wantBody := `{"status":"success","data":{"resultType":"vector","result":[]}}`
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path = %q; want /api/v1/query", r.URL.Path)
		}
		if got := r.URL.Query().Get("query"); got != "up" {
			t.Errorf("query param = %q; want up", got)
		}
		if got := r.URL.Query().Get("time"); got != "1700000000" {
			t.Errorf("time param = %q; want 1700000000", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(wantBody))
	})
	defer srv.Close()

	got, err := c.Query(context.Background(), "up", "1700000000", "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("got %s\nwant %s", got, wantBody)
	}
}

func TestQueryRange_RequiresAllParams(t *testing.T) {
	c := New("http://example", nil)
	if _, err := c.QueryRange(context.Background(), "", "1", "2", "1m", ""); err == nil {
		t.Error("expected error for missing query")
	}
	if _, err := c.QueryRange(context.Background(), "up", "", "2", "1m", ""); err == nil {
		t.Error("expected error for missing start")
	}
}

func TestLabelValues_EncodesLabelInPath(t *testing.T) {
	// `?` MUST be encoded in a path segment (it would otherwise start the query
	// string). Verifies url.PathEscape is doing its job.
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		want := "/api/v1/label/job%3Ffoo/values"
		if r.URL.EscapedPath() != want {
			t.Errorf("escaped path = %q; want %q", r.URL.EscapedPath(), want)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":[]}`))
	})
	defer srv.Close()

	if _, err := c.LabelValues(context.Background(), "job?foo", "", "", nil); err != nil {
		t.Fatalf("LabelValues: %v", err)
	}
}

func TestSeries_RequiresMatcher(t *testing.T) {
	c := New("http://example", nil)
	if _, err := c.Series(context.Background(), nil, "", ""); err == nil {
		t.Error("expected error for missing matcher")
	}
}

func TestGet_PropagatesHTTP4xx(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
	})
	defer srv.Close()

	_, err := c.Query(context.Background(), "((", "", "")
	if err == nil {
		t.Fatal("expected error for HTTP 400")
	}
	if !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "parse error") {
		t.Errorf("error %q does not mention status or body", err.Error())
	}
}

func TestExtraHeaders_Sent(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Scope-OrgID"); got != "tenant-42" {
			t.Errorf("X-Scope-OrgID = %q; want tenant-42", got)
		}
		_, _ = w.Write([]byte(`{}`))
	})
	defer srv.Close()
	c.ExtraHeaders = http.Header{"X-Scope-OrgID": []string{"tenant-42"}}

	if _, err := c.Query(context.Background(), "up", "", ""); err != nil {
		t.Fatal(err)
	}
}

// TestURLQueryString_Appended verifies PROMETHEUS_URL_QUERY_STRING is merged
// into the request query string alongside the query params.
func TestURLQueryString_Appended(t *testing.T) {
	var gotRegion, gotQuery string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		gotRegion = r.URL.Query().Get("region")
		gotQuery = r.URL.Query().Get("query")
		_, _ = w.Write([]byte(`{}`))
	})
	defer srv.Close()
	c.URLQueryString = "?region=us-east" // leading ? tolerated

	if _, err := c.Query(context.Background(), "up", "", ""); err != nil {
		t.Fatal(err)
	}
	if gotRegion != "us-east" {
		t.Errorf("region = %q; want us-east", gotRegion)
	}
	if gotQuery != "up" {
		t.Errorf("query = %q; want up (existing params preserved)", gotQuery)
	}
}

func TestAppendQueryString(t *testing.T) {
	cases := []struct{ url, extra, want string }{
		{"http://h/api/v1/query?query=up", "region=us", "http://h/api/v1/query?query=up&region=us"},
		{"http://h/api/v1/labels", "region=us", "http://h/api/v1/labels?region=us"},
		{"http://h/api/v1/labels", "?region=us", "http://h/api/v1/labels?region=us"},
		{"http://h/api/v1/labels", "", "http://h/api/v1/labels"},
	}
	for _, tc := range cases {
		if got := appendQueryString(tc.url, tc.extra); got != tc.want {
			t.Errorf("appendQueryString(%q,%q) = %q; want %q", tc.url, tc.extra, got, tc.want)
		}
	}
}

func TestHandlers_DispatchesViaActionName(t *testing.T) {
	wantBody := `{"status":"success","data":[]}`
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(wantBody))
	})
	defer srv.Close()

	hs := Handlers(c)
	if _, ok := hs["prometheus_query"]; !ok {
		t.Fatal("Handlers missing prometheus_query")
	}

	got, err := hs["prometheus_query"](context.Background(), map[string]any{"query": "up"})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	raw, ok := got.(json.RawMessage)
	if !ok {
		t.Fatalf("handler returned %T; want json.RawMessage", got)
	}
	if string(raw) != wantBody {
		t.Errorf("got %s\nwant %s", raw, wantBody)
	}
}

// One unit test per handler to exercise param marshaling without needing
// a real Prometheus container. Verifies path, method, and that recognised
// params land where they should.
func TestHandlers_AllActionsRoundtrip(t *testing.T) {
	type wantReq struct {
		path  string
		query map[string]string
	}
	cases := []struct {
		action string
		params map[string]any
		want   wantReq
	}{
		{
			"prometheus_query",
			map[string]any{"query": "up", "time": "1700000000", "timeout": "5s"},
			wantReq{path: "/api/v1/query", query: map[string]string{"query": "up", "time": "1700000000", "timeout": "5s"}},
		},
		{
			"prometheus_query_range",
			map[string]any{"query": "up", "start": "1", "end": "2", "step": "1s"},
			wantReq{path: "/api/v1/query_range", query: map[string]string{"query": "up", "start": "1", "end": "2", "step": "1s"}},
		},
		{
			// /api/v1/labels (list of all label NAMES). The
			// `prometheus_labels` action lives in the enrichers package now
			// (Finding-wrapped label-VALUES query) — registered separately
			// in cmd/agent/main.go after this primitive map is merged.
			"prometheus_label_names",
			map[string]any{"start": "1", "end": "2"},
			wantReq{path: "/api/v1/labels", query: map[string]string{"start": "1", "end": "2"}},
		},
		{
			"prometheus_label_values",
			map[string]any{"label": "job", "start": "1"},
			wantReq{path: "/api/v1/label/job/values", query: map[string]string{"start": "1"}},
		},
		{
			"prometheus_series",
			map[string]any{"match[]": []any{`up`, `down`}, "start": "1"},
			wantReq{path: "/api/v1/series", query: map[string]string{"start": "1"}},
		},
		{
			"prometheus_alerts",
			nil,
			wantReq{path: "/api/v1/alerts", query: map[string]string{}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			cli, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.want.path {
					t.Errorf("path = %q; want %q", r.URL.Path, tc.want.path)
				}
				for k, v := range tc.want.query {
					if got := r.URL.Query().Get(k); got != v {
						t.Errorf("query[%s] = %q; want %q", k, got, v)
					}
				}
				_, _ = w.Write([]byte(`{"status":"success"}`))
			})
			defer srv.Close()
			hs := Handlers(cli)
			h, ok := hs[tc.action]
			if !ok {
				t.Fatalf("handler not registered: %s", tc.action)
			}
			if _, err := h(context.Background(), tc.params); err != nil {
				t.Fatalf("handler err: %v", err)
			}
		})
	}
}

func TestStrSlice_Variants(t *testing.T) {
	cases := []struct {
		in   any
		want []string
	}{
		{nil, nil},
		{`x`, []string{"x"}},
		{[]string{"a", "b"}, []string{"a", "b"}},
		{[]any{"a", "b"}, []string{"a", "b"}},
		{[]any{"a", 42}, []string{"a"}}, // non-string entries dropped
		{42, nil},
	}
	for _, c := range cases {
		got := strSlice(map[string]any{"k": c.in}, "k")
		if len(got) != len(c.want) {
			t.Errorf("strSlice(%v) = %v; want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("strSlice(%v)[%d] = %q; want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestHandlers_PromotesSingleStringMatcher(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		matchers := r.URL.Query()["match[]"]
		if len(matchers) != 1 || matchers[0] != `up{job="foo"}` {
			t.Errorf("match[] = %v; want one entry %q", matchers, `up{job="foo"}`)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":[]}`))
	})
	defer srv.Close()

	hs := Handlers(c)
	_, err := hs["prometheus_series"](context.Background(), map[string]any{"match[]": `up{job="foo"}`})
	if err != nil {
		t.Fatal(err)
	}
}
