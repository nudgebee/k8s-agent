package mutate

import (
	"context"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Prometheus CRD GVR. Read-only: we inspect spec.ruleSelector to learn which
// labels a PrometheusRule must carry to be picked up.
var prometheusGVR = schema.GroupVersionResource{
	Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheuses",
}

// ruleSelectorLabels returns the labels a PrometheusRule must carry for the
// cluster's Prometheus to select it, read from the Prometheus CR's
// spec.ruleSelector.matchLabels.
//
// Why this is discovered instead of hardcoded: prometheus-operator only
// evaluates a PrometheusRule whose labels match the Prometheus CR's
// ruleSelector. kube-prometheus-stack defaults that to
// `release: <its own Helm release name>`, which is NOT the agent's release
// name — the two are separate Helm releases in every install we ship. A
// PrometheusRule written without those labels is accepted by the apiserver,
// reported healthy, and never evaluated: the rule silently does nothing.
//
// A Prometheus CR in the agent's own namespace wins; otherwise the
// cluster-wide list is used, sorted by namespace/name so the choice is
// deterministic when several exist. Any failure (no RBAC, CRD absent, no
// Prometheus CR, selector empty or expression-only) returns nil — the caller
// then writes the CR without extra labels, which is the pre-existing
// behaviour.
func (m *Mutator) ruleSelectorLabels(ctx context.Context) map[string]string {
	if m.dynamic == nil {
		return nil
	}
	list, err := m.dynamic.Resource(prometheusGVR).Namespace(metav1.NamespaceAll).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		m.logger().Warn("mutate: cannot read Prometheus CRs to derive ruleSelector labels; "+
			"alert rules may not be evaluated", "error", err)
		return nil
	}
	if len(list.Items) == 0 {
		m.logger().Warn("mutate: no Prometheus CR found; writing alert rule without " +
			"ruleSelector labels")
		return nil
	}

	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		// Same-namespace CRs first, then namespace/name for determinism.
		iLocal := items[i].GetNamespace() == m.Namespace
		jLocal := items[j].GetNamespace() == m.Namespace
		if iLocal != jLocal {
			return iLocal
		}
		if items[i].GetNamespace() != items[j].GetNamespace() {
			return items[i].GetNamespace() < items[j].GetNamespace()
		}
		return items[i].GetName() < items[j].GetName()
	})

	for i := range items {
		raw, found, err := unstructured.NestedStringMap(items[i].Object, "spec", "ruleSelector", "matchLabels")
		if err != nil || !found || len(raw) == 0 {
			continue
		}
		m.logger().Info("mutate: derived PrometheusRule selector labels",
			"prometheus", items[i].GetNamespace()+"/"+items[i].GetName(), "labels", raw)
		return raw
	}

	// Every Prometheus either selects all rules (empty selector) or uses
	// matchExpressions, which we cannot satisfy by stamping labels.
	m.logger().Warn("mutate: no Prometheus CR exposes spec.ruleSelector.matchLabels; " +
		"writing alert rule without ruleSelector labels")
	return nil
}
