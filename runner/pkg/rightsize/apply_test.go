package rightsize

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// applyScheme registers the GVRs the apply path understands so the dynamic fake
// recognises them (the fake 404s a Get for an unregistered GVR). Skips the
// no-op Job entry, which has no GVR.
func applyScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for kind, entry := range applyKinds {
		if entry.noop {
			continue
		}
		gvr := entry.gvr
		s.AddKnownTypeWithName(
			schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: kind},
			&unstructured.Unstructured{},
		)
		s.AddKnownTypeWithName(
			schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: kind + "List"},
			&unstructured.UnstructuredList{},
		)
	}
	return s
}

// deploymentWith builds a Deployment with one container carrying the given
// resources (nil resources → no resources block).
func deploymentWith(name, ns, container string, resources map[string]any) *unstructured.Unstructured {
	c := map[string]any{"name": container, "image": "nginx"}
	if resources != nil {
		c["resources"] = resources
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{"containers": []any{c}},
			},
		},
	}}
	return u
}

func getContainer0(t *testing.T, obj *unstructured.Unstructured) map[string]any {
	t.Helper()
	containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil || len(containers) == 0 {
		t.Fatalf("no containers: %v", err)
	}
	c, _ := containers[0].(map[string]any)
	return c
}

func TestApply_Deployment_SetsRequestsLimitsAndAnnotations(t *testing.T) {
	existing := deploymentWith("web", "shop", "app", map[string]any{
		"requests": map[string]any{"cpu": "100m", "memory": "128Mi"},
	})
	dyn := dynamicfake.NewSimpleDynamicClient(applyScheme(), existing)
	a := NewApplier(dyn)

	params := map[string]any{
		"kind":      "Deployment",
		"name":      "web",
		"namespace": "shop",
		"containers": []any{
			map[string]any{
				"container_name": "app",
				"cpu_request":    "250m",
				"cpu_limit":      "500m",
				"memory_request": "256Mi",
				"memory_limit":   "512Mi",
			},
		},
		"annotations": map[string]any{
			"recommendation_apply.vertical-scaler": `{"id":"r1"}`,
			"skip_me":                              "None", // filtered out
			"skip_nil":                             nil,    // filtered out
		},
	}

	res, err := a.Handle(context.Background(), params)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if m, _ := res.(map[string]any); m["success"] != true {
		t.Fatalf("want success=true, got %v", res)
	}

	// Read back the persisted object.
	got, err := dyn.Resource(applyKinds["Deployment"].gvr).Namespace("shop").
		Get(context.Background(), "web", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := getContainer0(t, got)
	r := c["resources"].(map[string]any)
	req := r["requests"].(map[string]any)
	lim := r["limits"].(map[string]any)
	if req["cpu"] != "250m" || req["memory"] != "256Mi" {
		t.Errorf("requests = %v; want cpu=250m memory=256Mi", req)
	}
	if lim["cpu"] != "500m" || lim["memory"] != "512Mi" {
		t.Errorf("limits = %v; want cpu=500m memory=512Mi", lim)
	}
	ann := got.GetAnnotations()
	if ann["recommendation_apply.vertical-scaler"] != `{"id":"r1"}` {
		t.Errorf("annotation not applied: %v", ann)
	}
	if _, ok := ann["skip_me"]; ok {
		t.Errorf(`"None"-valued annotation should be filtered: %v`, ann)
	}
	if _, ok := ann["skip_nil"]; ok {
		t.Errorf("nil-valued annotation should be filtered: %v", ann)
	}
}

func TestApply_ClearsResourceWhenValueEmpty(t *testing.T) {
	existing := deploymentWith("web", "shop", "app", map[string]any{
		"requests": map[string]any{"cpu": "100m", "memory": "128Mi"},
		"limits":   map[string]any{"cpu": "200m", "memory": "256Mi"},
	})
	dyn := dynamicfake.NewSimpleDynamicClient(applyScheme(), existing)
	a := NewApplier(dyn)

	// Only memory_request set; cpu_* and memory_limit are null → cleared.
	params := map[string]any{
		"kind": "Deployment", "name": "web", "namespace": "shop",
		"containers": []any{map[string]any{
			"container_name": "app",
			"memory_request": "200Mi",
		}},
	}
	if _, err := a.Handle(context.Background(), params); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	got, _ := dyn.Resource(applyKinds["Deployment"].gvr).Namespace("shop").
		Get(context.Background(), "web", metav1.GetOptions{})
	r := getContainer0(t, got)["resources"].(map[string]any)
	req := r["requests"].(map[string]any)
	if req["memory"] != "200Mi" {
		t.Errorf("memory request = %v; want 200Mi", req["memory"])
	}
	if _, ok := req["cpu"]; ok {
		t.Errorf("cpu request should have been cleared: %v", req)
	}
	// memory limit cleared; cpu limit also cleared → limits map removed.
	if _, ok := r["limits"]; ok {
		t.Errorf("limits should have been removed once empty: %v", r)
	}
}

func TestApply_RequestAboveLimitRejected(t *testing.T) {
	a := NewApplier(dynamicfake.NewSimpleDynamicClient(applyScheme()))
	params := map[string]any{
		"kind": "Deployment", "name": "web", "namespace": "shop",
		"containers": []any{map[string]any{
			"container_name": "app",
			"cpu_request":    "500m",
			"cpu_limit":      "250m", // request > limit
		}},
	}
	if _, err := a.Handle(context.Background(), params); err == nil {
		t.Fatal("expected error for request > limit, got nil")
	}
}

func TestApply_UnsupportedKind(t *testing.T) {
	a := NewApplier(dynamicfake.NewSimpleDynamicClient(applyScheme()))
	// ReplicaSet is intentionally unsupported (legacy parity).
	params := map[string]any{
		"kind": "ReplicaSet", "name": "web", "namespace": "shop",
		"containers": []any{map[string]any{"container_name": "app", "cpu_request": "100m"}},
	}
	if _, err := a.Handle(context.Background(), params); err == nil {
		t.Fatal("expected unsupported-kind error, got nil")
	}
}

func TestApply_JobIsNoOpSuccess(t *testing.T) {
	a := NewApplier(dynamicfake.NewSimpleDynamicClient(applyScheme()))
	params := map[string]any{
		"kind": "Job", "name": "batch", "namespace": "shop",
		"containers": []any{map[string]any{"container_name": "app", "cpu_request": "100m"}},
	}
	res, err := a.Handle(context.Background(), params)
	if err != nil {
		t.Fatalf("Job should be a no-op success, got err: %v", err)
	}
	if m, _ := res.(map[string]any); m["success"] != true {
		t.Fatalf("Job no-op should report success, got %v", res)
	}
}

func TestApply_EmptyContainersRejected(t *testing.T) {
	a := NewApplier(dynamicfake.NewSimpleDynamicClient(applyScheme()))
	params := map[string]any{
		"kind": "Deployment", "name": "web", "namespace": "shop",
		"containers": []any{},
	}
	if _, err := a.Handle(context.Background(), params); err == nil {
		t.Fatal("expected error for empty containers, got nil")
	}
}

func TestApply_WorkloadNotFound(t *testing.T) {
	a := NewApplier(dynamicfake.NewSimpleDynamicClient(applyScheme()))
	params := map[string]any{
		"kind": "Deployment", "name": "ghost", "namespace": "shop",
		"containers": []any{map[string]any{"container_name": "app", "cpu_request": "100m"}},
	}
	if _, err := a.Handle(context.Background(), params); err == nil {
		t.Fatal("expected not-found error, got nil")
	}
}
