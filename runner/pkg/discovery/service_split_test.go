package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fake "k8s.io/client-go/kubernetes/fake"
)

// splitSink is a Sink posting to a server that records every body it gets.
func splitSink(t *testing.T, maxBody int) (*Sink, func() [][]byte) {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	sink := NewSink(srv.URL, "s", "a", "c", slog.Default())
	sink.MaxBodyBytes = maxBody
	return sink, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), bodies...)
	}
}

// items returns n objects of about size bytes each once encoded.
func items(n, size int) []any {
	out := make([]any, n)
	for i := range out {
		out[i] = map[string]any{"name": fmt.Sprintf("obj-%d", i), "pad": strings.Repeat("x", size)}
	}
	return out
}

func decodeAll(t *testing.T, bodies [][]byte) []Envelope {
	t.Helper()
	envs := make([]Envelope, len(bodies))
	for i, b := range bodies {
		if err := json.Unmarshal(b, &envs[i]); err != nil {
			t.Fatalf("body %d: %v", i, err)
		}
	}
	return envs
}

// A snapshot batch over the body limit goes out as several parts of the same
// snapshot: sequences continue with no gaps, only the first part is first and
// only the last is last, every body fits, and no object is lost.
func TestPostSplitting_SnapshotPartsStayOneSnapshot(t *testing.T) {
	sink, got := splitSink(t, 2500) // fits two ~1 KB objects, not three
	svc := NewService(fake.NewClientset(), sink, 0, slog.Default())

	seq := 3 // this batch's own sequence; earlier batches used 1 and 2
	env := &Envelope{Type: "service", Data: items(7, 1000), FullLoad: true, BatchID: "snap-1",
		BatchSequence: seq, TotalBatches: 3, IsLastBatch: true}
	if err := svc.postSplitting(context.Background(), env, &seq); err != nil {
		t.Fatal(err)
	}

	envs := decodeAll(t, got())
	if len(envs) < 4 {
		t.Fatalf("posted %d parts; want 7 objects at <=2 per part", len(envs))
	}
	names := map[string]bool{}
	for i, e := range envs {
		if want := 3 + i; e.BatchSequence != want {
			t.Errorf("part %d sequence = %d; want %d", i, e.BatchSequence, want)
		}
		if e.BatchID != "snap-1" || !e.FullLoad {
			t.Errorf("part %d lost its snapshot fields: %+v", i, e)
		}
		if e.IsFirstBatch {
			t.Errorf("part %d is_first_batch set; the batch was not the first", i)
		}
		if last := i == len(envs)-1; e.IsLastBatch != last {
			t.Errorf("part %d is_last_batch = %v; want %v", i, e.IsLastBatch, last)
		}
		for _, raw := range e.Data.([]any) {
			names[raw.(map[string]any)["name"].(string)] = true
		}
	}
	for i, b := range got() {
		if len(b) > 2500 {
			t.Errorf("part %d is %d bytes, over the limit", i, len(b))
		}
	}
	if len(names) != 7 {
		t.Errorf("parts carry %d distinct objects; want 7", len(names))
	}
	if seq != 2+len(envs) {
		t.Errorf("seq after = %d; want %d so the next batch continues the sequence", seq, 2+len(envs))
	}
}

// A first batch keeps is_first_batch on its first part only.
func TestPostSplitting_FirstFlagOnFirstPartOnly(t *testing.T) {
	sink, got := splitSink(t, 2500)
	svc := NewService(fake.NewClientset(), sink, 0, slog.Default())
	seq := 1
	env := &Envelope{Type: "service", Data: items(4, 1000), FullLoad: true, BatchID: "snap-2",
		BatchSequence: 1, TotalBatches: 2, IsFirstBatch: true}
	if err := svc.postSplitting(context.Background(), env, &seq); err != nil {
		t.Fatal(err)
	}
	envs := decodeAll(t, got())
	for i, e := range envs {
		if e.IsFirstBatch != (i == 0) {
			t.Errorf("part %d is_first_batch = %v", i, e.IsFirstBatch)
		}
		if e.IsLastBatch {
			t.Errorf("part %d is_last_batch set; the batch was not the last", i)
		}
	}
}

