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
	"k8s.io/client-go/util/retry"
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

	// Resolve once, outside the retry loop: the selector can't change between
	// conflict retries, and each lookup is an extra apiserver call.
	selectorLabels := m.ruleSelectorLabels(ctx)

	ri := m.dynamic.Resource(prometheusRuleGVR).Namespace(m.Namespace)
	// Shared CR + concurrent UI sessions → 409 Conflict whenever two callers
	// land between Get and Update. RetryOnConflict re-reads + re-applies the
	// patch on each conflict; the AlreadyExists branch covers the rare case
	// where two callers race to create the CR at once (we surface that as a
	// Conflict so the retry loop handles it the same way).
	var result *unstructured.Unstructured
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, gerr := ri.Get(ctx, LegacyAlertRuleCRDName, metav1.GetOptions{})
		if apierrors.IsNotFound(gerr) {
			u := newLegacyAlertRuleCR(m.Namespace, []any{rule}, selectorLabels)
			created, cerr := ri.Create(ctx, u, metav1.CreateOptions{})
			if cerr != nil {
				if apierrors.IsAlreadyExists(cerr) {
					return apierrors.NewConflict(
						schema.GroupResource{Group: prometheusRuleGVR.Group, Resource: prometheusRuleGVR.Resource},
						LegacyAlertRuleCRDName, cerr)
				}
				return cerr
			}
			result = created
			return nil
		}
		if gerr != nil {
			return gerr
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
		if serr := unstructured.SetNestedSlice(existing.Object, groups, "spec", "groups"); serr != nil {
			return serr
		}
		// Heal a CR created before selector labels were stamped (or after the
		// Prometheus release was renamed): without this, every rule written
		// into an already-mislabelled CR stays invisible to prometheus-operator
		// forever, since labels were only ever set on create.
		applySelectorLabels(existing, selectorLabels)

		updated, uerr := ri.Update(ctx, existing, metav1.UpdateOptions{})
		if uerr != nil {
			return uerr
		}
		result = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result.UnstructuredContent(), nil
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
	// Same conflict story as CreateOrReplaceAlertRule: shared CR, concurrent
	// writers. Retry re-reads + re-applies the deletion on 409.
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, gerr := ri.Get(ctx, LegacyAlertRuleCRDName, metav1.GetOptions{})
		if apierrors.IsNotFound(gerr) {
			return nil
		}
		if gerr != nil {
			return gerr
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
				if r != nil && str(r, "alert") == alert {
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
		if serr := unstructured.SetNestedSlice(existing.Object, groups, "spec", "groups"); serr != nil {
			return serr
		}
		_, uerr := ri.Update(ctx, existing, metav1.UpdateOptions{})
		return uerr
	})
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
			if r == nil || str(r, "alert") != alert {
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

// applySelectorLabels adds any missing selector labels to an existing CR.
// Existing values are left alone: an operator who deliberately retargeted the
// CR keeps their edit.
func applySelectorLabels(u *unstructured.Unstructured, selectorLabels map[string]string) {
	if len(selectorLabels) == 0 {
		return
	}
	labels := u.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	changed := false
	for k, v := range selectorLabels {
		if _, ok := labels[k]; !ok {
			labels[k] = v
			changed = true
		}
	}
	if changed {
		u.SetLabels(labels)
	}
}

// newLegacyAlertRuleCR builds a fresh canonical PrometheusRule CR with the
// Robusta-compat labels so a parallel legacy runner can still find it via
// the same label selector, plus selectorLabels — the Prometheus CR's
// ruleSelector.matchLabels, without which prometheus-operator never loads
// the rule (see ruleSelectorLabels).
func newLegacyAlertRuleCR(namespace string, rules []any, selectorLabels map[string]string) *unstructured.Unstructured {
	labels := map[string]any{
		LegacyAlertRuleLabelKey: LegacyAlertRuleLabelValue,
		"role":                  "alert-rules",
	}
	for k, v := range selectorLabels {
		labels[k] = v
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "monitoring.coreos.com/v1",
		"kind":       "PrometheusRule",
		"metadata": map[string]any{
			"name":      LegacyAlertRuleCRDName,
			"namespace": namespace,
			"labels":    labels,
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

// AlertRuleLocator names one rule inside one PrometheusRule: api-server sends it
// for rules it synced from any CR (not only the canonical one), so a change lands
// where the rule is defined instead of creating a copy in the canonical CR.
type AlertRuleLocator struct {
	Namespace string // CR namespace; the install namespace when empty
	Name      string // CR name
	Group     string // rule group; any group when empty
	Alert     string // rule name
}

// ErrAlertRuleNotFound is returned when the locator matches no rule.
var ErrAlertRuleNotFound = errors.New("mutate: alert rule not found in PrometheusRule")

// ErrAlertRuleAmbiguous is returned when the locator matches more than one rule.
var ErrAlertRuleAmbiguous = errors.New("mutate: more than one rule matches; give the rule group")

func (m *Mutator) locatedRuleClient(loc AlertRuleLocator) (dynamic.ResourceInterface, error) {
	if m.dynamic == nil {
		return nil, errors.New("mutate: dynamic client not configured")
	}
	if loc.Name == "" || loc.Alert == "" {
		return nil, errors.New("mutate: PrometheusRule name and alert are required")
	}
	ns := loc.Namespace
	if ns == "" {
		ns = m.Namespace
	}
	if ns == "" {
		return nil, errors.New("mutate: PrometheusRule namespace required")
	}
	return m.dynamic.Resource(prometheusRuleGVR).Namespace(ns), nil
}

// ruleMatch is the position of one rule inside spec.groups.
type ruleMatch struct{ group, rule int }

func findLocatedRules(groups []any, loc AlertRuleLocator) []ruleMatch {
	var out []ruleMatch
	for gi, raw := range groups {
		g, _ := raw.(map[string]any)
		if g == nil || (loc.Group != "" && str(g, "name") != loc.Group) {
			continue
		}
		rules, _ := g["rules"].([]any)
		for ri, rr := range rules {
			if r, _ := rr.(map[string]any); r != nil && str(r, "alert") == loc.Alert {
				out = append(out, ruleMatch{gi, ri})
			}
		}
	}
	return out
}

// PatchAlertRuleInCR changes the expression and `for` of one rule in the named
// PrometheusRule and leaves its labels, annotations and every other rule alone.
// When the rule is absent (it was removed by DeleteAlertRuleInCR) the full rule
// from p is appended to loc.Group, creating the group if needed.
func (m *Mutator) PatchAlertRuleInCR(ctx context.Context, loc AlertRuleLocator, p LegacyAlertRuleParams) (any, error) {
	ri, err := m.locatedRuleClient(loc)
	if err != nil {
		return nil, err
	}
	if p.Expr == "" {
		return nil, errors.New("mutate: expr required")
	}
	var result *unstructured.Unstructured
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, gerr := ri.Get(ctx, loc.Name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		groups, _, _ := unstructured.NestedSlice(existing.Object, "spec", "groups")
		matches := findLocatedRules(groups, loc)
		switch len(matches) {
		case 0:
			if loc.Group == "" {
				return fmt.Errorf("%w: %s; a rule group is required to add it back", ErrAlertRuleNotFound, loc.Alert)
			}
			rule := map[string]any{"alert": loc.Alert, "expr": p.Expr}
			if p.Duration != "" {
				rule["for"] = p.Duration
			}
			if len(p.Annotations) > 0 {
				rule["annotations"] = p.Annotations
			}
			if len(p.Labels) > 0 {
				rule["labels"] = p.Labels
			}
			groups = appendRuleToGroup(groups, loc.Group, rule)
		case 1:
			g := groups[matches[0].group].(map[string]any)
			rules := g["rules"].([]any)
			r := rules[matches[0].rule].(map[string]any)
			r["expr"] = p.Expr
			if p.Duration != "" {
				r["for"] = p.Duration
			}
		default:
			return fmt.Errorf("%w: %s", ErrAlertRuleAmbiguous, loc.Alert)
		}
		if serr := unstructured.SetNestedSlice(existing.Object, groups, "spec", "groups"); serr != nil {
			return serr
		}
		updated, uerr := ri.Update(ctx, existing, metav1.UpdateOptions{})
		if uerr != nil {
			return uerr
		}
		result = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result.UnstructuredContent(), nil
}

// DeleteAlertRuleInCR removes one rule from the named PrometheusRule. It never
// deletes the PrometheusRule itself, even when the rule was its last one.
func (m *Mutator) DeleteAlertRuleInCR(ctx context.Context, loc AlertRuleLocator) error {
	ri, err := m.locatedRuleClient(loc)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, gerr := ri.Get(ctx, loc.Name, metav1.GetOptions{})
		if gerr != nil {
			return gerr
		}
		groups, _, _ := unstructured.NestedSlice(existing.Object, "spec", "groups")
		matches := findLocatedRules(groups, loc)
		switch len(matches) {
		case 0:
			return fmt.Errorf("%w: %s", ErrAlertRuleNotFound, loc.Alert)
		case 1:
		default:
			return fmt.Errorf("%w: %s", ErrAlertRuleAmbiguous, loc.Alert)
		}
		g := groups[matches[0].group].(map[string]any)
		rules := g["rules"].([]any)
		g["rules"] = append(rules[:matches[0].rule:matches[0].rule], rules[matches[0].rule+1:]...)
		groups[matches[0].group] = g
		if serr := unstructured.SetNestedSlice(existing.Object, groups, "spec", "groups"); serr != nil {
			return serr
		}
		_, uerr := ri.Update(ctx, existing, metav1.UpdateOptions{})
		return uerr
	})
}

// appendRuleToGroup adds rule to the group named name, creating the group at the
// end when the CR has none by that name.
func appendRuleToGroup(groups []any, name string, rule map[string]any) []any {
	for gi, raw := range groups {
		g, _ := raw.(map[string]any)
		if g == nil || str(g, "name") != name {
			continue
		}
		rules, _ := g["rules"].([]any)
		g["rules"] = append(rules, rule)
		groups[gi] = g
		return groups
	}
	return append(groups, map[string]any{"name": name, "rules": []any{rule}})
}
