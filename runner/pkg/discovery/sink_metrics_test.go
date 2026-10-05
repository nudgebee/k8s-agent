package discovery

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// recordingMetrics is a SinkMetrics that remembers what it was told, so the
// assertions are about the production code path calling it — not about a
// registry being able to hold a counter.
type recordingMetrics struct {
	mu     sync.Mutex
	posts  []string // "<type>/<full_load>"
	errors []string // "<type>"
}

func (m *recordingMetrics) OnDiscoveryPost(typ string, fullLoad bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	suffix := "/false"
	if fullLoad {
		suffix = "/true"
	}
	m.posts = append(m.posts, typ+suffix)
}

func (m *recordingMetrics) OnDiscoveryError(typ string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errors = append(m.errors, typ)
}

func (m *recordingMetrics) snapshot() ([]string, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.posts...), append([]string(nil), m.errors...)
}

func TestSinkPost_RecordsSuccessWithTypeAndFullLoad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	m := &recordingMetrics{}
	sink := NewSink(srv.URL, "s", "a", "c", slog.Default())
	sink.Metrics = m

	if err := sink.Post(context.Background(), &Envelope{Type: TypeService, Data: []any{}, FullLoad: true}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := sink.Post(context.Background(), &Envelope{Type: TypeNode, Data: []any{}}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	posts, errs := m.snapshot()
	want := []string{"service/true", "node/false"}
	if len(posts) != len(want) {
		t.Fatalf("posts = %v; want %v", posts, want)
	}
	for i := range want {
		if posts[i] != want[i] {
			t.Errorf("posts[%d] = %q; want %q", i, posts[i], want[i])
		}
	}
	if len(errs) != 0 {
		t.Errorf("errors = %v; want none", errs)
	}
}

func TestSinkPost_RecordsBackendRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	m := &recordingMetrics{}
	sink := NewSink(srv.URL, "s", "a", "c", slog.Default())
	sink.Metrics = m

	if err := sink.Post(context.Background(), &Envelope{Type: TypeService, Data: []any{}, FullLoad: true}); err == nil {
		t.Fatal("Post returned nil error on HTTP 500")
	}

	posts, errs := m.snapshot()
	if len(posts) != 0 {
		t.Errorf("posts = %v; want none on a rejected POST", posts)
	}
	if len(errs) != 1 || errs[0] != "service" {
		t.Errorf("errors = %v; want [service]", errs)
	}
}

// A misconfigured sink never reaches the HTTP layer. It still has to be
// counted, or "the backend URL is empty" looks identical to "discovery is
// idle" on the dashboard.
func TestSinkPost_RecordsPreFlightFailure(t *testing.T) {
	m := &recordingMetrics{}
	sink := NewSink("", "s", "a", "c", slog.Default())
	sink.Metrics = m

	if err := sink.Post(context.Background(), &Envelope{Type: TypeJob, Data: []any{}}); err == nil {
		t.Fatal("Post returned nil error with no backend URL")
	}

	posts, errs := m.snapshot()
	if len(posts) != 0 {
		t.Errorf("posts = %v; want none", posts)
	}
	if len(errs) != 1 || errs[0] != "job" {
		t.Errorf("errors = %v; want [job]", errs)
	}
}

func TestSinkPost_NilMetricsIsSafe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewSink(srv.URL, "s", "a", "c", slog.Default())
	if err := sink.Post(context.Background(), &Envelope{Type: TypeService, Data: []any{}}); err != nil {
		t.Fatalf("Post with nil Metrics: %v", err)
	}
}
