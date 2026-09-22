package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Retry contract for the discovery sink.
//
// Motivated by the 2026-09-19 dev incident: rabbitmq-0 was rescheduled onto
// another node, the collector publishes to it inline on the request path, and
// 543 POSTs came back 5xx in five minutes. The agent dropped every one of those
// payloads, so cluster inventory went stale until the next resync.
//
// Backoff is swapped for near-zero waits; the production values are in sink.go.

func withFastBackoff(t *testing.T) {
	t.Helper()
	original := postBackoff
	postBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { postBackoff = original })
}

// countingServer answers with the given statuses in order, repeating the last
// one once exhausted, and counts the requests it saw.
func countingServer(t *testing.T, statuses ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		status := statuses[len(statuses)-1]
		if n <= len(statuses) {
			status = statuses[n-1]
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func testEnvelope() *Envelope {
	return &Envelope{Type: TypeService, Data: []any{map[string]any{"name": "frontend"}}}
}

func TestSink_RetriesWhileBackendIsUnavailable(t *testing.T) {
	withFastBackoff(t)
	srv, calls := countingServer(t, http.StatusServiceUnavailable, http.StatusServiceUnavailable, http.StatusOK)

	s := NewSink(srv.URL, "secret", "acc-1", "cluster-x", slog.Default())
	if err := s.Post(context.Background(), testEnvelope()); err != nil {
		t.Fatalf("Post should have succeeded on the third attempt: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestSink_GivesUpAfterTheRetryBudget(t *testing.T) {
	withFastBackoff(t)
	srv, calls := countingServer(t, http.StatusServiceUnavailable)

	s := NewSink(srv.URL, "secret", "acc-1", "cluster-x", slog.Default())
	if err := s.Post(context.Background(), testEnvelope()); err == nil {
		t.Fatal("Post should return the last error once the budget is spent")
	}
	// One initial attempt plus one per backoff entry — not unbounded.
	if got, want := int(calls.Load()), len(postBackoff)+1; got != want {
		t.Errorf("attempts = %d, want %d", got, want)
	}
}

func TestSink_DoesNotRetryClientErrors(t *testing.T) {
	withFastBackoff(t)

	// 401 is the one that matters: a wrong agent secret retried on every
	// delta would quadruple auth-failure load on the collector, which already
	// spends most of its ERROR budget on exactly that.
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv, calls := countingServer(t, status)
			s := NewSink(srv.URL, "secret", "acc-1", "cluster-x", slog.Default())
			if err := s.Post(context.Background(), testEnvelope()); err == nil {
				t.Fatal("expected an error")
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("attempts = %d, want 1 (client errors are not retriable)", got)
			}
		})
	}
}

func TestSink_DoesNotRetryBareInternalErrors(t *testing.T) {
	withFastBackoff(t)
	srv, calls := countingServer(t, http.StatusInternalServerError)

	s := NewSink(srv.URL, "secret", "acc-1", "cluster-x", slog.Default())
	if err := s.Post(context.Background(), testEnvelope()); err == nil {
		t.Fatal("expected an error")
	}
	// 500 means the collector hit something unexpected. Replaying the same
	// payload into that is not a fix; 503 is the status that says "come back".
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestSink_RetriedGzipBodyIsNotEmpty(t *testing.T) {
	withFastBackoff(t)

	// Regression guard: the body used to be streamed from a bytes.Buffer, which
	// a retry would find already drained — the retry would then POST 0 bytes and
	// "succeed", silently losing the payload it was added to save.
	type attempt struct {
		encoding string
		body     []byte
	}
	attempts := make(chan attempt, 4)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		attempts <- attempt{encoding: r.Header.Get("Content-Encoding"), body: body}
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	items := make([]any, 0, 400)
	for i := 0; i < 400; i++ {
		items = append(items, map[string]any{"name": fmt.Sprintf("workload-%d-%s", i, "padding-to-exceed-16kb")})
	}

	s := NewSink(srv.URL, "secret", "acc-1", "cluster-x", slog.Default())
	if err := s.Post(context.Background(), &Envelope{Type: TypeService, Data: items}); err != nil {
		t.Fatalf("Post: %v", err)
	}

	first, second := <-attempts, <-attempts
	if first.encoding != "gzip" {
		t.Fatalf("first attempt encoding = %q, want gzip (payload should exceed the 16 KB threshold)", first.encoding)
	}
	if second.encoding != "gzip" {
		t.Errorf("retry encoding = %q, want gzip", second.encoding)
	}
	if len(second.body) != len(first.body) {
		t.Fatalf("retry sent %d bytes, first attempt sent %d — the retry must resend the same payload",
			len(second.body), len(first.body))
	}

	// And it must still be decodable, not just the same length.
	zr, err := gzip.NewReader(bytes.NewReader(second.body))
	if err != nil {
		t.Fatalf("retry body is not valid gzip: %v", err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read retry body: %v", err)
	}
	var decoded Envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("retry body is not a valid envelope: %v", err)
	}
	if envelopeItemCount(decoded.Data) != len(items) {
		t.Errorf("retry carried %d items, want %d", envelopeItemCount(decoded.Data), len(items))
	}
}

func TestSink_StopsRetryingWhenContextIsCancelled(t *testing.T) {
	// Real backoff here on purpose: the point is that cancellation wins over
	// the wait rather than the test outrunning it.
	srv, calls := countingServer(t, http.StatusServiceUnavailable)

	ctx, cancel := context.WithCancel(context.Background())
	s := NewSink(srv.URL, "secret", "acc-1", "cluster-x", slog.Default())

	done := make(chan error, 1)
	go func() { done <- s.Post(ctx, testEnvelope()) }()

	// Let the first attempt land, then cancel while it is backing off.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Post kept waiting after its context was cancelled")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("attempts = %d, want 1 (cancellation should land during the first backoff)", got)
	}
}
