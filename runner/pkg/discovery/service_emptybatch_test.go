package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

// collectRawBodies is collectEnvelopes' sibling that keeps the RAW request
// body. The decoded Envelope cannot distinguish `"data": null` from
// `"data": []` — both land in an `any` field as nil/empty — and that
// distinction is exactly what these tests assert, because the collector
// discards a null-data payload and keeps an empty-array one.
func collectRawBodies(t *testing.T, cs *fake.Clientset, configure func(*Service), until func([][]byte) bool) [][]byte {
	t.Helper()
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			// t.Errorf, not t.Fatalf: this runs on the server's goroutine, and
			// Fatalf there would Goexit the wrong one and hang the test.
			t.Errorf("read request body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := NewSink(srv.URL, "s", "a", "c", slog.Default())
	svc := NewService(cs, sink, time.Hour, slog.Default())
	configure(svc)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		snap := append([][]byte(nil), bodies...)
		mu.Unlock()
		if until(snap) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return append([][]byte(nil), bodies...)
}

// rawDataIsNull reports whether the envelope's `data` key is JSON null.
func rawDataIsNull(t *testing.T, body []byte) bool {
	t.Helper()
	var probe struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("unmarshal body %s: %v", body, err)
	}
	return string(probe.Data) == "null"
}

// lastBatchBody is strict about malformed bodies: every envelope the sink
// posts must be valid JSON, so a decode failure here is a real defect rather
// than a body to skip past.
func lastBatchBody(t *testing.T, bodies [][]byte) []byte {
	t.Helper()
	for _, b := range bodies {
		var e Envelope
		if err := json.Unmarshal(b, &e); err != nil {
			t.Fatalf("posted body is not a valid envelope: %v\nbody: %s", err, b)
		}
		if e.FullLoad && e.IsLastBatch {
			return b
		}
	}
	return nil
}

func anyLastBatch(bodies [][]byte) bool {
	for _, b := range bodies {
		var e Envelope
		if err := json.Unmarshal(b, &e); err != nil {
			continue
		}
		if e.FullLoad && e.IsLastBatch {
			return true
		}
	}
	return false
}

// A resource type with no items at all still owes the collector one
// authoritative is_last envelope. It must carry `"data": []`, never
// `"data": null` — the collector discards null-data payloads, which would
// drop the terminal sequence and silently skip the deletion-reconcile for
// every snapshot of that type, forever.
func TestEmptyType_LastBatchDataIsEmptyArrayNotNull(t *testing.T) {
	cs := fake.NewClientset()

	bodies := collectRawBodies(t, cs,
		func(s *Service) {
			s.SetOptions(Options{SnapshotBatching: true, BatchSize: 2})
			s.RegisterPods()
		},
		anyLastBatch,
	)

	last := lastBatchBody(t, bodies)
	if last == nil {
		t.Fatalf("no full-load is_last envelope was posted; got %d bodies", len(bodies))
	}
	if rawDataIsNull(t, last) {
		t.Errorf("empty type posted \"data\": null; want []\nbody: %s", last)
	}
}

// The production shape: the last *converting* item exactly fills a chunk and
// is followed by at least one item that does not convert (a scaled-to-zero
// ReplicaSet — a real cluster has hundreds trailing the pod/workload list).
// The `i < len(items)-1` guard then fires a non-final flush for that full
// chunk, so the terminal flush has an empty chunk.
//
// 4 pods + 1 zero-replica ReplicaSet at BatchSize 2: seq 1 and 2 carry two
// pods each, and seq 3 is the is_last envelope with no items.
func TestFullChunkThenNonConvertingTail_LastBatchDataIsEmptyArrayNotNull(t *testing.T) {
	var objs []runtime.Object
	for i := 0; i < 4; i++ {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("p%d", i), Namespace: "ns"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		})
	}
	// replicas == 0 => newReplicaSetConverter drops it.
	objs = append(objs, &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs-old", Namespace: "ns"},
		Spec:       appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(0))},
	})
	cs := fake.NewClientset(objs...)

	bodies := collectRawBodies(t, cs,
		func(s *Service) {
			s.SetOptions(Options{SnapshotBatching: true, BatchSize: 2})
			s.RegisterPods()
			s.RegisterReplicaSets()
		},
		anyLastBatch,
	)

	last := lastBatchBody(t, bodies)
	if last == nil {
		t.Fatalf("no full-load is_last envelope was posted; got %d bodies", len(bodies))
	}

	// The ReplicaSet lives in its own handler, so it is always last in the
	// flattened item list no matter how the pod indexer orders its own
	// entries — the empty-final-chunk shape here is deterministic.
	var e Envelope
	if err := json.Unmarshal(last, &e); err != nil {
		t.Fatalf("unmarshal last batch: %v", err)
	}
	if e.BatchSequence != 3 || dataLen(e) != 0 {
		t.Fatalf("last batch = seq %d with %d items; want seq 3 with 0 (4 pods / batch 2, then a dropped ReplicaSet)",
			e.BatchSequence, dataLen(e))
	}
	if rawDataIsNull(t, last) {
		t.Errorf("empty final chunk posted \"data\": null; want []\nbody: %s", last)
	}
}
