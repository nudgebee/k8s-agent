//go:build live

// Live coverage for revert_workload against a REAL apiserver. The bug this
// action replaces was invisible to fake-client tests: the dynamic fake accepts
// whatever map you hand it, while the real apiserver drops unknown fields and
// then rejects what's left. Only a real Update proves the revert survives
// validation.
//
// Run:
//
//	go test -tags live ./pkg/mutate -run TestLiveRevert -v -timeout 5m
//
// Creates a throwaway namespace and deletes it on cleanup.
package mutate

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// applyChange performs the test's OWN pre-revert mutation. It retries on
// conflict because the workload controllers write status the moment the object
// is created, so a bare Get-then-Update in test setup races them. (The action
// under test retries for the same reason — see revert.go.)
func applyChange(t *testing.T, mutate func() error) {
	t.Helper()
	if err := retry.RetryOnConflict(retry.DefaultRetry, mutate); err != nil {
		t.Fatalf("apply change: %v", err)
	}
}

// liveRevertDeployment mirrors the fields that made the old snapshot-replay
// revert fail: a selector (immutable, and invalid when empty) and a named
// container port (containerPort is a required value).
func liveRevertDeployment(name string) *appsv1.Deployment {
	labels := map[string]string{"app.kubernetes.io/name": name}
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
					Annotations: map[string]string{
						"rollme":                            "zde7X",
						"kubectl.kubernetes.io/restartedAt": "2026-07-30T12:25:40Z",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "registry.k8s.io/pause:3.9",
						Ports: []corev1.ContainerPort{{
							Name:          "http",
							ContainerPort: 8000,
							Protocol:      corev1.ProtocolTCP,
						}},
					}},
				},
			},
		},
	}
}

func TestLiveRevertWorkload_Deployment(t *testing.T) {
	cs, _, m := liveSetup(t)
	ns := newNamespace(t, cs)
	ctx := context.Background()

	const name = "revert-target"
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, liveRevertDeployment(name), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	// Simulate the config change the event recorded: new image + new rollme.
	// Also add a replica so we can prove the revert leaves it alone.
	applyChange(t, func() error {
		live, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		live.Spec.Template.Annotations["rollme"] = "c4cjS"
		live.Spec.Template.Spec.Containers[0].Image = "registry.k8s.io/pause:3.10"
		two := int32(2)
		live.Spec.Replicas = &two
		_, err = cs.AppsV1().Deployments(ns).Update(ctx, live, metav1.UpdateOptions{})
		return err
	})

	// Revert exactly what the diff evidence records for this change.
	resp, err := m.RevertWorkload(ctx, "Deployment", ns, name, []RevertEntry{
		{Path: "spec.template.metadata.annotations.rollme", Old: "zde7X", HasOld: true},
		{Path: "spec.template.spec.containers[0].image", Old: "registry.k8s.io/pause:3.9", HasOld: true},
	})
	if err != nil {
		t.Fatalf("RevertWorkload: %v", err)
	}
	t.Logf("revert response: %v", resp.(map[string]any)["message"])

	got, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after revert: %v", err)
	}
	if v := got.Spec.Template.Annotations["rollme"]; v != "zde7X" {
		t.Errorf("rollme = %q; want zde7X", v)
	}
	if v := got.Spec.Template.Spec.Containers[0].Image; v != "registry.k8s.io/pause:3.9" {
		t.Errorf("image = %q; want pause:3.9", v)
	}
	// Fields the change didn't touch must survive. A snapshot replay would have
	// rolled the replica count back too.
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 2 {
		t.Errorf("replicas = %v; revert clobbered an unrelated field", got.Spec.Replicas)
	}
	// The two fields the apiserver rejected under the old implementation.
	if got.Spec.Selector == nil || len(got.Spec.Selector.MatchLabels) == 0 {
		t.Error("selector was emptied by the revert")
	}
	if got.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort != 8000 {
		t.Error("containerPort was dropped by the revert")
	}
}

// An annotation key containing dots is the case a naive path split mangles,
// and the apiserver is what ultimately rejects the mangled result.
func TestLiveRevertWorkload_DottedAnnotationKey(t *testing.T) {
	cs, _, m := liveSetup(t)
	ns := newNamespace(t, cs)
	ctx := context.Background()

	const name = "revert-dotted"
	if _, err := cs.AppsV1().Deployments(ns).Create(ctx, liveRevertDeployment(name), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	mutateAnnotation(t, cs, ns, name, "kubectl.kubernetes.io/restartedAt", "2026-08-25T00:00:00Z")

	if _, err := m.RevertWorkload(ctx, "Deployment", ns, name, []RevertEntry{{
		Path:   "spec.template.metadata.annotations.kubectl.kubernetes.io/restartedAt",
		Old:    "2026-07-30T12:25:40Z",
		HasOld: true,
	}}); err != nil {
		t.Fatalf("RevertWorkload: %v", err)
	}

	got, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after revert: %v", err)
	}
	ann := got.Spec.Template.Annotations
	if ann["kubectl.kubernetes.io/restartedAt"] != "2026-07-30T12:25:40Z" {
		t.Errorf("restartedAt = %q", ann["kubectl.kubernetes.io/restartedAt"])
	}
	if _, split := ann["kubectl"]; split {
		t.Error("the dotted annotation key was split into separate annotations")
	}
}

