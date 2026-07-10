package chronosphere

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQueryTraces_PostsBodyAndAuth(t *testing.T) {
	var path, body, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"traces":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	c.APIKey = "tk-1"
	if _, err := c.QueryTraces(context.Background(), map[string]any{"service": "frontend"}); err != nil {
		t.Fatal(err)
	}
	if path != "/api/v1/data/traces" {
		t.Errorf("path = %q", path)
	}
	if auth != "Bearer tk-1" {
		t.Errorf("Authorization = %q", auth)
	}
	if !strings.Contains(body, `"service":"frontend"`) {
		t.Errorf("body = %s", body)
	}
}

func TestQueryTraces_RequiresParams(t *testing.T) {
	c := New("http://x", nil)
	if _, err := c.QueryTraces(context.Background(), nil); err == nil {
		t.Error("expected error for nil params")
	}
}

func TestQueryTraces_NoURL(t *testing.T) {
	c := New("", nil)
	if _, err := c.QueryTraces(context.Background(), map[string]any{}); err == nil {
		t.Error("expected error for missing URL")
	}
}

func TestQueryTraces_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	_, err := c.QueryTraces(context.Background(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("expected HTTP 401 error, got %v", err)
	}
}

func TestHandlers_DispatchEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"traces":[]}`))
	}))
	defer srv.Close()
	hs := Handlers(New(srv.URL, nil))
	if _, err := hs["chronosphere_query_traces"](context.Background(), map[string]any{"x": 1}); err != nil {
		t.Fatal(err)
	}
}
