package mutate

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// livePodSpecObject mirrors the shape a real Deployment presents to
// applyRevert: annotations whose keys contain dots, an indexed container
// array, and a nested resources map.
func livePodSpecObject() map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "web", "namespace": "shop"},
		"spec": map[string]any{
			"replicas": int64(3),
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						"rollme":                            "c4cjS",
						"kubectl.kubernetes.io/restartedAt": "2026-08-25T00:00:00Z",
						"checksum/config":                   "newsum",
					},
				},
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name":  "web",
							"image": "registry/app:new",
							"resources": map[string]any{
								"limits": map[string]any{"memory": "2Gi"},
							},
						},
					},
				},
			},
		},
	}
}

func TestApplyRevert_PathSyntaxes(t *testing.T) {
	tests := []struct {
		name  string
		entry RevertEntry
		check func(*testing.T, map[string]any)
	}{
		{
			// Go-agent form: unquoted key, no dots inside it.
			name:  "plain dotted key",
			entry: RevertEntry{Path: "spec.template.metadata.annotations.rollme", Old: "zde7X", HasOld: true},
			check: func(t *testing.T, o map[string]any) {
				if got := annotations(o)["rollme"]; got != "zde7X" {
					t.Errorf("rollme = %v; want zde7X", got)
				}
			},
		},
		{
			// Go-agent form where the KEY itself contains dots — the case a
			// naive strings.Split(path, ".") turns into four bogus segments.
			name: "unquoted key containing dots",
			entry: RevertEntry{
				Path: "spec.template.metadata.annotations.kubectl.kubernetes.io/restartedAt",
				Old:  "2026-07-30T12:25:40Z", HasOld: true,
			},
			check: func(t *testing.T, o map[string]any) {
				if got := annotations(o)["kubectl.kubernetes.io/restartedAt"]; got != "2026-07-30T12:25:40Z" {
					t.Errorf("restartedAt = %v", got)
				}
				if _, split := annotations(o)["kubectl"]; split {
					t.Error("path was split on the dots inside the annotation key")
				}
			},
		},
		{
			// Legacy-agent form: bracket-quoted map key.
			name:  "bracket-quoted key",
			entry: RevertEntry{Path: "spec.template.metadata.annotations['checksum/config']", Old: "oldsum", HasOld: true},
			check: func(t *testing.T, o map[string]any) {
				if got := annotations(o)["checksum/config"]; got != "oldsum" {
					t.Errorf("checksum/config = %v; want oldsum", got)
				}
			},
		},
		{
			// A label/annotation the change DELETED: nothing to longest-match
			// against, so the flat-map rule is what keeps the dotted key whole
			// instead of nesting maps under "app".
			name: "recreates a deleted dotted label key",
			entry: RevertEntry{
				Path: "spec.template.metadata.labels.app.kubernetes.io/version",
				Old:  "0.0.1", HasOld: true,
			},
			check: func(t *testing.T, o map[string]any) {
				labels := o["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["labels"].(map[string]any)
				if labels["app.kubernetes.io/version"] != "0.0.1" {
					t.Errorf("labels = %#v", labels)
				}
			},
		},
		{
			// Extended resource names contain dots too.
			name: "recreates a deleted extended resource limit",
			entry: RevertEntry{
				Path: "spec.template.spec.containers[0].resources.limits.nvidia.com/gpu",
				Old:  "1", HasOld: true,
			},
			check: func(t *testing.T, o map[string]any) {
				limits := container0(o)["resources"].(map[string]any)["limits"].(map[string]any)
				if limits["nvidia.com/gpu"] != "1" {
					t.Errorf("limits = %#v", limits)
				}
			},
		},
		{
			// Legacy-agent form: kind-qualified path.
			name:  "kind-prefixed path",
			entry: RevertEntry{Path: "Deployment.spec.replicas", Old: float64(2), HasOld: true},
			check: func(t *testing.T, o map[string]any) {
				spec := o["spec"].(map[string]any)
				if got := spec["replicas"]; got != int64(2) {
					t.Errorf("replicas = %#v; want int64(2)", got)
				}
			},
		},
		{
			name:  "array index",
			entry: RevertEntry{Path: "spec.template.spec.containers[0].image", Old: "registry/app:old", HasOld: true},
			check: func(t *testing.T, o map[string]any) {
				if got := container0(o)["image"]; got != "registry/app:old" {
					t.Errorf("image = %v", got)
				}
			},
		},
		{
			name: "nested map value",
			entry: RevertEntry{
				Path: "spec.template.spec.containers[0].resources.limits['memory']",
				Old:  "1Gi", HasOld: true,
			},
			check: func(t *testing.T, o map[string]any) {
				limits := container0(o)["resources"].(map[string]any)["limits"].(map[string]any)
				if limits["memory"] != "1Gi" {
					t.Errorf("memory = %v; want 1Gi", limits["memory"])
				}
			},
		},
		{
			// evidence records `old: null` => the change ADDED the field, so
			// reverting removes it rather than writing a null.
			name:  "added field is removed",
			entry: RevertEntry{Path: "spec.template.metadata.annotations.rollme", HasOld: false},
			check: func(t *testing.T, o map[string]any) {
				if _, ok := annotations(o)["rollme"]; ok {
					t.Error("rollme should have been deleted, not set to null")
				}
			},
		},
		{
			// Reverting a field the change DELETED must recreate the branch.
			name: "deleted field is recreated",
			entry: RevertEntry{
				Path: "spec.template.spec.containers[0].resources.requests.cpu",
				Old:  "100m", HasOld: true,
			},
			check: func(t *testing.T, o map[string]any) {
				reqs := container0(o)["resources"].(map[string]any)["requests"].(map[string]any)
				if reqs["cpu"] != "100m" {
					t.Errorf("cpu = %v; want 100m", reqs["cpu"])
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obj := livePodSpecObject()
			if err := applyRevert(obj, tc.entry); err != nil {
				t.Fatalf("applyRevert: %v", err)
			}
			tc.check(t, obj)
		})
	}
}

// Removing a field whose parent branch is already gone is a no-op: the
// desired end state (field absent) already holds.
func TestApplyRevert_RemoveMissingBranchIsNoop(t *testing.T) {
	obj := livePodSpecObject()
	entry := RevertEntry{Path: "spec.strategy.rollingUpdate.maxSurge", HasOld: false}
	if err := applyRevert(obj, entry); err != nil {
		t.Fatalf("applyRevert: %v", err)
	}
	if _, ok := obj["spec"].(map[string]any)["strategy"]; ok {
		t.Error("removing a missing field should not materialise its parents")
	}
}

// An appended array element can't be dropped by index without renumbering
// sibling paths in the same revert, so it must fail loudly.
func TestApplyRevert_AddedArrayElementRefused(t *testing.T) {
	obj := livePodSpecObject()
	entry := RevertEntry{Path: "spec.template.spec.containers[0]", HasOld: false}
	if err := applyRevert(obj, entry); err == nil {
		t.Fatal("expected an error for an added array element")
	}
}

func TestApplyRevert_IndexOutOfRange(t *testing.T) {
	obj := livePodSpecObject()
	entry := RevertEntry{Path: "spec.template.spec.containers[3].image", Old: "x", HasOld: true}
	if err := applyRevert(obj, entry); err == nil {
		t.Fatal("expected an out-of-range error")
	}
}

// Whole numbers arrive from JSON as float64; unstructured objects must hold
// int64 or typed conversions downstream break.
func TestNormalizeJSONValue_IntegralFloats(t *testing.T) {
	got := normalizeJSONValue(map[string]any{
		"replicas": float64(2),
		"ratio":    float64(1.5),
		"nested":   []any{float64(7)},
	})
	want := map[string]any{
		"replicas": int64(2),
		"ratio":    float64(1.5),
		"nested":   []any{int64(7)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v; want %#v", got, want)
	}
}

func TestParseRevertEntries(t *testing.T) {
	entries, err := parseRevertEntries([]any{
		map[string]any{"path": "spec.replicas", "old": float64(2)},
		map[string]any{"path": "spec.template.metadata.annotations.rollme", "old": nil},
		map[string]any{"path": "spec.paused"}, // `old` absent entirely
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries; want 3", len(entries))
	}
	if !entries[0].HasOld || entries[0].Old != float64(2) {
		t.Errorf("entry 0 = %#v", entries[0])
	}
	for _, i := range []int{1, 2} {
		if entries[i].HasOld {
			t.Errorf("entry %d: null/absent `old` must mean remove-the-field", i)
		}
	}
}

func TestParseRevertEntries_Rejects(t *testing.T) {
	for _, bad := range []any{nil, []any{}, "notalist", []any{map[string]any{"old": 1}}} {
		if _, err := parseRevertEntries(bad); err == nil {
			t.Errorf("parseRevertEntries(%#v) should have failed", bad)
		}
	}
}

// ---------- end to end through the handler ----------

func TestHandleRevertWorkload_Deployment(t *testing.T) {
	existing := &unstructured.Unstructured{Object: livePodSpecObject()}
	existing.SetResourceVersion("100")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	resp, err := Handlers(m)["revert_workload"](context.Background(), map[string]any{
		"kind":      "Deployment",
		"name":      "web",
		"namespace": "shop",
		"revert_paths": []any{
			map[string]any{"path": "spec.template.metadata.annotations.rollme", "old": "zde7X"},
			map[string]any{"path": "spec.template.spec.containers[0].image", "old": "registry/app:old"},
		},
	})
	if err != nil {
		t.Fatalf("revert_workload: %v", err)
	}
	r := resp.(map[string]any)
	if r["message"] != "Deployment/shop/web reverted (2 fields)" {
		t.Errorf("message = %v", r["message"])
	}
	updated := r["updated"].(map[string]any)
	if got := annotations(updated)["rollme"]; got != "zde7X" {
		t.Errorf("rollme = %v; want zde7X", got)
	}
	if got := container0(updated)["image"]; got != "registry/app:old" {
		t.Errorf("image = %v; want registry/app:old", got)
	}
	// Untouched fields must survive — the whole point of patching the live
	// object instead of replaying a stored snapshot.
	if got := updated["spec"].(map[string]any)["replicas"]; got != int64(3) {
		t.Errorf("replicas = %v; revert clobbered an unrelated field", got)
	}
}

// The kind arrives from cloud_resourses.resourse_id, whose casing is not
// consistent (140k rows carry a lowercase "pod"), so resolution must be
// case-insensitive like delete_workload's.
func TestHandleRevertWorkload_KindCaseInsensitive(t *testing.T) {
	existing := &unstructured.Unstructured{Object: livePodSpecObject()}
	existing.SetResourceVersion("100")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	_, err := Handlers(m)["revert_workload"](context.Background(), map[string]any{
		"kind":         "deployment",
		"name":         "web",
		"namespace":    "shop",
		"revert_paths": []any{map[string]any{"path": "spec.replicas", "old": float64(2)}},
	})
	if err != nil {
		t.Fatalf("lowercase kind rejected: %v", err)
	}
}

// The api-server sends an event revert as replace_workload carrying BOTH
// revert_paths and the legacy snake_case manifest, so tenants still on the
// Python agent keep working. This agent must take the paths and never replay
// that manifest — replaying it is the bug this action exists to fix.
func TestHandleReplaceWorkload_RevertPathsWinOverLegacyManifest(t *testing.T) {
	existing := &unstructured.Unstructured{Object: livePodSpecObject()}
	existing.SetResourceVersion("100")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	resp, err := Handlers(m)["replace_workload"](context.Background(), map[string]any{
		"kind":      "Deployment",
		"name":      "web",
		"namespace": "shop",
		"revert_paths": []any{
			map[string]any{"path": "spec.template.spec.containers[0].image", "old": "registry/app:old"},
		},
		// What the Python agent reads: snake_case, and missing the fields the
		// apiserver requires once the unknown keys are dropped.
		"deployment": map[string]any{
			"spec": map[string]any{
				"replicas": int64(9),
				"selector": map[string]any{"match_labels": map[string]any{"app": "web"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("replace_workload with revert_paths: %v", err)
	}
	updated := resp.(map[string]any)["updated"].(map[string]any)
	if got := container0(updated)["image"]; got != "registry/app:old" {
		t.Errorf("image = %v; want registry/app:old", got)
	}
	if got := updated["spec"].(map[string]any)["replicas"]; got != int64(3) {
		t.Errorf("replicas = %v; the legacy manifest was replayed", got)
	}
	sel, _ := updated["spec"].(map[string]any)["selector"].(map[string]any)
	if _, snake := sel["match_labels"]; snake {
		t.Error("snake_case selector from the legacy manifest reached the object")
	}
}

// Malformed revert_paths must fail loudly rather than fall through to the
// stale manifest sitting in the same payload.
func TestHandleReplaceWorkload_MalformedRevertPathsDoesNotFallBack(t *testing.T) {
	existing := &unstructured.Unstructured{Object: livePodSpecObject()}
	existing.SetResourceVersion("100")
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	_, err := Handlers(m)["replace_workload"](context.Background(), map[string]any{
		"kind": "Deployment", "name": "web", "namespace": "shop",
		"revert_paths": []any{map[string]any{"old": "orphan"}},
		"deployment":   map[string]any{"spec": map[string]any{"replicas": int64(9)}},
	})
	if err == nil {
		t.Fatal("expected an error, not a fall-through to the legacy manifest")
	}
}

func TestHandleRevertWorkload_Rejects(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(workloadScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	h := Handlers(m)["revert_workload"]

	if _, err := h(context.Background(), map[string]any{"name": "web", "namespace": "shop"}); err == nil {
		t.Error("missing kind should fail")
	}
	if _, err := h(context.Background(), map[string]any{"kind": "Deployment", "name": "web", "namespace": "shop"}); err == nil {
		t.Error("missing revert_paths should fail")
	}
	if _, err := h(context.Background(), map[string]any{
		"kind": "Service", "name": "web", "namespace": "shop",
		"revert_paths": []any{map[string]any{"path": "spec.replicas", "old": float64(1)}},
	}); err == nil {
		t.Error("unsupported kind should fail")
	}
}

func TestRevertWorkload_NoDynamicClient(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	if _, ok := Handlers(m)["revert_workload"]; ok {
		t.Error("revert_workload registered without a dynamic client — gating broken")
	}
}

func annotations(o map[string]any) map[string]any {
	return o["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)
}

func container0(o map[string]any) map[string]any {
	return o["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
}
