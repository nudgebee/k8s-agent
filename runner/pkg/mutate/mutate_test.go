package mutate

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clienttesting "k8s.io/client-go/testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func newPod(name, ns string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

func newJob(name, ns string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

func newNode(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func newDeployment(name, ns string) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
}

func TestNew_NoClient(t *testing.T) {
	m := &Mutator{}
	if err := m.DeletePod(context.Background(), "n", "p", nil); err == nil {
		t.Error("expected error when client unset")
	}
}

func TestDeletePod_RemovesPodFromCluster(t *testing.T) {
	cs := fake.NewClientset(newPod("frontend", "shop"))
	m := New(cs, "", nil)
	if err := m.DeletePod(context.Background(), "shop", "frontend", nil); err != nil {
		t.Fatal(err)
	}
	_, err := cs.CoreV1().Pods("shop").Get(context.Background(), "frontend", metav1.GetOptions{})
	if err == nil {
		t.Error("pod still present after DeletePod")
	}
}

func TestDeletePod_RequiresNamespaceAndName(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	if err := m.DeletePod(context.Background(), "", "p", nil); err == nil {
		t.Error("missing namespace should error")
	}
	if err := m.DeletePod(context.Background(), "n", "", nil); err == nil {
		t.Error("missing name should error")
	}
}

func TestDeleteJob_PassesBackgroundPropagation(t *testing.T) {
	cs := fake.NewClientset(newJob("j1", "ns"))
	var capturedDeleteOpts metav1.DeleteOptions
	cs.PrependReactor("delete", "jobs", func(a clienttesting.Action) (bool, runtime.Object, error) {
		da := a.(clienttesting.DeleteActionImpl)
		capturedDeleteOpts = da.DeleteOptions
		return false, nil, nil // fall through to default reactor
	})

	m := New(cs, "", nil)
	if err := m.DeleteJob(context.Background(), "ns", "j1"); err != nil {
		t.Fatal(err)
	}
	if capturedDeleteOpts.PropagationPolicy == nil || *capturedDeleteOpts.PropagationPolicy != metav1.DeletePropagationBackground {
		t.Errorf("PropagationPolicy = %v; want Background", capturedDeleteOpts.PropagationPolicy)
	}
}

func TestCordonUncordon_TogglesUnschedulable(t *testing.T) {
	cs := fake.NewClientset(newNode("node-1"))
	m := New(cs, "", nil)

	if err := m.Cordon(context.Background(), "node-1"); err != nil {
		t.Fatal(err)
	}
	n, _ := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if !n.Spec.Unschedulable {
		t.Error("Cordon: Unschedulable still false")
	}

	if err := m.Uncordon(context.Background(), "node-1"); err != nil {
		t.Fatal(err)
	}
	n, _ = cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if n.Spec.Unschedulable {
		t.Error("Uncordon: Unschedulable still true")
	}
}

func TestCordon_RequiresNodeName(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	if err := m.Cordon(context.Background(), ""); err == nil {
		t.Error("expected error for missing node name")
	}
}

func TestRolloutRestart_PatchesAnnotation(t *testing.T) {
	cs := fake.NewClientset(newDeployment("frontend", "shop"))

	var patchedAt string
	cs.PrependReactor("patch", "deployments", func(a clienttesting.Action) (bool, runtime.Object, error) {
		pa := a.(clienttesting.PatchActionImpl)
		patchedAt = string(pa.Patch)
		return false, nil, nil
	})

	m := New(cs, "", nil)
	if err := m.RolloutRestart(context.Background(), "deployment", "shop", "frontend"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patchedAt, "kubectl.kubernetes.io/restartedAt") {
		t.Errorf("patch missing restartedAt annotation: %s", patchedAt)
	}
}

func TestRolloutRestart_StatefulSetAndDaemonSet(t *testing.T) {
	cs := fake.NewClientset()
	if err := cs.Tracker().Add(&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "ss", Namespace: "ns"}}); err != nil {
		t.Fatal(err)
	}
	if err := cs.Tracker().Add(&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "ds", Namespace: "ns"}}); err != nil {
		t.Fatal(err)
	}
	m := New(cs, "", nil)

	if err := m.RolloutRestart(context.Background(), "statefulset", "ns", "ss"); err != nil {
		t.Errorf("statefulset: %v", err)
	}
	if err := m.RolloutRestart(context.Background(), "daemonset", "ns", "ds"); err != nil {
		t.Errorf("daemonset: %v", err)
	}
}

func TestRolloutRestart_RejectsUnknownKind(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	err := m.RolloutRestart(context.Background(), "replicaset", "ns", "rs")
	if err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Errorf("expected unsupported-kind error, got %v", err)
	}
}

func TestRolloutRestart_RequiresAllFields(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	if err := m.RolloutRestart(context.Background(), "", "ns", "n"); err == nil {
		t.Error("missing kind should error")
	}
	if err := m.RolloutRestart(context.Background(), "deployment", "", "n"); err == nil {
		t.Error("missing namespace should error")
	}
	if err := m.RolloutRestart(context.Background(), "deployment", "ns", ""); err == nil {
		t.Error("missing name should error")
	}
}

// _ = schema is just to keep imports tidy across test files.
var _ = schema.GroupVersionResource{}
