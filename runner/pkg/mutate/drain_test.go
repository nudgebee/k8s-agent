package mutate

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func podOnNode(name, ns, node string, opts ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       corev1.PodSpec{NodeName: node},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func withOwner(kind, name string, controller bool) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.OwnerReferences = append(p.OwnerReferences, metav1.OwnerReference{
			Kind: kind, Name: name, Controller: ptr.To(controller),
		})
	}
}

func withMirror() func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations["kubernetes.io/config.mirror"] = "1"
	}
}

func withEmptyDir() func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
			Name:         "scratch",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
}

func TestSkipReason(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		opts DrainOptions
		want string // empty = NOT skipped
	}{
		{"controlled-pod-evicted", podOnNode("a", "n", "node-1", withOwner("ReplicaSet", "rs-1", true)),
			DrainOptions{IgnoreDaemonSets: true}, ""},
		{"daemonset-skipped", podOnNode("a", "n", "node-1", withOwner("DaemonSet", "ds-1", true)),
			DrainOptions{IgnoreDaemonSets: true}, "daemonset pod"},
		{"daemonset-evicted-when-flag-off", podOnNode("a", "n", "node-1", withOwner("DaemonSet", "ds-1", true)),
			DrainOptions{IgnoreDaemonSets: false}, ""},
		{"mirror-skipped", podOnNode("a", "n", "node-1", withMirror()),
			DrainOptions{IgnoreDaemonSets: true}, "mirror pod"},
		{"unmanaged-skipped-without-force", podOnNode("a", "n", "node-1"),
			DrainOptions{IgnoreDaemonSets: true}, "unmanaged pod (set Force=true to delete)"},
		{"unmanaged-evicted-with-force", podOnNode("a", "n", "node-1"),
			DrainOptions{IgnoreDaemonSets: true, Force: true}, ""},
		{"emptydir-skipped-by-default", podOnNode("a", "n", "node-1", withOwner("ReplicaSet", "r", true), withEmptyDir()),
			DrainOptions{IgnoreDaemonSets: true}, "uses emptyDir (set DeleteEmptyDirData=true to evict)"},
		{"emptydir-evicted-with-flag", podOnNode("a", "n", "node-1", withOwner("ReplicaSet", "r", true), withEmptyDir()),
			DrainOptions{IgnoreDaemonSets: true, DeleteEmptyDirData: true}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := skipReason(c.pod, c.opts)
			if got != c.want {
				t.Errorf("got %q; want %q", got, c.want)
			}
		})
	}
}

func TestDrain_RequiresNodeName(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	_, err := m.Drain(context.Background(), "", DrainOptions{})
	if err == nil {
		t.Error("expected error for missing node name")
	}
}

func TestDrain_RequiresClient(t *testing.T) {
	m := &Mutator{}
	_, err := m.Drain(context.Background(), "node-1", DrainOptions{})
	if err == nil {
		t.Error("expected error for missing client")
	}
}

func TestDrain_CordonsAndPartitionsPods(t *testing.T) {
	cs := fake.NewClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		// Will be evicted
		podOnNode("worker", "shop", "node-1", withOwner("ReplicaSet", "rs", true)),
		// Skipped: daemonset
		podOnNode("ds-pod", "kube-system", "node-1", withOwner("DaemonSet", "node-exporter", true)),
		// Skipped: mirror
		podOnNode("mirror", "kube-system", "node-1", withMirror()),
		// Skipped: unmanaged (no controller)
		podOnNode("orphan", "default", "node-1"),
	)

	// Make eviction return success immediately, then pretend the pod is gone
	// for the worker pod. The fake client's default handler errors on
	// EvictV1; we tolerate that and rely on the not-found check to short-circuit.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = cs.CoreV1().Pods("shop").Delete(context.Background(), "worker", metav1.DeleteOptions{})
	}()

	m := New(cs, "", nil)
	res, err := m.Drain(context.Background(), "node-1", DrainOptions{
		IgnoreDaemonSets: true,
		Timeout:          3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cordon happened.
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Error("node not cordoned")
	}

	// Skipped: daemonset, mirror, orphan = 3.
	if len(res.Skipped) != 3 {
		t.Errorf("skipped = %d; want 3 (got %+v)", len(res.Skipped), res.Skipped)
	}
	// Evicted: only the worker.
	if len(res.Pods) != 1 {
		t.Errorf("evicted = %d; want 1 (got %+v)", len(res.Pods), res.Pods)
	}
}

func TestDrain_HandlerParsing(t *testing.T) {
	cs := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}})
	m := New(cs, "", nil)
	hs := Handlers(m)
	got, err := hs["drain"](context.Background(), map[string]any{
		"node":                 "node-1",
		"ignore_daemonsets":    true,
		"delete_emptydir_data": true,
		"force":                true,
		"timeout_seconds":      float64(2),
		"grace_period_seconds": float64(15),
	})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := got.(*DrainResult)
	if !ok {
		t.Fatalf("got %T; want *DrainResult", got)
	}
	if r.Node != "node-1" {
		t.Errorf("node = %s", r.Node)
	}
}
