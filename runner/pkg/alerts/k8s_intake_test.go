package alerts

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingEngine records the order events reach the matcher in, optionally
// blocking on gate first. It fires no matchers.
type recordingEngine struct {
	gate     chan struct{} // nil = never block
	kinds    map[string]bool
	panicOn  string
	mu       sync.Mutex
	seen     []string // "kind/name/rv"
	reachedC chan struct{}
}

func (e *recordingEngine) MatchK8sEvent(_, kind string, obj, _ map[string]any) []MatchedTrigger {
	if e.reachedC != nil {
		select {
		case e.reachedC <- struct{}{}:
		default:
		}
	}
	if e.gate != nil {
		<-e.gate
	}
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if name == e.panicOn && name != "" {
		panic("boom")
	}
	rv, _ := meta["resourceVersion"].(string)
	e.mu.Lock()
	e.seen = append(e.seen, kind+"/"+name+"/"+rv)
	e.mu.Unlock()
	return nil
}

func (e *recordingEngine) HandlesKind(kind string) bool {
	return e.kinds == nil || e.kinds[strings.ToLower(kind)]
}

func (e *recordingEngine) seenEvents() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.seen...)
}

type kwMetrics struct {
	mu       sync.Mutex
	outcomes map[string]int // "kind:outcome"
}

func (m *kwMetrics) OnKubewatchEvent(kind, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.outcomes == nil {
		m.outcomes = map[string]int{}
	}
	m.outcomes[kind+":"+outcome]++
}
func (m *kwMetrics) ObserveKubewatchHandle(float64)        {}
func (m *kwMetrics) ObserveKubewatchQueueWait(float64)     {}
func (m *kwMetrics) ObserveKubewatchMatch(string, float64) {}
func (m *kwMetrics) get(key string) int                    { m.mu.Lock(); defer m.mu.Unlock(); return m.outcomes[key] }
func kwPayload(kind, ns, name, rv string) string {
	return fmt.Sprintf(`{"data":{"operation":"update","kind":%q,"obj":{"metadata":{"name":%q,"namespace":%q,"resourceVersion":%q}}}}`,
		kind, name, ns, rv)
}

func postKW(t *testing.T, url, body string) time.Duration {
	t.Helper()
	start := time.Now()
	resp, err := http.Post(url+"/api/handle", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", resp.StatusCode)
	}
	return time.Since(start)
}

// A slow matcher (the evidence-table Lists during an incident) must not hold
// the kubewatch request: kubewatch sends the next event of a type only after
// this response, so matcher latency used to become kubewatch queue growth.
func TestKubewatchIntake_HandlerDoesNotWaitForMatcher(t *testing.T) {
	gate := make(chan struct{})
	eng := &recordingEngine{gate: gate}
	f := NewForwarder("", "", "", "", slog.Default())
	f.Engine = eng
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	for i := 0; i < 5; i++ {
		if d := postKW(t, srv.URL, kwPayload("Pod", "ns", fmt.Sprintf("p%d", i), "1")); d > time.Second {
			t.Fatalf("POST %d took %v while the matcher was blocked; the handler must not wait for matching", i, d)
		}
	}
	close(gate)
	waitFor(t, func() bool { return len(eng.seenEvents()) == 5 })
	if got := len(eng.seenEvents()); got != 5 {
		t.Fatalf("matcher saw %d events; want 5", got)
	}
}

// Kinds no matcher reads are dropped before they are queued or decoded.
func TestKubewatchIntake_IgnoredKindNeverQueued(t *testing.T) {
	eng := &recordingEngine{kinds: map[string]bool{"pod": true}}
	m := &kwMetrics{}
	f := NewForwarder("", "", "", "", slog.Default())
	f.Engine = eng
	f.KubewatchMetrics = m
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	postKW(t, srv.URL, kwPayload("ReplicaSet", "ns", "rs", "1"))
	postKW(t, srv.URL, kwPayload("Pod", "ns", "p", "1"))
	waitFor(t, func() bool { return len(eng.seenEvents()) == 1 })

	if got := eng.seenEvents(); len(got) != 1 || got[0] != "Pod/p/1" {
		t.Fatalf("matcher saw %v; want only the Pod", got)
	}
	if got := m.get("other:" + KubewatchOutcomeIgnoredKind); got != 1 {
		t.Errorf("ignored_kind count = %d; want 1", got)
	}
	if got := f.K8sUnmatched(); got != 2 {
		t.Errorf("K8sUnmatched = %d; want 2 (ignored kind + unmatched pod)", got)
	}
}

