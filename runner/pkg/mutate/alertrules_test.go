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

// Legacy alert-rule path: the api-server today sends {alert, expr, duration,
// annotations, labels} — not a manifest. These tests pin the shape-router and
// the canonical-CR semantics so the path doesn't silently regress to "DB row
// saved but no CR" the way it did before this change.

func newLegacyCR(namespace string, rules []map[string]any) *unstructured.Unstructured {
	asAny := make([]any, len(rules))
	for i, r := range rules {
		asAny[i] = r
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "monitoring.coreos.com/v1",
		"kind":       "PrometheusRule",
		"metadata": map[string]any{
			"name":      LegacyAlertRuleCRDName,
			"namespace": namespace,
			"labels": map[string]any{
				LegacyAlertRuleLabelKey: LegacyAlertRuleLabelValue,
				"role":                  "alert-rules",
			},
		},
		"spec": map[string]any{
			"groups": []any{
				map[string]any{"name": LegacyAlertRuleGroupName, "rules": asAny},
			},
		},
	}}
}

func legacyRulesFromCR(t *testing.T, dyn *dynamicfake.FakeDynamicClient, namespace string) []any {
	t.Helper()
	got, err := dyn.Resource(prometheusRuleGVR).Namespace(namespace).Get(
		context.Background(), LegacyAlertRuleCRDName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read CR: %v", err)
	}
	groups, _, _ := unstructured.NestedSlice(got.Object, "spec", "groups")
	if len(groups) == 0 {
		return nil
	}
	g, _ := groups[0].(map[string]any)
	rules, _ := g["rules"].([]any)
	return rules
}

func TestCreateOrReplaceAlertRule_CreatesCRWhenAbsent(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	m.SetNamespace("nudgebee-agent")

	if _, err := m.CreateOrReplaceAlertRule(context.Background(), LegacyAlertRuleParams{
		Alert:       "TestAlertA",
		Expr:        "up == 0",
		Duration:    "1m",
		Annotations: map[string]any{"summary": "s"},
		Labels:      map[string]any{"severity": "warning"},
	}); err != nil {
		t.Fatal(err)
	}
	rules := legacyRulesFromCR(t, dyn, "nudgebee-agent")
	if len(rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(rules))
	}
	r, _ := rules[0].(map[string]any)
	if r["alert"] != "TestAlertA" || r["for"] != "1m" {
		t.Errorf("bad rule shape: %v", r)
	}
}

func TestCreateOrReplaceAlertRule_AppendsWhenAlertNew(t *testing.T) {
	pre := newLegacyCR("nudgebee-agent", []map[string]any{
		{"alert": "Existing", "expr": "up", "for": "5m"},
	})
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme(), pre)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	m.SetNamespace("nudgebee-agent")

	if _, err := m.CreateOrReplaceAlertRule(context.Background(), LegacyAlertRuleParams{
		Alert: "Fresh", Expr: "up == 0", Duration: "2m",
	}); err != nil {
		t.Fatal(err)
	}
	rules := legacyRulesFromCR(t, dyn, "nudgebee-agent")
	if len(rules) != 2 {
		t.Fatalf("want 2 rules after append, got %d", len(rules))
	}
}

func TestCreateOrReplaceAlertRule_ReplacesExistingAlertInPlace(t *testing.T) {
	pre := newLegacyCR("nudgebee-agent", []map[string]any{
		{"alert": "TargetAlert", "expr": "old_expr", "for": "5m"},
		{"alert": "Other", "expr": "up", "for": "1m"},
	})
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme(), pre)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	m.SetNamespace("nudgebee-agent")

	if _, err := m.CreateOrReplaceAlertRule(context.Background(), LegacyAlertRuleParams{
		Alert: "TargetAlert", Expr: "new_expr", Duration: "30s",
	}); err != nil {
		t.Fatal(err)
	}
	rules := legacyRulesFromCR(t, dyn, "nudgebee-agent")
	if len(rules) != 2 {
		t.Fatalf("expected 2 rules (in-place replace), got %d", len(rules))
	}
	r0, _ := rules[0].(map[string]any)
	if r0["expr"] != "new_expr" || r0["for"] != "30s" {
		t.Errorf("expected first rule replaced, got %v", r0)
	}
	r1, _ := rules[1].(map[string]any)
	if r1["alert"] != "Other" {
		t.Errorf("second rule should be untouched, got %v", r1)
	}
}

func TestCreateOrReplaceAlertRule_RequiresNamespace(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	// Namespace deliberately not set.
	if _, err := m.CreateOrReplaceAlertRule(context.Background(), LegacyAlertRuleParams{
		Alert: "X", Expr: "up",
	}); err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Errorf("expected namespace error, got %v", err)
	}
}

