package elasticsearch

import (
	"context"
	"encoding/json"
	"io"
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

func TestSearch_DSL_PostsBody(t *testing.T) {
	var gotBody string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/logs-2026.05/_search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_, _ = w.Write([]byte(`{"hits":{"total":{"value":0}}}`))
	})
	defer srv.Close()

	body := map[string]any{"size": 10, "query": map[string]any{"match_all": map[string]any{}}}
	_, err := c.Search(context.Background(), "logs-2026.05", "dsl", body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"size":10`) || !strings.Contains(gotBody, "match_all") {
		t.Errorf("body did not pass through: %q", gotBody)
	}
}

func TestSearch_PPL_PostsToPlugins(t *testing.T) {
	var gotPath, gotBody string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		_, _ = w.Write([]byte(`{"schema":[]}`))
	})
	defer srv.Close()
	if _, err := c.Search(context.Background(), "logs-*", "ppl", "source=logs | head 5"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/_plugins/_ppl" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"query":"source=logs | head 5"`) {
		t.Errorf("body = %s", gotBody)
	}
}

func TestSearch_RequiresIndex(t *testing.T) {
	c := New("http://x", nil)
	if _, err := c.Search(context.Background(), "", "dsl", map[string]any{}); err == nil {
		t.Error("expected error for missing index")
	}
}

func TestSearch_RejectsUnknownQueryType(t *testing.T) {
	c := New("http://x", nil)
	if _, err := c.Search(context.Background(), "i", "graphql", map[string]any{}); err == nil {
		t.Error("expected error for unknown query_type")
	}
}

func TestIndices_GetsCatEndpoint(t *testing.T) {
	var path string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.RequestURI()
		_, _ = w.Write([]byte(`[]`))
	})
	defer srv.Close()
	if _, err := c.Indices(context.Background()); err != nil {
		t.Fatal(err)
	}
	if path != "/_cat/indices?format=json" {
		t.Errorf("path = %q", path)
	}
}

func TestIndexFields_GetsMapping(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/logs-1/_mapping" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{}`))
	})
	defer srv.Close()
	if _, err := c.IndexFields(context.Background(), "logs-1"); err != nil {
		t.Fatal(err)
	}
}

func TestIndexFields_RequiresIndex(t *testing.T) {
	c := New("http://x", nil)
	if _, err := c.IndexFields(context.Background(), ""); err == nil {
		t.Error("expected error")
	}
}

func TestFieldValues_BuildsTermsAgg(t *testing.T) {
	var body string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"aggregations":{}}`))
	})
	defer srv.Close()
	if _, err := c.FieldValues(context.Background(), "logs-1", "service.name", 100); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"field":"service.name"`) || !strings.Contains(body, `"size":100`) {
		t.Errorf("body = %s", body)
	}
}

func TestFieldValues_DefaultsLimit(t *testing.T) {
	var body string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{}`))
	})
	defer srv.Close()
	_, _ = c.FieldValues(context.Background(), "logs-1", "x", 0)
	if !strings.Contains(body, `"size":5000`) {
		t.Errorf("default limit not applied: %s", body)
	}
}

func TestAPIKeyHeader(t *testing.T) {
	var got string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	})
	defer srv.Close()
	c.APIKey = "test-key"
	_, _ = c.Indices(context.Background())
	if got != "ApiKey test-key" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestBasicAuth(t *testing.T) {
	var u, p string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ = r.BasicAuth()
		_, _ = w.Write([]byte(`[]`))
	})
	defer srv.Close()
	c.Username = "elastic"
	c.Password = "changeme"
	_, _ = c.Indices(context.Background())
	if u != "elastic" || p != "changeme" {
		t.Errorf("BasicAuth = %s/%s", u, p)
	}
}

func TestHandlers_DSL_Roundtrip(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"hits":[]}`))
	})
	defer srv.Close()

	hs := Handlers(c)
	got, err := hs["query_es"](context.Background(), map[string]any{
		"index":      "logs-1",
		"query_type": "dsl",
		"query":      map[string]any{"size": 5, "query": map[string]any{"match_all": map[string]any{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := got.(json.RawMessage)
	if !strings.Contains(string(raw), `"hits":[]`) {
		t.Errorf("body = %s", raw)
	}
}

func TestHandlers_PPL(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"query":"source=logs"`) {
			t.Errorf("body = %s", body)
		}
		_, _ = w.Write([]byte(`{}`))
	})
	defer srv.Close()
	hs := Handlers(c)
	if _, err := hs["query_es"](context.Background(), map[string]any{
		"index": "logs-1", "query_type": "ppl", "query": "source=logs",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHandlers_AllRegistered(t *testing.T) {
	hs := Handlers(New("http://x", nil))
	for _, want := range []string{"query_es", "query_es_indices", "query_es_index_field", "query_es_field_index_values"} {
		if _, ok := hs[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
}
