package loki

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQuery_PassesParams(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("query"); got != `{job="x"}` {
			t.Errorf("query = %q", got)
		}
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	got, err := c.Query(context.Background(), `{job="x"}`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"status":"success"}` {
		t.Errorf("body %s", got)
	}
}

func TestHandlers_Series_PromotesSingleString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		matchers := r.URL.Query()["match[]"]
		if len(matchers) != 1 || matchers[0] != `{job="x"}` {
			t.Errorf("match[] = %v", matchers)
		}
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	hs := Handlers(New(srv.URL, &http.Client{Timeout: 5 * time.Second}))
	if _, err := hs["loki_series"](context.Background(), map[string]any{"match[]": `{job="x"}`}); err != nil {
		t.Fatal(err)
	}
}

func TestQueryRange_RequiresQuery(t *testing.T) {
	c := New("http://example", nil)
	if _, err := c.QueryRange(context.Background(), "", "1", "2", "", "", ""); err == nil {
		t.Error("expected error for missing query")
	}
}

func TestSeries_RequiresMatcher(t *testing.T) {
	c := New("http://example", nil)
	if _, err := c.Series(context.Background(), nil, "", ""); err == nil {
		t.Error("expected error for missing matcher")
	}
}

func TestLabelValues_RequiresLabel(t *testing.T) {
	c := New("http://example", nil)
	if _, err := c.LabelValues(context.Background(), "", "", "", ""); err == nil {
		t.Error("expected error for missing label")
	}
}

func TestExtraHeaders_Sent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Scope-OrgID"); got != "tenant-1" {
			t.Errorf("X-Scope-OrgID = %q", got)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	c.ExtraHeaders = http.Header{"X-Scope-OrgID": []string{"tenant-1"}}
	if _, err := c.Query(context.Background(), `{job="x"}`, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestGet_PropagatesHTTP4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad query"))
	}))
	defer srv.Close()
	c := New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	_, err := c.Query(context.Background(), `(invalid`, "", "")
	if err == nil || !contains(err.Error(), "HTTP 400") {
		t.Errorf("expected HTTP 400 error, got %v", err)
	}
}

// One unit test per handler — same pattern as prometheus.
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
		{"loki_query", map[string]any{"query": `{job="x"}`, "limit": "10"}, wantReq{path: "/loki/api/v1/query", query: map[string]string{"query": `{job="x"}`, "limit": "10"}}},
		{"loki_query_range", map[string]any{"query": `{job="x"}`, "start": "1", "end": "2", "direction": "backward"}, wantReq{path: "/loki/api/v1/query_range", query: map[string]string{"query": `{job="x"}`, "start": "1", "end": "2", "direction": "backward"}}},
		{"loki_labels", map[string]any{"start": "1", "end": "2"}, wantReq{path: "/loki/api/v1/labels", query: map[string]string{"start": "1", "end": "2"}}},
		{"loki_label_values", map[string]any{"label": "job", "start": "1"}, wantReq{path: "/loki/api/v1/label/job/values", query: map[string]string{"start": "1"}}},
		{"loki_series", map[string]any{"match[]": []any{`{job="x"}`}, "start": "1"}, wantReq{path: "/loki/api/v1/series", query: map[string]string{"start": "1"}}},
	}

	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.want.path {
					t.Errorf("path = %q; want %q", r.URL.Path, tc.want.path)
				}
				for k, v := range tc.want.query {
					if got := r.URL.Query().Get(k); got != v {
						t.Errorf("query[%s] = %q; want %q", k, got, v)
					}
				}
				_, _ = w.Write([]byte(`{"status":"success"}`))
			}))
			defer srv.Close()
			hs := Handlers(New(srv.URL, &http.Client{Timeout: 5 * time.Second}))
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
		{[]any{"a", 42}, []string{"a"}},
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

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
