package httpproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDo_NamedTarget(t *testing.T) {
	got := struct {
		path, method string
		body         string
		hdrName      string
		query        string
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.method = r.Method
		got.hdrName = r.Header.Get("X-Test")
		got.query = r.URL.RawQuery
		body, _ := io.ReadAll(r.Body)
		got.body = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New(map[string]string{"grafana": srv.URL}, &http.Client{Timeout: 5 * time.Second})
	resp, err := c.Do(context.Background(), &Request{
		Target:  "grafana",
		Method:  "POST",
		Path:    "/api/dashboards/db",
		Headers: map[string]string{"X-Test": "v1"},
		Query:   url.Values{"q": []string{"x"}},
		Body:    json.RawMessage(`{"name":"x"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if got.path != "/api/dashboards/db" || got.method != "POST" {
		t.Errorf("got %+v", got)
	}
	if got.hdrName != "v1" || got.query != "q=x" {
		t.Errorf("hdr/query: %+v", got)
	}
	if got.body != `{"name":"x"}` {
		t.Errorf("body = %q", got.body)
	}
	if !strings.Contains(resp.Body, `"ok":true`) {
		t.Errorf("response body = %q", resp.Body)
	}
}

func TestDo_RejectsUnknownTarget(t *testing.T) {
	c := New(map[string]string{"grafana": "http://x"}, nil)
	_, err := c.Do(context.Background(), &Request{Target: "secret-internal"})
	if err == nil {
		t.Error("expected error for unconfigured target")
	}
}

func TestDo_RejectsExplicitURLByDefault(t *testing.T) {
	c := New(map[string]string{"grafana": "http://g"}, nil)
	_, err := c.Do(context.Background(), &Request{Target: "http://attacker.example/x"})
	if err == nil {
		t.Error("expected error for explicit URL without wildcard")
	}
}

func TestDo_AllowsExplicitURLWithWildcard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()
	c := New(map[string]string{"*": "ignored"}, nil)
	resp, err := c.Do(context.Background(), &Request{Target: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestDo_RequiresTarget(t *testing.T) {
	c := New(nil, nil)
	if _, err := c.Do(context.Background(), &Request{}); err == nil {
		t.Error("expected error for missing target")
	}
}

func TestDo_DefaultMethodIsGet(t *testing.T) {
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
	}))
	defer srv.Close()
	c := New(map[string]string{"x": srv.URL}, nil)
	if _, err := c.Do(context.Background(), &Request{Target: "x", Path: "/"}); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet {
		t.Errorf("method = %q; want GET", method)
	}
}

func TestParseRequest_AllFields(t *testing.T) {
	r := parseRequest(map[string]any{
		"target": "grafana",
		"method": "POST",
		"path":   "/api/x",
		"headers": map[string]any{
			"X-Foo": "bar",
			"X-Bad": 42, // not a string — dropped
		},
		"query": map[string]any{
			"q":    "x",
			"tags": []any{"a", "b"},
		},
		"body": map[string]any{"k": "v"},
	})
	if r.Target != "grafana" || r.Method != "POST" || r.Path != "/api/x" {
		t.Errorf("scalars: %+v", r)
	}
	if r.Headers["X-Foo"] != "bar" || r.Headers["X-Bad"] != "" {
		t.Errorf("headers: %+v", r.Headers)
	}
	if r.Query.Get("q") != "x" || len(r.Query["tags"]) != 2 {
		t.Errorf("query: %+v", r.Query)
	}
	if string(r.Body) != `{"k":"v"}` {
		t.Errorf("body: %s", r.Body)
	}
}

func TestHandlers_RegisterAndDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()
	hs := Handlers(New(map[string]string{"g": srv.URL}, nil))
	if _, ok := hs["http_proxy_request"]; !ok {
		t.Fatal("http_proxy_request not registered")
	}
	got, err := hs["http_proxy_request"](context.Background(), map[string]any{
		"target": "g", "path": "/x",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := got.(*Response)
	if resp == nil || resp.StatusCode != 200 {
		t.Errorf("response = %+v", resp)
	}
}
