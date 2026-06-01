package mutate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// PrometheusRule CRD GVR. Standard prometheus-operator group.
var prometheusRuleGVR = schema.GroupVersionResource{
	Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheusrules",
}

// SetDynamic wires a dynamic client. Required for the alert-rule CRUD path
// (PrometheusRule CRDs aren't typed in client-go).
func (m *Mutator) SetDynamic(d dynamic.Interface) { m.dynamic = d }

// CreateOrReplacePrometheusRule applies a PrometheusRule object. If a rule
// with the same name+namespace exists, it's overwritten (with ResourceVersion
// preservation so the apiserver doesn't reject the update).
//
// rule is the raw PrometheusRule manifest (any). It MUST contain at least
// metadata.name + metadata.namespace + spec.
func (m *Mutator) CreateOrReplacePrometheusRule(ctx context.Context, rule any) (any, error) {
	if m.dynamic == nil {
		return nil, errors.New("mutate: dynamic client not configured")
	}
	body, err := json.Marshal(rule)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal(body, &u.Object); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if u.GetName() == "" || u.GetNamespace() == "" {
		return nil, errors.New("mutate: rule.metadata.name and namespace are required")
	}
	// Force the kind/apiVersion so the apiserver knows what we're creating.
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule",
	})

	ri := m.dynamic.Resource(prometheusRuleGVR).Namespace(u.GetNamespace())
	existing, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		created, err := ri.Create(ctx, u, metav1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		return created.UnstructuredContent(), nil
	}
	if err != nil {
		return nil, err
	}
	// Replace: copy ResourceVersion so the apiserver accepts the update.
	u.SetResourceVersion(existing.GetResourceVersion())
	updated, err := ri.Update(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	return updated.UnstructuredContent(), nil
}

