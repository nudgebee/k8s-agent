package alerts

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const crashLoopWebhook = `{"alerts":[{
	"startsAt":"2026-05-07T10:00:00Z",
	"status":"firing",
	"labels":{"alertname":"PodCrashLooping","pod":"web-0","namespace":"prod","severity":"critical"},
	"annotations":{"summary":"Pod web-0 in prod is crashlooping"}
}]}`

type hookCounts struct {
	mu        sync.Mutex
	forwarded int
	dropped   []string
}

func (h *hookCounts) onForward() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.forwarded++
}

func (h *hookCounts) onDrop(source string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropped = append(h.dropped, source)
}

func (h *hookCounts) get() (int, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.forwarded, append([]string(nil), h.dropped...)
}

// waitFor polls until cond holds or the deadline passes — the forward runs in
// a goroutine spawned after the handler has already returned 202.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func postAlert(t *testing.T, f *Forwarder) {
	t.Helper()
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/alerts", "application/json", strings.NewReader(crashLoopWebhook))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", resp.StatusCode)
	}
}

func TestForwarder_OnForwardFiresWhenBackendAccepts(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	h := &hookCounts{}
	f := NewForwarder(backend.URL, "tok", "acc", "cluster", slog.Default())
	f.OnForward = h.onForward
	f.OnDrop = h.onDrop

	postAlert(t, f)
	waitFor(t, func() bool { n, _ := h.get(); return n == 1 })

	forwarded, dropped := h.get()
	if forwarded != 1 {
		t.Errorf("OnForward fired %d times; want 1", forwarded)
	}
	if len(dropped) != 0 {
		t.Errorf("OnDrop fired for %v; want none", dropped)
	}
}

func TestForwarder_OnDropFiresWhenBackendRejects(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()

	h := &hookCounts{}
	f := NewForwarder(backend.URL, "tok", "acc", "cluster", slog.Default())
	f.OnForward = h.onForward
	f.OnDrop = h.onDrop

	postAlert(t, f)
	waitFor(t, func() bool { _, d := h.get(); return len(d) == 1 })

	forwarded, dropped := h.get()
	if forwarded != 0 {
		t.Errorf("OnForward fired %d times on a rejected forward; want 0", forwarded)
	}
	if len(dropped) != 1 || dropped[0] != "alertmanager" {
		t.Errorf("OnDrop sources = %v; want [alertmanager]", dropped)
	}
}

func TestForwarder_NilHooksAreSafe(t *testing.T) {
	reached := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		select {
		case reached <- struct{}{}:
		default:
		}
	}))
	defer backend.Close()

	// OnForward / OnDrop left nil: the forward must still run to completion.
	f := NewForwarder(backend.URL, "tok", "acc", "cluster", slog.Default())
	postAlert(t, f)

	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("backend never received the forward")
	}
	if got := f.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d; want 0", got)
	}
}
