package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func nodeWithAddress(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: ip},
				{Type: corev1.NodeHostName, Address: name},
			},
		},
	}
}

func TestNodeLocator_NodeForPod(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "node-exporter-skqnn", Namespace: "monitoring"},
		Spec:       corev1.PodSpec{NodeName: "gke-pool-abc"},
	})
	l := newNodeLocator(cs)

	if got := l.NodeForPod(context.Background(), "monitoring", "node-exporter-skqnn"); got != "gke-pool-abc" {
		t.Errorf("NodeForPod = %q, want gke-pool-abc", got)
	}
	// A pod that is not there resolves to nothing rather than a guess.
	if got := l.NodeForPod(context.Background(), "monitoring", "gone"); got != "" {
		t.Errorf("NodeForPod(missing) = %q, want empty", got)
	}
	// A namespace-less lookup is refused — Pods().Get needs one, and guessing
	// a namespace would return another tenant's pod of the same name.
	if got := l.NodeForPod(context.Background(), "", "node-exporter-skqnn"); got != "" {
		t.Errorf("NodeForPod(no namespace) = %q, want empty", got)
	}
}

func TestNodeLocator_NodeForIP(t *testing.T) {
	cs := fake.NewSimpleClientset(
		nodeWithAddress("gke-pool-abc", "10.128.0.14"),
		nodeWithAddress("gke-pool-def", "10.128.0.24"),
	)
	l := newNodeLocator(cs)

	if got := l.NodeForIP(context.Background(), "10.128.0.24"); got != "gke-pool-def" {
		t.Errorf("NodeForIP = %q, want gke-pool-def", got)
	}
	// The hostname address form resolves too — some clusters report the node
	// name in `instance` rather than an IP.
	if got := l.NodeForIP(context.Background(), "gke-pool-abc"); got != "gke-pool-abc" {
		t.Errorf("NodeForIP(hostname) = %q, want gke-pool-abc", got)
	}
	if got := l.NodeForIP(context.Background(), "10.128.0.99"); got != "" {
		t.Errorf("NodeForIP(unknown) = %q, want empty", got)
	}
}

// TestNodeLocator_ConcurrentMissesListOnce is the regression test for the
// stampede: alerts are handled on a goroutine pool, so a burst for an address
// the cache has never seen used to have every goroutine miss and list the
// nodes concurrently — one API call per alert, which is what the cache exists
// to prevent.
func TestNodeLocator_ConcurrentMissesListOnce(t *testing.T) {
	cs := fake.NewSimpleClientset(nodeWithAddress("gke-pool-abc", "10.128.0.14"))

	var lists atomic.Int32
	release := make(chan struct{})
	cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists.Add(1)
		// Hold the first LIST open so every goroutine is inside NodeForIP
		// before any of them completes — without serialization they would all
		// reach the API.
		<-release
		return false, nil, nil
	})

	l := newNodeLocator(cs)

	const callers = 16
	var wg sync.WaitGroup
	results := make([]string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = l.NodeForIP(context.Background(), "10.128.0.14")
		}(i)
	}
	close(release)
	wg.Wait()

	if got := lists.Load(); got != 1 {
		t.Errorf("node LIST calls = %d, want 1 — concurrent misses must share one refresh", got)
	}
	for i, got := range results {
		if got != "gke-pool-abc" {
			t.Errorf("caller %d resolved %q, want gke-pool-abc — a caller that waited for the refresh must read its result", i, got)
		}
	}
}