// With the queue full, events are shed and counted rather than buffered
// without bound or pushed back onto the sender.
func TestKubewatchIntake_ShedsWhenQueueFull(t *testing.T) {
	gate := make(chan struct{})
	reached := make(chan struct{}, 1)
	eng := &recordingEngine{gate: gate, reachedC: reached}
	m := &kwMetrics{}
	var shedSources []string
	var shedMu sync.Mutex
	f := NewForwarder("", "", "", "", slog.Default())
	f.Engine = eng
	f.KubewatchMetrics = m
	f.OnShed = func(s string) { shedMu.Lock(); shedSources = append(shedSources, s); shedMu.Unlock() }
	f.SetKubewatchQueue(1, 1, 0)
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	postKW(t, srv.URL, kwPayload("Pod", "ns", "a", "1")) // taken by the worker
	<-reached
	postKW(t, srv.URL, kwPayload("Pod", "ns", "b", "1")) // fills the one slot
	postKW(t, srv.URL, kwPayload("Pod", "ns", "c", "1")) // shed
	postKW(t, srv.URL, kwPayload("Pod", "ns", "d", "1")) // shed

	if got := f.ForwardShed(); got != 2 {
		t.Errorf("ForwardShed = %d; want 2", got)
	}
	if got := m.get("pod:" + KubewatchOutcomeShed); got != 2 {
		t.Errorf("shed outcome count = %d; want 2", got)
	}
	shedMu.Lock()
	if len(shedSources) != 2 || shedSources[0] != "kubewatch_queue" {
		t.Errorf("OnShed sources = %v; want 2 x kubewatch_queue", shedSources)
	}
	shedMu.Unlock()
	if d := f.KubewatchQueueDepth(); d != 1 {
		t.Errorf("queue depth = %d; want 1", d)
	}

	close(gate)
	waitFor(t, func() bool { return len(eng.seenEvents()) == 2 })
	if got := eng.seenEvents(); len(got) != 2 {
		t.Fatalf("matcher saw %v; want a and b", got)
	}
	waitFor(t, func() bool { return f.KubewatchQueueDepth() == 0 })
	if d := f.KubewatchQueueDepth(); d != 0 {
		t.Errorf("queue depth after drain = %d; want 0", d)
	}
}

// The byte budget sheds before a burst of large objects can grow the heap.
func TestKubewatchIntake_ShedsOverByteBudget(t *testing.T) {
	gate := make(chan struct{})
	reached := make(chan struct{}, 1)
	eng := &recordingEngine{gate: gate, reachedC: reached}
	f := NewForwarder("", "", "", "", slog.Default())
	f.Engine = eng
	f.SetKubewatchQueue(1, 100, 1)
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	postKW(t, srv.URL, kwPayload("Pod", "ns", "a", "1"))
	if got := f.ForwardShed(); got != 1 {
		t.Errorf("ForwardShed = %d; want 1 (payload larger than the 1-byte budget)", got)
	}
	close(gate)
}

// Updates to one object are matched in the order they arrived: the recovery
// predicate and rate limiter depend on it.
func TestKubewatchIntake_PerObjectOrder(t *testing.T) {
	eng := &recordingEngine{}
	f := NewForwarder("", "", "", "", slog.Default())
	f.Engine = eng
	f.SetKubewatchQueue(4, 1000, 0)
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	const n = 50
	for i := 0; i < n; i++ {
		postKW(t, srv.URL, kwPayload("Pod", "ns", "same", fmt.Sprint(i)))
		postKW(t, srv.URL, kwPayload("Pod", "ns", fmt.Sprintf("other-%d", i), "0"))
	}
	waitFor(t, func() bool { return len(eng.seenEvents()) == 2*n })

	next := 0
	for _, s := range eng.seenEvents() {
		if !strings.HasPrefix(s, "Pod/same/") {
			continue
		}
		if want := fmt.Sprintf("Pod/same/%d", next); s != want {
			t.Fatalf("got %s at position %d; want %s", s, next, want)
		}
		next++
	}
	if next != n {
		t.Fatalf("saw %d events for the object; want %d", next, n)
	}
}

// A panicking matcher is contained to its event; the worker keeps going.
func TestKubewatchIntake_MatcherPanicRecovered(t *testing.T) {
	eng := &recordingEngine{panicOn: "bad"}
	m := &kwMetrics{}
	f := NewForwarder("", "", "", "", slog.Default())
	f.Engine = eng
	f.KubewatchMetrics = m
	f.SetKubewatchQueue(1, 10, 0)
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	postKW(t, srv.URL, kwPayload("Pod", "ns", "bad", "1"))
	postKW(t, srv.URL, kwPayload("Pod", "ns", "good", "1"))
	waitFor(t, func() bool { return len(eng.seenEvents()) == 1 })

	if got := eng.seenEvents(); len(got) != 1 || got[0] != "Pod/good/1" {
		t.Fatalf("matcher saw %v; want the event after the panic", got)
	}
	if got := m.get("pod:" + KubewatchOutcomePanic); got != 1 {
		t.Errorf("panic outcome count = %d; want 1", got)
	}
}

// A flood of sheds is visible at Warn, once per interval, with the count.
func TestKubewatchIntake_ShedWarnsOncePerInterval(t *testing.T) {
	var buf bytes.Buffer
	f := NewForwarder("", "", "", "", slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	for i := 0; i < 5; i++ {
		f.warnKubewatchShed("Pod", "ns", fmt.Sprint(i))
	}
	if got := strings.Count(buf.String(), "events shed"); got != 1 {
		t.Fatalf("got %d shed warnings for one burst; want 1:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Errorf("shed warning not at Warn:\n%s", buf.String())
	}

	// After the interval, the next warning reports the sheds it covers.
	f.kwShedWarn.Store(time.Now().Add(-2 * kubewatchShedWarnInterval).UnixNano())
	buf.Reset()
	f.warnKubewatchShed("Pod", "ns", "x")
	if !strings.Contains(buf.String(), "shed_since_last_warning=5") {
		t.Errorf("want the 4 suppressed sheds plus this one counted:\n%s", buf.String())
	}
}
