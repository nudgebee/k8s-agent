package mutate

import (
	"context"
	"errors"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// customerCR is a PrometheusRule the customer owns, outside the install
// namespace, with two groups and labels/annotations that must survive edits.
func customerCR() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "monitoring.coreos.com/v1",
		"kind":       "PrometheusRule",
		"metadata":   map[string]any{"name": "payments-rules", "namespace": "monitoring"},
		"spec": map[string]any{
			"groups": []any{
				map[string]any{"name": "payments", "rules": []any{
					map[string]any{
						"alert": "HighErrorRate", "expr": "rate(errors[5m]) > 0.05", "for": "5m",
						"labels":      map[string]any{"severity": "critical", "team": "payments"},
						"annotations": map[string]any{"runbook_url": "https://rb"},
					},
					map[string]any{"alert": "LatencyHigh", "expr": "p99 > 1", "for": "10m"},
				}},
				map[string]any{"name": "payments-slo", "rules": []any{
					map[string]any{"alert": "Burn", "expr": "burn > 14", "for": "2m"},
				}},
			},
		},
	}}
}

func locatorMutator(t *testing.T, objs ...*unstructured.Unstructured) (*Mutator, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClient(promRuleScheme())
	for _, o := range objs {
		if _, err := dyn.Resource(prometheusRuleGVR).Namespace(o.GetNamespace()).Create(context.Background(), o, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	m := New(fake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	m.SetNamespace("nudgebee-agent")
	return m, dyn
}

func crGroups(t *testing.T, dyn *dynamicfake.FakeDynamicClient, ns, name string) []any {
	t.Helper()
	got, err := dyn.Resource(prometheusRuleGVR).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read CR %s/%s: %v", ns, name, err)
	}
	groups, _, _ := unstructured.NestedSlice(got.Object, "spec", "groups")
	return groups
}

func groupRules(groups []any, name string) []any {
	for _, raw := range groups {
		g := raw.(map[string]any)
		if g["name"] == name {
			rules, _ := g["rules"].([]any)
			return rules
		}
	}
	return nil
}

var paymentsLoc = AlertRuleLocator{Namespace: "monitoring", Name: "payments-rules", Group: "payments", Alert: "HighErrorRate"}

func TestPatchAlertRuleInCR_ChangesOnlyExprAndFor(t *testing.T) {
	m, dyn := locatorMutator(t, customerCR())
	_, err := m.PatchAlertRuleInCR(context.Background(), paymentsLoc, LegacyAlertRuleParams{
		Alert: "HighErrorRate", Expr: "rate(errors[5m]) > 0.10", Duration: "15m",
		// The edit form sends severity only; it must not replace the CR's labels.
		Labels: map[string]any{"severity": "warning"},
	})
	if err != nil {
		t.Fatal(err)
	}
	groups := crGroups(t, dyn, "monitoring", "payments-rules")
	rules := groupRules(groups, "payments")
	if len(rules) != 2 {
		t.Fatalf("want 2 rules in group, got %d", len(rules))
	}
	r := rules[0].(map[string]any)
	if r["expr"] != "rate(errors[5m]) > 0.10" || r["for"] != "15m" {
		t.Errorf("expr/for not patched: %v", r)
	}
	wantLabels := map[string]any{"severity": "critical", "team": "payments"}
	if !reflect.DeepEqual(r["labels"], wantLabels) {
		t.Errorf("labels changed: %v", r["labels"])
	}
	if !reflect.DeepEqual(r["annotations"], map[string]any{"runbook_url": "https://rb"}) {
		t.Errorf("annotations changed: %v", r["annotations"])
	}
	if got := groupRules(groups, "payments-slo"); len(got) != 1 {
		t.Errorf("other group touched: %v", got)
	}
	// No copy is written into the canonical CR.
	if _, err := dyn.Resource(prometheusRuleGVR).Namespace("nudgebee-agent").Get(
		context.Background(), LegacyAlertRuleCRDName, metav1.GetOptions{}); err == nil {
		t.Error("canonical CR must not be created by a located edit")
	}
}

func TestDeleteThenPatch_RemovesAndRestoresOneRule(t *testing.T) {
	m, dyn := locatorMutator(t, customerCR())
	if err := m.DeleteAlertRuleInCR(context.Background(), paymentsLoc); err != nil {
		t.Fatal(err)
	}
	groups := crGroups(t, dyn, "monitoring", "payments-rules")
	rules := groupRules(groups, "payments")
	if len(rules) != 1 || rules[0].(map[string]any)["alert"] != "LatencyHigh" {
		t.Fatalf("only HighErrorRate should be gone: %v", rules)
	}
	if len(groupRules(groups, "payments-slo")) != 1 {
		t.Error("other group touched")
	}

	// Deleting it again reports not found instead of pretending success.
	if err := m.DeleteAlertRuleInCR(context.Background(), paymentsLoc); !errors.Is(err, ErrAlertRuleNotFound) {
		t.Errorf("want ErrAlertRuleNotFound, got %v", err)
	}

	// Enable re-adds the stored definition to its group.
	_, err := m.PatchAlertRuleInCR(context.Background(), paymentsLoc, LegacyAlertRuleParams{
		Alert: "HighErrorRate", Expr: "rate(errors[5m]) > 0.05", Duration: "5m",
		Labels: map[string]any{"severity": "critical", "team": "payments"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rules = groupRules(crGroups(t, dyn, "monitoring", "payments-rules"), "payments")
	if len(rules) != 2 {
		t.Fatalf("rule not re-added: %v", rules)
	}
	readded := rules[1].(map[string]any)
	if readded["alert"] != "HighErrorRate" || readded["for"] != "5m" {
		t.Errorf("re-added rule wrong: %v", readded)
	}
}

func TestPatchAlertRuleInCR_AddsMissingGroup(t *testing.T) {
	m, dyn := locatorMutator(t, customerCR())
	loc := AlertRuleLocator{Namespace: "monitoring", Name: "payments-rules", Group: "new-group", Alert: "Fresh"}
	if _, err := m.PatchAlertRuleInCR(context.Background(), loc, LegacyAlertRuleParams{Alert: "Fresh", Expr: "up == 0"}); err != nil {
		t.Fatal(err)
	}
	if got := groupRules(crGroups(t, dyn, "monitoring", "payments-rules"), "new-group"); len(got) != 1 {
		t.Errorf("group not created: %v", got)
	}
}

func TestPatchAlertRuleInCR_MissingRuleWithoutGroup(t *testing.T) {
	m, _ := locatorMutator(t, customerCR())
	loc := AlertRuleLocator{Namespace: "monitoring", Name: "payments-rules", Alert: "Nope"}
	_, err := m.PatchAlertRuleInCR(context.Background(), loc, LegacyAlertRuleParams{Alert: "Nope", Expr: "up"})
	if !errors.Is(err, ErrAlertRuleNotFound) {
		t.Errorf("want ErrAlertRuleNotFound, got %v", err)
	}
}

func TestLocatedWrites_AmbiguousWithoutGroup(t *testing.T) {
	cr := customerCR()
	groups, _, _ := unstructured.NestedSlice(cr.Object, "spec", "groups")
	slo := groups[1].(map[string]any)
	slo["rules"] = append(slo["rules"].([]any), map[string]any{"alert": "HighErrorRate", "expr": "slo_errors > 0"})
	_ = unstructured.SetNestedSlice(cr.Object, groups, "spec", "groups")
	m, dyn := locatorMutator(t, cr)

	noGroup := AlertRuleLocator{Namespace: "monitoring", Name: "payments-rules", Alert: "HighErrorRate"}
	if err := m.DeleteAlertRuleInCR(context.Background(), noGroup); !errors.Is(err, ErrAlertRuleAmbiguous) {
		t.Errorf("delete: want ErrAlertRuleAmbiguous, got %v", err)
	}
	if _, err := m.PatchAlertRuleInCR(context.Background(), noGroup, LegacyAlertRuleParams{Alert: "HighErrorRate", Expr: "x"}); !errors.Is(err, ErrAlertRuleAmbiguous) {
		t.Errorf("patch: want ErrAlertRuleAmbiguous, got %v", err)
	}

	// With the group it is unambiguous and only that copy changes.
	sloLoc := noGroup
	sloLoc.Group = "payments-slo"
	if err := m.DeleteAlertRuleInCR(context.Background(), sloLoc); err != nil {
		t.Fatal(err)
	}
	after := crGroups(t, dyn, "monitoring", "payments-rules")
	if len(groupRules(after, "payments-slo")) != 1 || len(groupRules(after, "payments")) != 2 {
		t.Errorf("wrong rule removed: %v", after)
	}
}

func TestLocatedWrites_Validation(t *testing.T) {
	m, _ := locatorMutator(t, customerCR())
	if err := m.DeleteAlertRuleInCR(context.Background(), AlertRuleLocator{Name: "payments-rules"}); err == nil {
		t.Error("missing alert must fail")
	}
	if err := m.DeleteAlertRuleInCR(context.Background(), AlertRuleLocator{Namespace: "monitoring", Name: "absent", Alert: "X"}); err == nil {
		t.Error("missing CR must fail")
	}
	if _, err := m.PatchAlertRuleInCR(context.Background(), paymentsLoc, LegacyAlertRuleParams{Alert: "HighErrorRate"}); err == nil {
		t.Error("missing expr must fail")
	}
}

func TestHandlers_RouteLocatedPayloads(t *testing.T) {
	m, dyn := locatorMutator(t, customerCR())
	ctx := context.Background()

	if _, err := handleCreateOrReplacePromRule(ctx, m, map[string]any{
		"alert": "LatencyHigh", "expr": "p99 > 2", "duration": "10m",
		"namespace": "monitoring", "name": "payments-rules", "group": "payments",
	}); err != nil {
		t.Fatal(err)
	}
	rules := groupRules(crGroups(t, dyn, "monitoring", "payments-rules"), "payments")
	if rules[1].(map[string]any)["expr"] != "p99 > 2" {
		t.Errorf("located create_or_replace did not patch: %v", rules[1])
	}

	if err := handleDeletePromRule(ctx, m, map[string]any{
		"alert": "LatencyHigh", "namespace": "monitoring", "name": "payments-rules", "group": "payments",
	}); err != nil {
		t.Fatal(err)
	}
	if got := groupRules(crGroups(t, dyn, "monitoring", "payments-rules"), "payments"); len(got) != 1 {
		t.Errorf("located delete did not remove one rule: %v", got)
	}

	// api-server today sends namespace/group but no CR name: still the legacy path.
	if err := handleDeletePromRule(ctx, m, map[string]any{"alert": "Burn", "namespace": "monitoring", "group": "payments-slo"}); err != nil {
		t.Fatal(err)
	}
	if got := groupRules(crGroups(t, dyn, "monitoring", "payments-rules"), "payments-slo"); len(got) != 1 {
		t.Errorf("legacy-shape delete must not touch customer CRs: %v", got)
	}

	// No alert: refused, CR intact.
	if err := handleDeletePromRule(ctx, m, map[string]any{"namespace": "monitoring", "name": "payments-rules", "group": "payments"}); err == nil {
		t.Error("delete without alert must be refused")
	}
	if len(crGroups(t, dyn, "monitoring", "payments-rules")) != 2 {
		t.Error("CR must be intact")
	}
}

// A located edit changes only what the request carries: an omitted duration
// keeps the rule's `for` (dropping it would make the alert fire on the first
// breach), and an explicit "0s" is how a caller removes the wait.
func TestPatchAlertRuleInCR_DurationAbsentKeepsFor(t *testing.T) {
	m, dyn := locatorMutator(t, customerCR())
	if _, err := m.PatchAlertRuleInCR(context.Background(), paymentsLoc, LegacyAlertRuleParams{Alert: "HighErrorRate", Expr: "x > 1"}); err != nil {
		t.Fatal(err)
	}
	r := groupRules(crGroups(t, dyn, "monitoring", "payments-rules"), "payments")[0].(map[string]any)
	if r["for"] != "5m" {
		t.Errorf("for must be kept when no duration is sent, got %v", r["for"])
	}

	if _, err := m.PatchAlertRuleInCR(context.Background(), paymentsLoc, LegacyAlertRuleParams{Alert: "HighErrorRate", Expr: "x > 1", Duration: "0s"}); err != nil {
		t.Fatal(err)
	}
	r = groupRules(crGroups(t, dyn, "monitoring", "payments-rules"), "payments")[0].(map[string]any)
	if r["for"] != "0s" {
		t.Errorf("explicit 0s must be written, got %v", r["for"])
	}
}
