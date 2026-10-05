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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	fake "k8s.io/client-go/kubernetes/fake"
)

// An informer resync replays every cached object through UpdateFunc with an
// unchanged ResourceVersion. Those replays must not become incremental posts:
// the periodic full snapshot already re-sends everything, and posting them too
// doubles the discovery volume the collector has to drain. A real change (new
// ResourceVersion) must still go out as an incremental post.
func TestService_ResyncReplaysAreNotPosted(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "frontend",
			Namespace:         "shop",
			ResourceVersion:   "42",
			CreationTimestamp: metav1.Now(),
		},
		Spec:   corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "web", Image: "nginx:1.27"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cs := fake.NewClientset(pod)

	var (
		mu           sync.Mutex
		incrementals int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env Envelope
		_ = json.Unmarshal(body, &env)
		if !env.FullLoad {
			mu.Lock()
			incrementals++
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return incrementals
	}

	// 1s is the shortest resync client-go allows for an event handler.
	svc := NewService(cs, NewSink(srv.URL, "s", "a", "c", slog.Default()), time.Second, slog.Default())
	svc.RegisterPods()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	// Let startup (initial snapshot + the initial Add) settle, then sit through
	// two resync periods with nothing changing.
	time.Sleep(800 * time.Millisecond)
	settled := count()
	time.Sleep(2200 * time.Millisecond)
	if got := count(); got != settled {
		t.Fatalf("incremental posts grew from %d to %d across resyncs with no object change", settled, got)
	}

	updated := pod.DeepCopy()
	updated.ResourceVersion = "43"
	updated.Labels = map[string]string{"app": "frontend"}
	if _, err := cs.CoreV1().Pods("shop").Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && count() == settled {
		time.Sleep(20 * time.Millisecond)
	}
	if count() == settled {
		t.Fatal("a real change (new ResourceVersion) was not posted as an incremental update")
	}
}

func TestSameResourceVersion(t *testing.T) {
	pod := func(rv string) *corev1.Pod { return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: rv}} }
	rollout := func(rv string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetResourceVersion(rv)
		return u
	}
	cases := []struct {
		name     string
		old, new any
		want     bool
	}{
		{"typed resync replay", pod("7"), pod("7"), true},
		{"typed real change", pod("7"), pod("8"), false},
		{"unstructured resync replay", rollout("7"), rollout("7"), true},
		{"unstructured real change", rollout("7"), rollout("8"), false},
		{"empty resource versions", pod(""), pod(""), false},
		{"not an object", "a", "a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameResourceVersion(tc.old, tc.new); got != tc.want {
				t.Fatalf("sameResourceVersion = %v; want %v", got, tc.want)
			}
		})
	}
}
