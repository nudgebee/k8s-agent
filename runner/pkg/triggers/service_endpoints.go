package triggers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ------- Service selector mismatch / zero endpoints -------

// serviceBackendsTimeout caps the K8s reads the predicate + enricher make
// per Service event. Same rationale as recentEventsTimeout: the matcher
// path is hot, never block the trigger pipeline on a slow API server.
const serviceBackendsTimeout = 3 * time.Second

// podLabelSampleLimit caps the rows of the selector-vs-pod-labels
// comparison table attached as evidence.
const podLabelSampleLimit = 15

// serviceNoEndpointsMatcher fires when a Service's spec.selector matches
// no pods AND no workload pod template in its namespace — the Service has
// no endpoints and silently blackholes every request, while looking
// healthy at a glance (the classic `kubectl patch svc … selector:
// wrong-label` misconfiguration).
//
// Firing decision needs cluster state (do any pods carry these labels?),
// so this matcher uses PredicateCtx + the ServiceBackendsLister rather
// than a pure Predicate. With no lister wired it never fires.
//
// Why the workload-template check: "selector matches zero pods" alone
// false-positives on intentional scale-to-zero (replicas=0, KEDA). A
// selector that matches a Deployment/StatefulSet/DaemonSet pod template
// is wired to a real workload — no pods right now is a scale state, not a
// misconfiguration. Only a selector matching neither pods nor templates
// is genuinely broken.
//
// Why UPDATE only: helm installs Services before the workloads backing
// them (its install order sorts Services first), so firing on CREATE
// would flag every chart install for the seconds until the Deployment's
// pods appear. A selector broken from birth is still caught by the first
// subsequent UPDATE (including kubewatch resync re-emits, which carry
// obj==oldObj and re-evaluate this level-triggered predicate).
//
// SuppressOnResync stays false deliberately: it suppresses by
// creationTimestamp, which would mute every Service older than the agent
// — i.e. nearly all of them. Dedup is handled by the fingerprint
// (ns/name/selector) + the 6h rate limit instead, mirroring the
// node_not_ready pattern: one Finding per broken-selector episode,
// re-fired only while still broken 6h later. A *changed* selector hashes
// to a fresh fingerprint and fires immediately.
func serviceNoEndpointsMatcher() MatcherSpec {
	return MatcherSpec{
		Name:           "service_no_endpoints",
		Kind:           "Service",
		Operations:     []string{"update"},
		AggregationKey: "service_no_endpoints",
		Priority:       "HIGH",
		FindingType:    "issue",
		RateLimit:      6 * time.Hour,
		PredicateCtx:   serviceNoEndpointsPredicate,
		FingerprintFn: func(obj map[string]any) string {
			return fp("service_no_endpoints", metaNS(obj), metaName(obj),
				formatSelector(serviceSelector(obj)))
		},
		EnrichBlocks: serviceNoEndpointsEnrichBlocks,
	}
}

func serviceNoEndpointsPredicate(obj, _ map[string]any, ec EnrichContext) bool {
	if ec.ServiceBackends == nil {
		return false // unwired (unit tests / no K8s client) — cannot decide
	}
	sel := serviceSelector(obj)
	if len(sel) == 0 {
		return false // selector-less: ExternalName / manually-managed Endpoints
	}
	if serviceType(obj) == "ExternalName" {
		return false // kube-proxy ignores the selector for ExternalName
	}
	ns := metaNS(obj)
	if ns == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), serviceBackendsTimeout)
	defer cancel()
	// Fail open on API errors — never alert on missing data.
	anyPod, err := ec.ServiceBackends.AnyPodMatching(ctx, ns, sel)
	if err != nil || anyPod {
		return false
	}
	anyTemplate, err := ec.ServiceBackends.AnyWorkloadTemplateMatching(ctx, ns, sel)
	if err != nil || anyTemplate {
		return false // wired to a real workload → scale-to-zero, not misconfig
	}
	return true
}

// serviceNoEndpointsEnrichBlocks attaches what an operator needs to see
// the mismatch without kubectl: the configured selector, a plain-language
// statement of the condition, and a table of actual pod labels in the
// namespace to compare against.
func serviceNoEndpointsEnrichBlocks(obj, _ map[string]any, ec EnrichContext) []EvidenceBlock {
	if obj == nil {
		return nil
	}
	sel := serviceSelector(obj)
	ns := metaNS(obj)
	condition := fmt.Sprintf(
		"No pods and no workload pod templates in namespace `%s` match this selector. "+
			"The Service has no endpoints and cannot route traffic — requests to it fail "+
			"even though the Service object itself looks healthy.", ns)
	blocks := []EvidenceBlock{
		headerBlock(fmt.Sprintf("Service %s has no matching endpoints", metaName(obj))),
		markdownBlock(fmt.Sprintf("*Configured selector:* `%s`", formatSelector(sel))),
		// The collector passes additional_info.insights through to the
		// Finding's per-block insight list, which is what the investigate
		// page renders as Insights. Without this the page shows nothing:
		// its other insight sources are Warning rows in event tables (a
		// zero-endpoint Service emits no K8s events) and pod-status
		// extraction from the raw-event json (subject is a Service).
		{
			"type": "markdown",
			"data": condition,
			"additional_info": map[string]any{
				"insights": []map[string]any{{
					"message": fmt.Sprintf(
						"Service %s/%s selector (%s) matches no pods and no workload pod templates — traffic to the Service is failing",
						ns, metaName(obj), formatSelector(sel)),
					"severity": "Critical",
				}},
			},
		},
	}
	return append(blocks, podLabelComparisonTable(ec, ns)...)
}

// podLabelComparisonTable lists pods in the namespace with their labels so
// the selector-vs-labels mismatch is visible on the Finding card.
func podLabelComparisonTable(ec EnrichContext, ns string) []EvidenceBlock {
	if ec.ServiceBackends == nil || ns == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), serviceBackendsTimeout)
	defer cancel()
	samples, err := ec.ServiceBackends.ListPodLabels(ctx, ns, podLabelSampleLimit)
	if err != nil {
		return nil
	}
	if len(samples) == 0 {
		return []EvidenceBlock{markdownBlock(fmt.Sprintf(
			"Namespace `%s` currently has no pods at all.", ns))}
	}
	rows := make([]any, 0, len(samples))
	for _, s := range samples {
		rows = append(rows, []any{s.Name, formatSelector(s.Labels)})
	}
	return []EvidenceBlock{{
		"type": "table",
		"data": map[string]any{
			"table_name":       fmt.Sprintf("*Pod labels in namespace %s*", ns),
			"headers":          []any{"pod", "labels"},
			"rows":             rows,
			"column_renderers": map[string]any{},
		},
		"additional_info": nil,
	}}
}

// serviceSelector returns spec.selector as a string map, or nil when the
// Service has none (ExternalName / manually-managed Endpoints).
func serviceSelector(obj map[string]any) map[string]string {
	spec, _ := obj["spec"].(map[string]any)
	raw, _ := spec["selector"].(map[string]any)
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		s, _ := v.(string)
		out[k] = s
	}
	return out
}

// serviceType returns spec.type ("ClusterIP" / "NodePort" / "LoadBalancer"
// / "ExternalName"), or "" when unset (defaults to ClusterIP).
func serviceType(obj map[string]any) string {
	spec, _ := obj["spec"].(map[string]any)
	t, _ := spec["type"].(string)
	return t
}

// formatSelector renders a label map as sorted "k=v,k=v" — stable for
// fingerprints and readable in evidence blocks.
func formatSelector(sel map[string]string) string {
	pairs := make([]string, 0, len(sel))
	for k, v := range sel {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}
