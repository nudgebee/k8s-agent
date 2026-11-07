package discovery

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	fake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// deleteHarness wires one Pod handler to a backend whose status code the test
// controls, without running informers: the test drives onDelete and
// processBatch directly and edits the indexer by hand.
type deleteHarness struct {
	svc    *Service
	h      *resourceHandler
	status int

	mu    sync.Mutex
	items []map[string]any
}

func newDeleteHarness(t *testing.T) *deleteHarness {
	t.Helper()
	d := &deleteHarness{status: http.StatusOK}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var e struct {
			Data []map[string]any `json:"data"`
		}
		_ = json.Unmarshal(body, &e)
		d.mu.Lock()
		d.items = append(d.items, e.Data...)
		status := d.status
		d.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	d.svc = NewService(fake.NewClientset(), NewSink(srv.URL, "s", "a", "c", slog.Default()), 0, slog.Default())
	d.svc.SetOptions(Options{EmitTombstones: true})
	d.h = &resourceHandler{
		typ:       TypeService,
		informer:  informers.NewSharedInformerFactory(fake.NewClientset(), 0).Core().V1().Pods().Informer(),
		queue:     workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()),
		converter: convertPod,
	}
	return d
}

func (d *deleteHarness) posted() []map[string]any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]map[string]any(nil), d.items...)
}

func (d *deleteHarness) setStatus(code int) {
	d.mu.Lock()
	d.status = code
	d.mu.Unlock()
}

func testPod(uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-0", Namespace: "ns", UID: types.UID(uid), ResourceVersion: "7"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// A delete the watch missed is delivered on relist as DeletedFinalStateUnknown.
// It used to be dropped (no tombstone, no queued key), leaving the pod shown
// as running until the next full snapshot.
func TestOnDelete_DeletedFinalStateUnknownPostsTombstone(t *testing.T) {
	d := newDeleteHarness(t)
	d.svc.onDelete(d.h, cache.DeletedFinalStateUnknown{Key: "ns/web-0", Obj: testPod("uid-1")})

	if d.h.queue.Len() != 1 {
		t.Fatalf("queue len = %d; want the deleted pod's key queued", d.h.queue.Len())
	}
	key, _ := d.h.queue.Get()
	d.svc.processBatch(context.Background(), d.h, []string{key})
	d.h.queue.Done(key)

	got := d.posted()
	if len(got) != 1 || got[0]["deleted"] != true || got[0]["service_key"] != "ns/Pod/web-0" || got[0]["uid"] != "uid-1" {
		t.Fatalf("posted %v; want one deleted:true tombstone for ns/Pod/web-0 carrying its uid", got)
	}
}

// A failed POST of an object's last change must be retried, or the backend
// keeps the previous state until the next snapshot. The tombstone is put back
// so the retry can send it again.
func TestProcessBatch_FailedPostRequeuesBounded(t *testing.T) {
	d := newDeleteHarness(t)
	d.setStatus(http.StatusInternalServerError) // the sink does not retry a 500
	if err := d.h.informer.GetIndexer().Add(testPod("uid-1")); err != nil {
		t.Fatal(err)
	}
	// web-0 is live (its last change must be re-sent); web-1 was deleted.
	gone := testPod("uid-2")
	gone.Name = "web-1"
	d.svc.onDelete(d.h, gone)
	for d.h.queue.Len() > 0 { // drain what onDelete queued
		k, _ := d.h.queue.Get()
		d.h.queue.Done(k)
		d.h.queue.Forget(k)
	}

	keys := []string{"ns/web-0", "ns/web-1"}
	for attempt := 1; attempt <= maxIncrementalRequeues+1; attempt++ {
		d.svc.processBatch(context.Background(), d.h, keys)
		for _, k := range keys {
			want := attempt
			if attempt > maxIncrementalRequeues {
				want = 0 // given up: forgotten, left to the next snapshot
			}
			if got := d.h.queue.NumRequeues(k); got != want {
				t.Fatalf("attempt %d: NumRequeues(%s) = %d; want %d", attempt, k, got, want)
			}
		}
		if attempt <= maxIncrementalRequeues {
			if _, ok := d.h.popTombstone("ns/web-1"); !ok {
				t.Fatalf("attempt %d: tombstone not restashed after a failed POST", attempt)
			}
			d.h.restashTombstone("ns/web-1", map[string]any{"deleted": true, "service_key": "ns/Pod/web-1"})
		}
	}

	// Once the backend recovers, the retry sends the latest state and the
	// tombstone, and the keys are forgotten.
	d.setStatus(http.StatusOK)
	d.h.restashTombstone("ns/web-1", map[string]any{"deleted": true, "service_key": "ns/Pod/web-1"})
	d.svc.processBatch(context.Background(), d.h, keys)
	sent := map[string]bool{}
	for _, it := range d.posted() {
		sent[it["service_key"].(string)+map[bool]string{true: ":deleted", false: ""}[it["deleted"] == true]] = true
	}
	if !sent["ns/Pod/web-0"] || !sent["ns/Pod/web-1:deleted"] {
		t.Errorf("after recovery posted %v; want web-0 state and web-1 tombstone", sent)
	}
	for _, k := range keys {
		if n := d.h.queue.NumRequeues(k); n != 0 {
			t.Errorf("NumRequeues(%s) = %d after success; want 0", k, n)
		}
	}
}

// Deleted and recreated under the same name before the worker ran: only the
// new pod is sent, and the old pod's tombstone is not left stashed.
func TestProcessBatch_RecreatedPodSupersedesTombstone(t *testing.T) {
	d := newDeleteHarness(t)
	d.svc.onDelete(d.h, testPod("uid-old"))
	if err := d.h.informer.GetIndexer().Add(testPod("uid-new")); err != nil {
		t.Fatal(err)
	}
	d.svc.processBatch(context.Background(), d.h, []string{"ns/web-0"})

	got := d.posted()
	if len(got) != 1 || got[0]["deleted"] == true || got[0]["uid"] != "uid-new" {
		t.Fatalf("posted %v; want only the recreated pod (uid-new)", got)
	}
	if _, ok := d.h.popTombstone("ns/web-0"); ok {
		t.Error("the old pod's tombstone is still stashed")
	}
}
