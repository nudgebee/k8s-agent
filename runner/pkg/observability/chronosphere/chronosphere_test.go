package chronosphere

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestQueryTraces_RetriesTransient503ThenSucceeds(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			// Chronosphere's gRPC UNAVAILABLE body.
			_, _ = w.Write([]byte(`{"code":14,"error":"Something went wrong and the request could not complete. In many cases the issue can be resolved by trying again."}`))
			return
		}
		_, _ = w.Write([]byte(`{"traces":[]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	c.RetryBackoff = time.Millisecond // keep the test fast
	if _, err := c.QueryTraces(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected 3 attempts (2 x 503 + 1 ok), got %d", got)
	}
}

func TestQueryTraces_RetriesExhausted(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":14,"error":"Something went wrong and the request could not complete."}`))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	c.RetryBackoff = time.Millisecond
	_, err := c.QueryTraces(context.Background(), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("expected HTTP 503 error after exhausting retries, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != int32(c.MaxRetries+1) {
		t.Errorf("expected %d attempts, got %d", c.MaxRetries+1, got)
	}
}

func TestQueryTraces_DoesNotRetryClientError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"unknown field"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, nil)
	c.RetryBackoff = time.Millisecond
	if _, err := c.QueryTraces(context.Background(), map[string]any{}); err == nil {
		t.Fatal("expected error for HTTP 400")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("400 must not be retried; expected 1 attempt, got %d", got)
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