// An incremental envelope splits without gaining batch fields.
func TestPostSplitting_IncrementalHasNoBatchFields(t *testing.T) {
	sink, got := splitSink(t, 2500)
	svc := NewService(fake.NewClientset(), sink, 0, slog.Default())
	if err := svc.postSplitting(context.Background(), &Envelope{Type: "service", Data: items(5, 1000)}, nil); err != nil {
		t.Fatal(err)
	}
	envs := decodeAll(t, got())
	if len(envs) < 3 {
		t.Fatalf("posted %d parts; want 5 objects at <=2 per part", len(envs))
	}
	for i, e := range envs {
		if e.BatchID != "" || e.BatchSequence != 0 || e.IsFirstBatch || e.IsLastBatch || e.FullLoad {
			t.Errorf("part %d gained batch fields: %+v", i, e)
		}
	}
}

// One object over the limit on its own cannot be sent: nothing is posted and
// the caller gets the error, so a snapshot aborts without its last batch.
func TestPostSplitting_SingleObjectOverLimitFails(t *testing.T) {
	sink, got := splitSink(t, 500)
	svc := NewService(fake.NewClientset(), sink, 0, slog.Default())
	seq := 1
	err := svc.postSplitting(context.Background(), &Envelope{Type: "service", Data: items(1, 1000), FullLoad: true,
		BatchID: "snap-3", BatchSequence: 1, IsFirstBatch: true, IsLastBatch: true}, &seq)
	var tooLarge *BodyTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v; want *BodyTooLargeError", err)
	}
	if n := len(got()); n != 0 {
		t.Errorf("posted %d bodies; want none", n)
	}
}

// With the limit off, nothing changes.
func TestPostSplitting_NoLimitPostsOnce(t *testing.T) {
	sink, got := splitSink(t, 0)
	svc := NewService(fake.NewClientset(), sink, 0, slog.Default())
	seq := 1
	if err := svc.postSplitting(context.Background(), &Envelope{Type: "service", Data: items(50, 1000), FullLoad: true,
		BatchID: "snap-4", BatchSequence: 1, IsFirstBatch: true, IsLastBatch: true}, &seq); err != nil {
		t.Fatal(err)
	}
	if n := len(got()); n != 1 || seq != 1 {
		t.Errorf("posted %d bodies, seq %d; want 1 and 1", n, seq)
	}
}

// The unbatched snapshot path (DISCOVERY_SNAPSHOT_BATCHING off, the binary's
// default) splits too, and its parts still form one complete snapshot.
func TestService_UnbatchedSnapshotSplitsByBytes(t *testing.T) {
	var objs []runtime.Object
	for i := 0; i < 8; i++ {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("p%d", i), Namespace: "ns"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		})
	}
	envs := collectEnvelopes(t, fake.NewClientset(objs...),
		func(s *Service) {
			s.sink.MaxBodyBytes = unbatchedPodLimit(t)
			s.RegisterPods()
		},
		func(e []Envelope) bool {
			for _, x := range e {
				if x.FullLoad && x.IsLastBatch {
					return true
				}
			}
			return false
		},
	)
	var full []Envelope
	for _, e := range envs {
		if e.FullLoad {
			full = append(full, e)
		}
	}
	if len(full) < 2 {
		t.Fatalf("full-load parts = %d; want the 8-pod snapshot split", len(full))
	}
	names := map[string]bool{}
	for i, e := range full {
		if e.BatchSequence != i+1 || e.BatchID != full[0].BatchID {
			t.Errorf("part %d: sequence %d batch %q; want %d of %q", i, e.BatchSequence, e.BatchID, i+1, full[0].BatchID)
		}
		if e.IsFirstBatch != (i == 0) || e.IsLastBatch != (i == len(full)-1) {
			t.Errorf("part %d flags first=%v last=%v", i, e.IsFirstBatch, e.IsLastBatch)
		}
		for _, raw := range e.Data.([]any) {
			names[raw.(map[string]any)["name"].(string)] = true
		}
	}
	if len(names) != 8 {
		t.Errorf("snapshot carries %d pods; want 8", len(names))
	}
}

// unbatchedPodLimit is a body limit that holds about three of the test's pods:
// the size of one converted pod's envelope times three.
func unbatchedPodLimit(t *testing.T) int {
	t.Helper()
	item, ok := convertPod(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p0", Namespace: "ns"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	})
	if !ok {
		t.Fatal("pod not converted")
	}
	b, err := json.Marshal(&Envelope{Type: "service", Data: []any{item}, FullLoad: true, BatchID: "snap-0000000000000000000",
		BatchSequence: 1, TotalBatches: 1, IsFirstBatch: true, IsLastBatch: true})
	if err != nil {
		t.Fatal(err)
	}
	return 3 * len(b)
}
