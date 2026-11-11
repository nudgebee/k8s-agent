package k8swatch

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
)

type recordingSink struct {
	mu      sync.Mutex
	changes []recorded
}

type recorded struct {
	operation, kind string
	data            map[string]any
	raw             json.RawMessage
}

func (s *recordingSink) HandleK8sChange(operation, kind string, data json.RawMessage) {
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	s.mu.Lock()
	s.changes = append(s.changes, recorded{operation, kind, m, data})
	s.mu.Unlock()
}

func (s *recordingSink) snapshot() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.changes...)
}

type countingMetrics struct {
	mu sync.Mutex
	n  map[string]int
}

func (m *countingMetrics) OnK8sWatchEvent(kind, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.n == nil {
		m.n = map[string]int{}
	}
	m.n[kind+"/"+outcome]++
}
func (m *countingMetrics) SetK8sWatchPending(int) {}
func (m *countingMetrics) get(k string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n[k]
}

func updatesOnly(_, op string) bool { return op == "update" }

func pod(name, rv string, created time.Time, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "app", ResourceVersion: rv,
			CreationTimestamp: metav1.NewTime(created)},
		Spec:   corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "web", Image: "web:1"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "web", RestartCount: restarts}}},
	}
}

// kubewatchData is kubewatch's CloudEventMessageData as it declares it, with
// runtime.Object fields. The watcher's payload must encode identically.
type kubewatchData struct {
	Operation   string         `json:"operation"`
	Kind        string         `json:"kind"`
	ClusterUID  string         `json:"clusterUid"`
	Description string         `json:"description"`
	APIVersion  string         `json:"apiVersion"`
	Obj         runtime.Object `json:"obj"`
	OldObj      runtime.Object `json:"oldObj"`
}

func TestEncode_MatchesKubewatchBytes(t *testing.T) {
	old := pod("web-0", "1", time.Unix(1700000000, 0), 1)
	cur := pod("web-0", "2", time.Unix(1700000000, 0), 2)
	got, err := encode(change{src: source{kind: "Pod", apiVersion: "v1"}, operation: "update", obj: cur, old: old})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(kubewatchData{
		Operation: "update", Kind: "Pod", APIVersion: "v1",
		Description: "A `Pod` in namespace `app` has been `Updated`:\n`web-0`",
		Obj:         cur, OldObj: old,
	})
	if string(got) != string(want) {
		t.Errorf("payload\n%s\nwant kubewatch's\n%s", got, want)
	}

	// A create carries no old object: kubewatch sent "oldObj": null.
	got, _ = encode(change{src: source{kind: "Node", apiVersion: "v1"}, operation: "create",
		obj: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}})
	if !strings.Contains(string(got), `"oldObj":null`) ||
		!strings.Contains(string(got), "A `Node` in namespace `` has been `Created`:\\n`n1`") {
		t.Errorf("create payload %s", got)
	}
}

func TestWatcher_DropsExistingObjectsAndQueuesUnchangedUpdates(t *testing.T) {
	start := time.Now()
	m := &countingMetrics{}
	w := New(Config{StartTime: start}, &recordingSink{}, func(string, string) bool { return true }, m, nil)
	s := source{kind: "Pod", apiVersion: "v1"}

	w.onAdd(s, pod("old", "1", start.Add(-time.Hour), 0))
	w.onAdd(s, pod("new", "1", start.Add(time.Second), 0))
	if got := m.get("pod/" + OutcomeBeforeStart); got != 1 {
		t.Errorf("dropped_existing = %d; want 1 (the pod from before start)", got)
	}
	if got := m.get("pod/" + OutcomeQueued); got != 1 {
		t.Errorf("queued = %d; want 1 (the pod created after start)", got)
	}

	// An unchanged update (a relist replay) is matched, as kubewatch did.
	p := pod("web-0", "7", start, 0)
	w.onUpdate(s, p, p.DeepCopy())
	if m.get("pod/"+OutcomeUnchanged) != 1 || m.get("pod/"+OutcomeQueued) != 2 {
		t.Errorf("outcomes = %v; want the unchanged update counted and queued", m.n)
	}
}

func TestWatcher_CoalescesUpdatesKeepingFirstOldAndLatestNew(t *testing.T) {
	m := &countingMetrics{}
	w := New(Config{}, &recordingSink{}, updatesOnly, m, nil)
	s := source{kind: "Pod", apiVersion: "v1"}
	v1, v2, v3 := pod("web-0", "1", time.Now(), 0), pod("web-0", "2", time.Now(), 1), pod("web-0", "3", time.Now(), 2)

	w.onUpdate(s, v1, v2)
	w.onUpdate(s, v2, v3)
	w.onAdd(s, pod("web-0", "4", time.Now().Add(time.Hour), 0)) // create: no matcher reads it

	changes := w.take("Pod/app/web-0")
	if len(changes) != 1 {
		t.Fatalf("changes = %d; want the two updates merged into one", len(changes))
	}
	if changes[0].old != v1 || changes[0].obj != v3 {
		t.Error("merged update should compare the first old object with the latest new one")
	}
	if m.get("pod/"+OutcomeCoalesced) != 1 || m.get("pod/"+OutcomeNoMatcher) != 1 {
		t.Errorf("outcomes = %v", m.n)
	}
}