func TestCreateOrReplaceAlertRule_RequiresAlertAndExpr(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	m.SetNamespace("nudgebee-agent")
	if _, err := m.CreateOrReplaceAlertRule(context.Background(), LegacyAlertRuleParams{}); err == nil {
		t.Error("expected error when alert+expr empty")
	}
}

func TestDeleteAlertRule_RemovesByAlertName(t *testing.T) {
	pre := newLegacyCR("nudgebee-agent", []map[string]any{
		{"alert": "Keep", "expr": "up"},
		{"alert": "Drop", "expr": "down"},
	})
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme(), pre)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	m.SetNamespace("nudgebee-agent")

	if err := m.DeleteAlertRule(context.Background(), "Drop"); err != nil {
		t.Fatal(err)
	}
	rules := legacyRulesFromCR(t, dyn, "nudgebee-agent")
	if len(rules) != 1 {
		t.Fatalf("want 1 rule after delete, got %d", len(rules))
	}
	r, _ := rules[0].(map[string]any)
	if r["alert"] != "Keep" {
		t.Errorf("wrong rule retained: %v", r)
	}
}

func TestDeleteAlertRule_NoCRIsNoop(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	m.SetNamespace("nudgebee-agent")
	if err := m.DeleteAlertRule(context.Background(), "anything"); err != nil {
		t.Errorf("delete on missing CR should be no-op, got %v", err)
	}
}

func TestDeleteAlertRule_UnknownAlertIsNoop(t *testing.T) {
	pre := newLegacyCR("nudgebee-agent", []map[string]any{
		{"alert": "Keep", "expr": "up"},
	})
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme(), pre)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	m.SetNamespace("nudgebee-agent")
	if err := m.DeleteAlertRule(context.Background(), "DoesNotExist"); err != nil {
		t.Errorf("delete of unknown alert should be no-op, got %v", err)
	}
	if got := legacyRulesFromCR(t, dyn, "nudgebee-agent"); len(got) != 1 {
		t.Errorf("expected CR untouched, got %d rules", len(got))
	}
}

// Shape-router: the handler must pick the legacy path for {alert, expr, ...}
// and the manifest path for {apiVersion, kind, metadata, spec}.

func TestHandleCreateOrReplacePromRule_RoutesLegacyShape(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	m.SetNamespace("nudgebee-agent")

	_, err := handleCreateOrReplacePromRule(context.Background(), m, map[string]any{
		"alert":       "RouterTest",
		"expr":        "up",
		"duration":    "1m",
		"annotations": map[string]any{"summary": "s"},
		"labels":      map[string]any{"severity": "warning"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rules := legacyRulesFromCR(t, dyn, "nudgebee-agent")
	if len(rules) != 1 {
		t.Fatalf("legacy path should have created 1 rule, got %d", len(rules))
	}
}

func TestHandleCreateOrReplacePromRule_RoutesManifestShape(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme())
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	m.SetNamespace("nudgebee-agent") // set but should be ignored on the manifest path

	if _, err := handleCreateOrReplacePromRule(context.Background(), m,
		newPromRuleManifest("manifest-route", "monitoring")); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(prometheusRuleGVR).Namespace("monitoring").Get(
		context.Background(), "manifest-route", metav1.GetOptions{}); err != nil {
		t.Errorf("manifest-route CR should exist: %v", err)
	}
}

func TestHandleDeletePromRule_RoutesByAlertVsName(t *testing.T) {
	pre := newLegacyCR("nudgebee-agent", []map[string]any{
		{"alert": "ToDrop", "expr": "up"},
	})
	// Also seed a manifest-style CR for the by-name path.
	manifest := &unstructured.Unstructured{}
	manifest.SetGroupVersionKind(schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule"})
	manifest.SetName("standalone")
	manifest.SetNamespace("monitoring")
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme(), pre, manifest)
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	m.SetNamespace("nudgebee-agent")

	// Alert-shape removes a rule from the canonical CR.
	if err := handleDeletePromRule(context.Background(), m, map[string]any{"alert": "ToDrop"}); err != nil {
		t.Fatalf("alert-shape: %v", err)
	}
	if got := legacyRulesFromCR(t, dyn, "nudgebee-agent"); len(got) != 0 {
		t.Errorf("expected ToDrop removed, %d rules remain", len(got))
	}

	// Name+namespace removes the standalone CR.
	if err := handleDeletePromRule(context.Background(), m, map[string]any{
		"name": "standalone", "namespace": "monitoring",
	}); err != nil {
		t.Fatalf("manifest-shape: %v", err)
	}
}
