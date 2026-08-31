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

// An applied rightsizing could not be undone: the value written was recorded, the value replaced was
// not. The applier holds the object before it mutates it, so it captures what was there.
func TestApply_ReturnsPreviousResourcesForUndo(t *testing.T) {
	existing := deploymentWith("web", "shop", "app", map[string]any{
		"requests": map[string]any{"cpu": "100m", "memory": "128Mi"},
		"limits":   map[string]any{"memory": "512Mi"},
	})
	dyn := dynamicfake.NewSimpleDynamicClient(applyScheme(), existing)
	a := NewApplier(dyn)

	res, err := a.Handle(context.Background(), map[string]any{
		"kind": "Deployment", "name": "web", "namespace": "shop",
		"containers": []any{map[string]any{
			"container_name": "app",
			"cpu_request":    "250m",
			"memory_request": "256Mi",
		}},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	prev, _ := res.(map[string]any)["previous_containers"].([]map[string]any)
	if len(prev) != 1 {
		t.Fatalf("want 1 captured container, got %v", res.(map[string]any)["previous_containers"])
	}
	// Keyed the way action_params.containers is, so the undo is this list passed straight back.
	if prev[0]["container_name"] != "app" {
		t.Errorf("container_name = %v", prev[0]["container_name"])
	}
	if prev[0]["cpu_request"] != "100m" || prev[0]["memory_request"] != "128Mi" {
		t.Errorf("captured the wrong requests: %v", prev[0])
	}
	if prev[0]["memory_limit"] != "512Mi" {
		t.Errorf("captured the wrong limit: %v", prev[0])
	}
	// cpu had no limit. Recording "" would mean "remove the limit" to the apply path, so an undo
	// built from it would strip a limit that was never set.
	if _, present := prev[0]["cpu_limit"]; present {
		t.Errorf("absent cpu limit must stay absent, got %v", prev[0]["cpu_limit"])
	}
}

// A container named in the change but absent from the workload must not be recorded at all — an undo
// must never write blank resources onto something it did not touch.
func TestApply_DoesNotCaptureContainersItDidNotTouch(t *testing.T) {
	existing := deploymentWith("web", "shop", "app", map[string]any{
		"requests": map[string]any{"cpu": "100m"},
	})
	dyn := dynamicfake.NewSimpleDynamicClient(applyScheme(), existing)
	a := NewApplier(dyn)

	res, err := a.Handle(context.Background(), map[string]any{
		"kind": "Deployment", "name": "web", "namespace": "shop",
		"containers": []any{map[string]any{"container_name": "app", "cpu_request": "250m"}},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	prev, _ := res.(map[string]any)["previous_containers"].([]map[string]any)
	for _, entry := range prev {
		if entry["container_name"] != "app" {
			t.Errorf("captured a container the change never targeted: %v", entry)
		}
	}
}

// A quantity is legal as a bare number in a manifest (`cpu: 1`), which decodes to int64/float64.
// NestedString reports such a field as absent, and absent means "was unset" to the undo — so a
// numeric limit was dropped here and then REMOVED by the undo meant to restore it.
func TestApply_CapturesNumericQuantities(t *testing.T) {
	existing := deploymentWith("web", "shop", "app", map[string]any{
		"requests": map[string]any{"cpu": int64(1), "memory": int64(104857600)},
		"limits":   map[string]any{"cpu": float64(2)},
	})
	dyn := dynamicfake.NewSimpleDynamicClient(applyScheme(), existing)
	a := NewApplier(dyn)

	res, err := a.Handle(context.Background(), map[string]any{
		"kind": "Deployment", "name": "web", "namespace": "shop",
		"containers": []any{map[string]any{"container_name": "app", "cpu_request": "500m"}},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	prev, _ := res.(map[string]any)["previous_containers"].([]map[string]any)
	if len(prev) != 1 {
		t.Fatalf("want 1 captured container, got %v", res.(map[string]any)["previous_containers"])
	}
	if prev[0]["cpu_request"] != "1" {
		t.Errorf("cpu_request = %v; want \"1\" — a numeric quantity must survive capture", prev[0]["cpu_request"])
	}
	if prev[0]["memory_request"] != "104857600" {
		t.Errorf("memory_request = %v; want \"104857600\"", prev[0]["memory_request"])
	}
	if prev[0]["cpu_limit"] != "2" {
		t.Errorf("cpu_limit = %v; want \"2\"", prev[0]["cpu_limit"])
	}
}