func TestWatcher_ShedsBeyondMaxPendingObjects(t *testing.T) {
	m := &countingMetrics{}
	w := New(Config{MaxPending: 2}, &recordingSink{}, updatesOnly, m, nil)
	s := source{kind: "Pod", apiVersion: "v1"}
	for _, name := range []string{"a", "b", "c"} {
		w.onUpdate(s, pod(name, "1", time.Now(), 0), pod(name, "2", time.Now(), 1))
	}
	// A further update to an object already waiting still coalesces.
	w.onUpdate(s, pod("a", "2", time.Now(), 1), pod("a", "3", time.Now(), 2))
	if got := m.get("pod/" + OutcomeShed); got != 1 {
		t.Errorf("shed = %d; want 1 (the third object)", got)
	}
	if got := m.get("pod/" + OutcomeCoalesced); got != 1 {
		t.Errorf("coalesced = %d; want 1", got)
	}
}

// End to end on a fake clientset: an update reaches the sink with both
// objects; the factory's resync never does, though the factory resyncs often.
func TestWatcher_DeliversUpdatesAndNotResyncs(t *testing.T) {
	start := time.Now().Add(-time.Minute)
	cs := fake.NewSimpleClientset(pod("web-0", "1", start.Add(-time.Hour), 0))
	f := informers.NewSharedInformerFactory(cs, 50*time.Millisecond)
	sink := &recordingSink{}
	m := &countingMetrics{}
	w := New(Config{StartTime: start, Workers: 2}, sink, updatesOnly, m, nil)
	w.RegisterShared(f)
	// Another consumer of the same informer (discovery, in the runner) asks
	// for the factory's resync; it must get resyncs while the watcher does not.
	var resyncs atomic.Int32
	_, _ = f.Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(o, n any) {
			if o.(*corev1.Pod).ResourceVersion == n.(*corev1.Pod).ResourceVersion {
				resyncs.Add(1)
			}
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	waitFor(t, func() bool { return f.Core().V1().Pods().Informer().HasSynced() })
	updated := pod("web-0", "2", start.Add(-time.Hour), 3)
	if _, err := cs.CoreV1().Pods("app").UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(sink.snapshot()) == 1 })
	waitFor(t, func() bool { return resyncs.Load() >= 3 }) // several resyncs reached the other handler

	got := sink.snapshot()
	if len(got) != 1 {
		t.Fatalf("changes = %d; want only the update, no resyncs", len(got))
	}
	c := got[0]
	if c.operation != "update" || c.kind != "Pod" {
		t.Errorf("change = %s %s", c.operation, c.kind)
	}
	restarts := func(o any) any {
		return o.(map[string]any)["status"].(map[string]any)["containerStatuses"].([]any)[0].(map[string]any)["restartCount"]
	}
	if restarts(c.data["obj"]) != float64(3) || restarts(c.data["oldObj"]) != float64(0) {
		t.Errorf("obj/oldObj restarts = %v/%v; want 3/0", restarts(c.data["obj"]), restarts(c.data["oldObj"]))
	}
	if m.get("pod/"+OutcomeUnchanged) != 0 {
		t.Errorf("unchanged = %d; the factory's resyncs should never reach the watcher's handler", m.get("pod/"+OutcomeUnchanged))
	}
	if m.get("pod/"+OutcomeBeforeStart) != 1 {
		t.Error("the pod from the initial list should be dropped as existing")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Kinds no matcher reads get no informer, so nothing caches them.
func TestWatcher_SkipsKindsNoMatcherReads(t *testing.T) {
	f := informers.NewSharedInformerFactory(fake.NewSimpleClientset(), 0)
	w := New(Config{}, &recordingSink{}, func(kind, op string) bool { return kind == "Pod" && op == "update" }, nil, nil)
	w.RegisterShared(f)
	if len(w.synced) != 1 {
		t.Errorf("registered %d informers; want only Pods", len(w.synced))
	}
	stop := make(chan struct{})
	defer close(stop)
	f.Start(stop)
	started := f.WaitForCacheSync(stop)
	if len(started) != 1 {
		t.Errorf("factory holds %d informers; want 1", len(started))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWatcher_WarnsOnShedAtMostOncePerInterval(t *testing.T) {
	var buf strings.Builder
	w := New(Config{MaxPending: 1}, &recordingSink{}, updatesOnly, nil, slog.New(slog.NewTextHandler(&buf, nil)))
	s := source{kind: "Pod", apiVersion: "v1"}
	for _, name := range []string{"a", "b", "c", "d"} {
		w.onUpdate(s, pod(name, "1", time.Now(), 0), pod(name, "2", time.Now(), 1))
	}
	if got := strings.Count(buf.String(), "changes shed"); got != 1 {
		t.Errorf("shed warnings = %d; want 1 for a burst of three sheds\n%s", got, buf.String())
	}
}
