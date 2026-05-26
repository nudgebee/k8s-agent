package mutate

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// workloadScheme registers every kind ReplaceWorkload accepts so the
// dynamic fake recognises the GVRs. The fake silently fails when a GVR
// isn't registered — tests would 404 the Get call before any Replace
// logic runs.
func workloadScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	for kind, entry := range supportedWorkloadKinds {
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

func newWorkloadManifest(apiVersion, kind, name, namespace string, extraSpec map[string]any) map[string]any {
	spec := map[string]any{"replicas": int64(2)}
	for k, v := range extraSpec {
		spec[k] = v
	}
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	return map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   meta,
		"spec":       spec,
	}
}

// seedExisting pre-seeds the dynamic fake with an existing object so
// ReplaceWorkload's Get-then-Update path has something to read for the
// ResourceVersion preservation step.
func seedExisting(apiVersion, kind, name, namespace, rv string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	parts := strings.SplitN(apiVersion, "/", 2)
	group, version := "", parts[0]
	if len(parts) == 2 {
		group, version = parts[0], parts[1]
	}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: group, Version: version, Kind: kind})
	u.SetName(name)
	if namespace != "" {
		u.SetNamespace(namespace)
	}
	u.SetResourceVersion(rv)
	return u
}

// ---------- happy path per kind ----------

func TestReplaceWorkload_Deployment(t *testing.T) {
	existing := seedExisting("apps/v1", "Deployment", "web", "shop", "100")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	body := newWorkloadManifest("apps/v1", "Deployment", "web", "shop", map[string]any{"replicas": int64(5)})
	got, err := m.ReplaceWorkload(context.Background(), "Deployment", "shop", "web", body)
	if err != nil {
		t.Fatal(err)
	}
	gotMap := got.(map[string]any)
	spec := gotMap["spec"].(map[string]any)
	// Round-trip through encoding/json normalises numbers to float64
	// — that's what unstructured.Unstructured carries, and what the fake
	// echoes back. Compare as float64 to avoid the int64-vs-float64 trap.
	if spec["replicas"] != float64(5) {
		t.Errorf("replicas = %v (%T); want 5", spec["replicas"], spec["replicas"])
	}
	// Apiserver echoes ResourceVersion through; the fake doesn't bump it
	// but does preserve it from the body, so we should see "100".
	if rv := gotMap["metadata"].(map[string]any)["resourceVersion"]; rv != "100" {
		t.Errorf("resourceVersion = %v; want 100 (preserved from existing)", rv)
	}
}

func TestReplaceWorkload_DaemonSet(t *testing.T) {
	existing := seedExisting("apps/v1", "DaemonSet", "node-exporter", "monitoring", "20")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	body := newWorkloadManifest("apps/v1", "DaemonSet", "node-exporter", "monitoring", nil)
	if _, err := m.ReplaceWorkload(context.Background(), "DaemonSet", "monitoring", "node-exporter", body); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceWorkload_StatefulSet(t *testing.T) {
	existing := seedExisting("apps/v1", "StatefulSet", "db", "data", "5")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	body := newWorkloadManifest("apps/v1", "StatefulSet", "db", "data", nil)
	if _, err := m.ReplaceWorkload(context.Background(), "StatefulSet", "data", "db", body); err != nil {
		t.Fatal(err)
	}
}

// TestReplaceWorkload_ReplicaSet_NotConfusedWithStatefulSet locks the
// fix for the legacy bug: the original implementation called
// `replace_namespaced_stateful_set` for ReplicaSet, which routes
// updates to the wrong endpoint. We use the right behaviour and pin
// it with a test so a future cross-port doesn't reintroduce it.
func TestReplaceWorkload_ReplicaSet_NotConfusedWithStatefulSet(t *testing.T) {
	rs := seedExisting("apps/v1", "ReplicaSet", "web-rs", "shop", "1")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), rs)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	body := newWorkloadManifest("apps/v1", "ReplicaSet", "web-rs", "shop", nil)
	if _, err := m.ReplaceWorkload(context.Background(), "ReplicaSet", "shop", "web-rs", body); err != nil {
		t.Fatal(err)
	}
	// Verify the replica-sets URL was hit, not statefulsets — the easiest
	// way is to confirm the existing object got updated under the rs GVR.
	rsGVR := supportedWorkloadKinds["ReplicaSet"].gvr
	if _, err := dyn.Resource(rsGVR).Namespace("shop").Get(context.Background(), "web-rs", metav1.GetOptions{}); err != nil {
		t.Errorf("ReplicaSet not present at expected GVR after replace: %v", err)
	}
}