// DeletePrometheusRule removes one PrometheusRule by namespace+name. Idempotent
// (NotFound is treated as success).
func (m *Mutator) DeletePrometheusRule(ctx context.Context, namespace, name string) error {
	if m.dynamic == nil {
		return errors.New("mutate: dynamic client not configured")
	}
	if namespace == "" || name == "" {
		return errors.New("mutate: namespace and name required")
	}
	err := m.dynamic.Resource(prometheusRuleGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// Legacy alert-rule path: the api-server's eventrule code (and the older
// Robusta playbook) sends `create_or_replace_alert_rule` with a flat
// {alert, expr, duration, annotations, labels} payload — NOT a full
// PrometheusRule manifest. We mutate a single shared CR in the agent's
// install namespace so existing installations (which already carry this CR
// from the legacy runner) keep working without a manifest-shape migration
// in api-server.
const (
	// LegacyAlertRuleCRDName is the canonical CR the legacy runner created.
	// We reuse the exact name so an existing installation's rules survive
	// the migration; a fresh installation gets a CR with this name too.
	LegacyAlertRuleCRDName = "nudgebee-prometheus.rules"

	// LegacyAlertRuleGroupName is the group inside the CR the legacy runner
	// appended into. New rules added by the legacy path go here.
	LegacyAlertRuleGroupName = "kubernetes-apps"

	// LegacyAlertRuleLabelKey/Value matches the label_selector the Robusta
	// playbook used to identify the canonical CR — preserved on freshly
	// created CRs so a parallel legacy runner can still find them.
	LegacyAlertRuleLabelKey   = "release.app"
	LegacyAlertRuleLabelValue = "nudgebee-resource-management"
)

// LegacyAlertRuleParams is the wire shape `create_or_replace_alert_rule`
// arrives with. Duration is translated to the CR's `for` field.
type LegacyAlertRuleParams struct {
	Alert       string
	Expr        string
	Duration    string
	Annotations map[string]any
	Labels      map[string]any
}

// CreateOrReplaceAlertRule applies a single alert rule to the canonical
// PrometheusRule CR. If a rule with the same `alert` name already exists in
// any group, it is replaced in place; otherwise the rule is appended to the
// first group. If the CR doesn't exist yet, it is created with the rule
// inside a single group named LegacyAlertRuleGroupName.
func (m *Mutator) CreateOrReplaceAlertRule(ctx context.Context, p LegacyAlertRuleParams) (any, error) {
	if m.dynamic == nil {
		return nil, errors.New("mutate: dynamic client not configured")
	}
	if m.Namespace == "" {
		return nil, errors.New("mutate: agent namespace not configured (set INSTALLATION_NAMESPACE)")
	}
	if p.Alert == "" || p.Expr == "" {
		return nil, errors.New("mutate: alert and expr required")
	}

	rule := map[string]any{
		"alert":       p.Alert,
		"expr":        p.Expr,
		"annotations": p.Annotations,
		"labels":      p.Labels,
	}
	if p.Duration != "" {
		rule["for"] = p.Duration
	}

	ri := m.dynamic.Resource(prometheusRuleGVR).Namespace(m.Namespace)
	existing, err := ri.Get(ctx, LegacyAlertRuleCRDName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		u := newLegacyAlertRuleCR(m.Namespace, []any{rule})
		created, cerr := ri.Create(ctx, u, metav1.CreateOptions{})
		if cerr != nil {
			return nil, cerr
		}
		return created.UnstructuredContent(), nil
	}
	if err != nil {
		return nil, err
	}

	groups, _, _ := unstructured.NestedSlice(existing.Object, "spec", "groups")
	if len(groups) == 0 {
		groups = []any{map[string]any{
			"name":  LegacyAlertRuleGroupName,
			"rules": []any{rule},
		}}
	} else if !replaceLegacyRuleInPlace(groups, p.Alert, rule) {
		first, _ := groups[0].(map[string]any)
		if first == nil {
			first = map[string]any{"name": LegacyAlertRuleGroupName}
		}
		rules, _ := first["rules"].([]any)
		first["rules"] = append(rules, rule)
		groups[0] = first
	}
	if err := unstructured.SetNestedSlice(existing.Object, groups, "spec", "groups"); err != nil {
		return nil, err
	}

	updated, err := ri.Update(ctx, existing, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	return updated.UnstructuredContent(), nil
}

// DeleteAlertRule removes a single rule by `alert` name from the canonical
// PrometheusRule CR. Missing CR or missing rule are no-ops — matches the
// legacy semantics so a delete-after-uninstall doesn't error.
func (m *Mutator) DeleteAlertRule(ctx context.Context, alert string) error {
	if m.dynamic == nil {
		return errors.New("mutate: dynamic client not configured")
	}
	if m.Namespace == "" {
		return errors.New("mutate: agent namespace not configured (set INSTALLATION_NAMESPACE)")
	}
	if alert == "" {
		return errors.New("mutate: alert name required")
	}
	ri := m.dynamic.Resource(prometheusRuleGVR).Namespace(m.Namespace)
	existing, err := ri.Get(ctx, LegacyAlertRuleCRDName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	groups, _, _ := unstructured.NestedSlice(existing.Object, "spec", "groups")
	changed := false
	for gi := range groups {
		g, _ := groups[gi].(map[string]any)
		if g == nil {
			continue
		}
		rules, _ := g["rules"].([]any)
		kept := make([]any, 0, len(rules))
		for _, raw := range rules {
			r, _ := raw.(map[string]any)
			if r != nil && r["alert"] == alert {
				changed = true
				continue
			}
			kept = append(kept, raw)
		}
		g["rules"] = kept
		groups[gi] = g
	}
	if !changed {
		return nil
	}
	if err := unstructured.SetNestedSlice(existing.Object, groups, "spec", "groups"); err != nil {
		return err
	}
	_, err = ri.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// replaceLegacyRuleInPlace walks each group's rules and replaces the first
// entry whose `alert` equals the target name. Returns true if a replacement
// happened.
func replaceLegacyRuleInPlace(groups []any, alert string, rule map[string]any) bool {
	for gi := range groups {
		g, _ := groups[gi].(map[string]any)
		if g == nil {
			continue
		}
		rules, _ := g["rules"].([]any)
		for ri := range rules {
			r, _ := rules[ri].(map[string]any)
			if r == nil || r["alert"] != alert {
				continue
			}
			rules[ri] = rule
			g["rules"] = rules
			groups[gi] = g
			return true
		}
	}
	return false
}

// newLegacyAlertRuleCR builds a fresh canonical PrometheusRule CR with the
// Robusta-compat labels so a parallel legacy runner can still find it via
// the same label selector.
func newLegacyAlertRuleCR(namespace string, rules []any) *unstructured.Unstructured {
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
				map[string]any{
					"name":  LegacyAlertRuleGroupName,
					"rules": rules,
				},
			},
		},
	}}
}
