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

// promRuleScheme registers PrometheusRule so the dynamic fake recognises the GVR.
func promRuleScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	gvr := prometheusRuleGVR
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: "PrometheusRule"},
		&unstructured.Unstructured{},
	)
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: "PrometheusRuleList"},
		&unstructured.UnstructuredList{},
	)
	return s
}

func newPromRuleManifest(name, namespace string) map[string]any {
	return map[string]any{
		"apiVersion": "monitoring.coreos.com/v1",
		"kind":       "PrometheusRule",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"groups": []any{
				map[string]any{
					"name": "g1",
					"rules": []any{
						map[string]any{"alert": "X", "expr": "up == 0", "for": "1m"},
					},
				},
			},
		},
	}
}

func TestCreateOrReplacePromRule_Create(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	got, err := m.CreateOrReplacePrometheusRule(context.Background(), newPromRuleManifest("r1", "monitoring"))
	if err != nil {
		t.Fatal(err)
	}
	gotMap, _ := got.(map[string]any)
	if meta, _ := gotMap["metadata"].(map[string]any); meta["name"] != "r1" {
		t.Errorf("name = %v", meta["name"])
	}
	// Verify it exists in the fake.
	if _, err := dyn.Resource(prometheusRuleGVR).Namespace("monitoring").Get(context.Background(), "r1", metav1.GetOptions{}); err != nil {
		t.Errorf("rule not present after create: %v", err)
	}
}

func TestCreateOrReplacePromRule_Update(t *testing.T) {
	// Pre-seed an existing rule.
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule"})
	existing.SetName("r1")
	existing.SetNamespace("monitoring")
	existing.SetResourceVersion("100")
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme(), existing)

	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	updated := newPromRuleManifest("r1", "monitoring")
	updated["spec"].(map[string]any)["new_field"] = "yes"
	got, err := m.CreateOrReplacePrometheusRule(context.Background(), updated)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected updated content")
	}
}

func TestCreateOrReplacePromRule_RequiresNameAndNamespace(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	_, err := m.CreateOrReplacePrometheusRule(context.Background(), map[string]any{
		"apiVersion": "monitoring.coreos.com/v1",
		"kind":       "PrometheusRule",
		"spec":       map[string]any{},
	})
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("expected missing-name error, got %v", err)
	}
}

func TestCreateOrReplacePromRule_NoDynamic(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	if _, err := m.CreateOrReplacePrometheusRule(context.Background(), newPromRuleManifest("r", "n")); err == nil {
		t.Error("expected error when dynamic not configured")
	}
}

func TestDeletePromRule_RemovesAndIdempotent(t *testing.T) {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule"})
	existing.SetName("r1")
	existing.SetNamespace("n")
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme(), existing)

	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	if err := m.DeletePrometheusRule(context.Background(), "n", "r1"); err != nil {
		t.Fatal(err)
	}
	// Second delete should also succeed (NotFound treated as success).
	if err := m.DeletePrometheusRule(context.Background(), "n", "r1"); err != nil {
		t.Errorf("delete should be idempotent: %v", err)
	}
}

func TestDeletePromRule_Validates(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dynamicfake.NewSimpleDynamicClient(promRuleScheme()))
	if err := m.DeletePrometheusRule(context.Background(), "", "r"); err == nil {
		t.Error("missing namespace should error")
	}
	if err := m.DeletePrometheusRule(context.Background(), "n", ""); err == nil {
		t.Error("missing name should error")
	}
	m2 := New(fake.NewClientset(), "", nil) // no dynamic
	if err := m2.DeletePrometheusRule(context.Background(), "n", "r"); err == nil {
		t.Error("missing dynamic should error")
	}
}

func TestHandlers_PromRuleActionsRegisteredWhenDynamicSet(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	if _, ok := Handlers(m)["create_or_replace_alert_rule"]; ok {
		t.Error("should NOT register create_or_replace_alert_rule without dynamic")
	}
	m.SetDynamic(dynamicfake.NewSimpleDynamicClient(promRuleScheme()))
	hs := Handlers(m)
	for _, want := range []string{"create_or_replace_alert_rule", "delete_alert_rule"} {
		if _, ok := hs[want]; !ok {
			t.Errorf("missing %s after SetDynamic", want)
		}
	}
}