func TestReplaceWorkload_Rollout_Namespaced(t *testing.T) {
	existing := seedExisting("argoproj.io/v1alpha1", "Rollout", "canary", "shop", "7")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	body := newWorkloadManifest("argoproj.io/v1alpha1", "Rollout", "canary", "shop", nil)
	if _, err := m.ReplaceWorkload(context.Background(), "Rollout", "shop", "canary", body); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceWorkload_NodePool_ClusterScoped(t *testing.T) {
	existing := seedExisting("karpenter.sh/v1", "NodePool", "default", "", "1")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	// Cluster-scoped: caller may pass an empty namespace.
	body := newWorkloadManifest("karpenter.sh/v1", "NodePool", "default", "", nil)
	if _, err := m.ReplaceWorkload(context.Background(), "NodePool", "", "default", body); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceWorkload_EC2NodeClass_ClusterScoped(t *testing.T) {
	existing := seedExisting("karpenter.k8s.aws/v1", "EC2NodeClass", "default", "", "1")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	body := newWorkloadManifest("karpenter.k8s.aws/v1", "EC2NodeClass", "default", "", nil)
	if _, err := m.ReplaceWorkload(context.Background(), "EC2NodeClass", "", "default", body); err != nil {
		t.Fatal(err)
	}
}

// ---------- error paths ----------

func TestReplaceWorkload_UnsupportedKind(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	_, err := m.ReplaceWorkload(context.Background(), "Pod", "shop", "web", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("err = %v; want 'not supported' rejection (RESOURCE_NOT_SUPPORTED)", err)
	}
}

func TestReplaceWorkload_NoDynamicClient(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	// SetDynamic deliberately not called — this is the "agent has no
	// CRD-capable client" production path. Should fail fast, not panic.
	_, err := m.ReplaceWorkload(context.Background(), "Deployment", "shop", "web", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "dynamic client") {
		t.Errorf("err = %v; want 'dynamic client not configured'", err)
	}
}

func TestReplaceWorkload_NamespacedRequiresNamespace(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	_, err := m.ReplaceWorkload(context.Background(), "Deployment", "", "web", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "namespace required") {
		t.Errorf("err = %v; want 'namespace required'", err)
	}
}

func TestReplaceWorkload_NotFoundSurfacesAsError(t *testing.T) {
	// 404 should surface as an explicit error (the legacy typed client
	// re-raised ACTION_UNEXPECTED_ERROR). A missing object must not
	// silently "create".
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	body := newWorkloadManifest("apps/v1", "Deployment", "missing", "shop", nil)
	_, err := m.ReplaceWorkload(context.Background(), "Deployment", "shop", "missing", body)
	if err == nil {
		t.Error("replace of missing resource must error (no implicit create)")
	}
}

func TestReplaceWorkload_RejectsEmptyName(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	_, err := m.ReplaceWorkload(context.Background(), "Deployment", "shop", "", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "name required") {
		t.Errorf("err = %v; want 'name required'", err)
	}
}

func TestReplaceWorkload_RejectsNilBody(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	_, err := m.ReplaceWorkload(context.Background(), "Deployment", "shop", "web", nil)
	if err == nil || !strings.Contains(err.Error(), "body required") {
		t.Errorf("err = %v; want 'replace body required'", err)
	}
}

// TestReplaceWorkload_BodyAcceptsJSONBytesAndString covers the
// permissive input shape — callers may forward raw JSON bytes or a
// JSON string instead of marshaling to map[string]any first.
func TestReplaceWorkload_BodyAcceptsJSONBytesAndString(t *testing.T) {
	existing := seedExisting("apps/v1", "Deployment", "web", "shop", "100")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	jsonBody := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"shop"},"spec":{"replicas":3}}`
	if _, err := m.ReplaceWorkload(context.Background(), "Deployment", "shop", "web", []byte(jsonBody)); err != nil {
		t.Errorf("[]byte body: %v", err)
	}

	existing2 := seedExisting("apps/v1", "Deployment", "web2", "shop", "100")
	dyn2 := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing2)
	m2 := New(fake.NewClientset(), "", nil)
	m2.SetDynamic(dyn2)
	jsonBody2 := strings.ReplaceAll(jsonBody, "web", "web2")
	if _, err := m2.ReplaceWorkload(context.Background(), "Deployment", "shop", "web2", jsonBody2); err != nil {
		t.Errorf("string body: %v", err)
	}
}

// ---------- handler shape ----------

func TestHandleReplaceWorkload_PerKindFieldShape(t *testing.T) {
	// ReplaceNamespacedWorkload exposes per-kind manifest fields
	// (deployment, daemonset, …). Forwarding ReplaceNamespacedWorkload.dict()
	// must work unchanged — pickReplaceBody walks `<kind-lower>` first.
	existing := seedExisting("apps/v1", "Deployment", "web", "shop", "100")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	hs := Handlers(m)
	h, ok := hs["replace_workload"]
	if !ok {
		t.Fatal("replace_workload not registered (dynamic client is set)")
	}
	resp, err := h(context.Background(), map[string]any{
		"kind":       "Deployment",
		"name":       "web",
		"namespace":  "shop",
		"deployment": newWorkloadManifest("apps/v1", "Deployment", "web", "shop", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.(map[string]any)
	msg, _ := r["message"].(string)
	if msg != "Deployment/shop/web updated" {
		t.Errorf("message = %q; want 'Deployment/shop/web updated'", msg)
	}
}

func TestHandleReplaceWorkload_BodyFieldShape(t *testing.T) {
	existing := seedExisting("apps/v1", "Deployment", "web", "shop", "100")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	hs := Handlers(m)
	resp, err := hs["replace_workload"](context.Background(), map[string]any{
		"kind":      "Deployment",
		"name":      "web",
		"namespace": "shop",
		"body":      newWorkloadManifest("apps/v1", "Deployment", "web", "shop", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.(map[string]any)["message"] != "Deployment/shop/web updated" {
		t.Errorf("message wrong: %v", resp)
	}
}

func TestHandleReplaceWorkload_MissingKind(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	_, err := Handlers(m)["replace_workload"](context.Background(), map[string]any{
		"name":      "web",
		"namespace": "shop",
	})
	if err == nil || !strings.Contains(err.Error(), "kind required") {
		t.Errorf("err = %v; want 'kind required'", err)
	}
}

func TestHandleReplaceWorkload_MissingBody(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	_, err := Handlers(m)["replace_workload"](context.Background(), map[string]any{
		"kind":      "Deployment",
		"name":      "web",
		"namespace": "shop",
	})
	if err == nil || !strings.Contains(err.Error(), "body required") {
		t.Errorf("err = %v; want 'body required' with hint", err)
	}
	// Confirm the error message hints at the per-kind field name.
	if err != nil && !strings.Contains(err.Error(), `"deployment"`) {
		t.Errorf("err hint missing per-kind field name: %v", err)
	}
}

func TestHandleReplaceWorkload_NotRegisteredWithoutDynamic(t *testing.T) {
	// Without SetDynamic the action MUST NOT register — otherwise calls
	// reach m.ReplaceWorkload and we'd return "dynamic client not
	// configured" with no help to the operator. The auth gate also can't
	// guard a missing handler differently from a misconfigured one.
	m := New(fake.NewClientset(), "", nil)
	if _, ok := Handlers(m)["replace_workload"]; ok {
		t.Error("replace_workload registered without dynamic client — gating broken")
	}
}