func mutateAnnotation(t *testing.T, cs kubernetes.Interface, ns, name, key, value string) {
	t.Helper()
	applyChange(t, func() error {
		live, err := cs.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		live.Spec.Template.Annotations[key] = value
		_, err = cs.AppsV1().Deployments(ns).Update(context.Background(), live, metav1.UpdateOptions{})
		return err
	})
}

// Argo Rollouts is a CRD, reached through the same dynamic client but a
// different GVR (argoproj.io/v1alpha1). An event on a Rollout-managed workload
// arrives with kind "Rollout" — cloud_resourses stores the service key as
// "<ns>/Rollout/<name>" — so the revert must drive the CR, not a Deployment.
// The dev cluster runs the real rollouts controller, so this also shows the
// revert survives a reconcile rather than being immediately undone.
func TestLiveRevertWorkload_ArgoRollout(t *testing.T) {
	cs, restCfg, m := liveSetup(t)
	ns := newNamespace(t, cs)
	ctx := context.Background()

	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}
	gvr := supportedWorkloadKinds["Rollout"].gvr
	if _, err := dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{Limit: 1}); err != nil {
		t.Skipf("Argo Rollouts CRD not installed: %v", err)
	}

	const name = "revert-rollout"
	labels := map[string]any{"app": name}
	rollout := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"replicas": int64(1),
			"selector": map[string]any{"matchLabels": labels},
			"strategy": map[string]any{
				"canary": map[string]any{
					"steps": []any{map[string]any{"setWeight": int64(50)}},
				},
			},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels":      labels,
					"annotations": map[string]any{"rollme": "zde7X"},
				},
				"spec": map[string]any{
					"containers": []any{map[string]any{
						"name":  "app",
						"image": "registry.k8s.io/pause:3.9",
						"ports": []any{map[string]any{"containerPort": int64(8000), "name": "http"}},
					}},
				},
			},
		},
	}}
	if _, err := dyn.Resource(gvr).Namespace(ns).Create(ctx, rollout, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create rollout: %v", err)
	}

	// The config change the event records.
	applyChange(t, func() error {
		live, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		containers, _, _ := unstructured.NestedSlice(live.Object, "spec", "template", "spec", "containers")
		containers[0].(map[string]any)["image"] = "registry.k8s.io/pause:3.10"
		if err := unstructured.SetNestedSlice(live.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(live.Object, "c4cjS",
			"spec", "template", "metadata", "annotations", "rollme"); err != nil {
			return err
		}
		_, err = dyn.Resource(gvr).Namespace(ns).Update(ctx, live, metav1.UpdateOptions{})
		return err
	})

	resp, err := m.RevertWorkload(ctx, "Rollout", ns, name, []RevertEntry{
		{Path: "spec.template.metadata.annotations.rollme", Old: "zde7X", HasOld: true},
		{Path: "spec.template.spec.containers[0].image", Old: "registry.k8s.io/pause:3.9", HasOld: true},
	})
	if err != nil {
		t.Fatalf("RevertWorkload(Rollout): %v", err)
	}
	t.Logf("revert response: %v", resp.(map[string]any)["message"])

	got, err := dyn.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get after revert: %v", err)
	}
	gotContainers, _, _ := unstructured.NestedSlice(got.Object, "spec", "template", "spec", "containers")
	if gotContainers[0].(map[string]any)["image"] != "registry.k8s.io/pause:3.9" {
		t.Errorf("image = %v; want pause:3.9", gotContainers[0].(map[string]any)["image"])
	}
	ann, _, _ := unstructured.NestedString(got.Object, "spec", "template", "metadata", "annotations", "rollme")
	if ann != "zde7X" {
		t.Errorf("rollme = %q; want zde7X", ann)
	}
	// The CRD's own required fields must survive the revert.
	sel, found, _ := unstructured.NestedMap(got.Object, "spec", "selector", "matchLabels")
	if !found || sel["app"] != name {
		t.Errorf("selector lost: found=%v %v", found, sel)
	}
	ports, found, _ := unstructured.NestedSlice(gotContainers[0].(map[string]any), "ports")
	if !found || len(ports) == 0 || ports[0].(map[string]any)["containerPort"] != int64(8000) {
		t.Errorf("containerPort lost: found=%v %v", found, ports)
	}
}
