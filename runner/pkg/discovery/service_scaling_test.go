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
	"k8s.io/apimachinery/pkg/types"
	fake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

// collectEnvelopes runs a Service against cs (configured by configure) and
// returns every envelope POSTed to the sink, stopping once `until` is satisfied
// or after a timeout.
func collectEnvelopes(t *testing.T, cs *fake.Clientset, configure func(*Service), until func([]Envelope) bool) []Envelope {
	t.Helper()
	var (
		mu   sync.Mutex
		envs []Envelope
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var e Envelope
		_ = json.Unmarshal(body, &e)
		mu.Lock()
		envs = append(envs, e)
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
		snap := append([]Envelope(nil), envs...)
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
	return append([]Envelope(nil), envs...)
}

func dataLen(e Envelope) int {
	if arr, ok := e.Data.([]any); ok {
		return len(arr)
	}
	return 0
}

// Snapshot batching: 5 pods at BatchSize 2 must produce 3 full-load batches
// sharing one batch_id, sequenced 1/2/3 of 3, first/last flags set, and whose
// data union is all 5 pods.
func TestService_SnapshotBatching(t *testing.T) {
	var objs []runtime.Object
	for i := 0; i < 5; i++ {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("p%d", i), Namespace: "ns"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		})
	}
	cs := fake.NewClientset(objs...)

	envs := collectEnvelopes(t, cs,
		func(s *Service) {
			s.SetOptions(Options{SnapshotBatching: true, BatchSize: 2})
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
	if len(full) != 3 {
		t.Fatalf("full-load batches = %d; want 3 (5 items / batch 2)", len(full))
	}
	names := map[string]bool{}
	total := 0
	for i, e := range full {
		if e.BatchID != full[0].BatchID {
			t.Errorf("batch %d batch_id = %q; want shared %q", i, e.BatchID, full[0].BatchID)
		}
		if e.BatchSequence != i+1 {
			t.Errorf("batch %d sequence = %d; want %d", i, e.BatchSequence, i+1)
		}
		if e.TotalBatches != 3 {
			t.Errorf("batch %d total_batches = %d; want 3", i, e.TotalBatches)
		}
		total += dataLen(e)
		for _, raw := range e.Data.([]any) {
			names[raw.(map[string]any)["name"].(string)] = true
		}
	}
	if !full[0].IsFirstBatch || full[0].IsLastBatch {
		t.Errorf("first batch flags wrong: %+v", full[0])
	}
	if last := full[2]; !last.IsLastBatch || last.IsFirstBatch {
		t.Errorf("last batch flags wrong: %+v", last)
	}
	if total != 5 || len(names) != 5 {
		t.Errorf("union of batched data = %d items / %d names; want 5/5", total, len(names))
	}
}

// Incremental coalescing: with IncrementalBatch high, the per-event informer
// adds should be drained into multi-item incremental envelopes rather than one
// envelope per object.
func TestService_IncrementalCoalescing(t *testing.T) {
	const n = 20
	var objs []runtime.Object
	for i := 0; i < n; i++ {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("p%d", i), Namespace: "ns"},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		})
	}
	cs := fake.NewClientset(objs...)

	incrItems := func(e []Envelope) int {
		total := 0
		for _, x := range e {
			if !x.FullLoad {
				total += dataLen(x)
			}
		}
		return total
	}

	envs := collectEnvelopes(t, cs,
		func(s *Service) {
			s.SetOptions(Options{IncrementalBatch: n})
			s.RegisterPods()
		},
		func(e []Envelope) bool { return incrItems(e) >= n },
	)

	var incrEnvs int
	if got := incrItems(envs); got != n {
		t.Fatalf("incremental items = %d; want %d", got, n)
	}
	for _, e := range envs {
		if !e.FullLoad {
			incrEnvs++
			if e.BatchID != "" || e.TotalBatches != 0 {
				t.Errorf("incremental envelope carries batch metadata: %+v", e)
			}
		}
	}
	if incrEnvs >= n {
		t.Errorf("no coalescing: %d incremental envelopes for %d items", incrEnvs, n)
	}
}

// workloadOwnerUIDs must resolve a Deployment-owned pod to BOTH its ReplicaSet
// and the controlling Deployment, and a directly-owned pod to just its owner.
func TestWorkloadOwnerUIDs(t *testing.T) {
	rsIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	_ = rsIndexer.Add(&appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api-rs", Namespace: "ns", UID: "rs-uid",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "api", UID: "dep-uid", Controller: ptr.To(true)}},
		},
	})

	rsPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace:       "ns",
		OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-rs", UID: "rs-uid", Controller: ptr.To(true)}},
	}}
	uids := workloadOwnerUIDs(rsPod, rsIndexer)
	if !containsUID(uids, "rs-uid") || !containsUID(uids, "dep-uid") {
		t.Errorf("RS-owned pod uids = %v; want both rs-uid and dep-uid", uids)
	}

	ssPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace:       "ns",
		OwnerReferences: []metav1.OwnerReference{{Kind: "StatefulSet", Name: "db", UID: "ss-uid", Controller: ptr.To(true)}},
	}}
	uids = workloadOwnerUIDs(ssPod, rsIndexer)
	if len(uids) != 1 || uids[0] != "ss-uid" {
		t.Errorf("StatefulSet-owned pod uids = %v; want [ss-uid]", uids)
	}

	if got := workloadOwnerUIDs(&corev1.Pod{}, rsIndexer); got != nil {
		t.Errorf("unowned pod uids = %v; want nil", got)
	}
}

func containsUID(s []types.UID, want types.UID) bool {
	for _, u := range s {
		if u == want {
			return true
		}
	}
	return false
}
