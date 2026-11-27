package triggers

import (
	"strings"
	"time"
)

// QualifiedKind is how a custom resource's kind is named inside the runner
// and on the wire: "Certificate.cert-manager.io". The engine matches on kind
// alone, so a bare "Service" from serving.knative.dev would otherwise be run
// through the core service_no_endpoints matcher, and the backend would treat
// it as a core Service. No built-in kind contains a dot, which is what the
// backend keys on to keep these Findings unattributed.
func QualifiedKind(kind, group string) string {
	return kind + "." + group
}

// CustomResourceDiffPaths is the default set of top-level fields diffed for a
// custom resource.
var CustomResourceDiffPaths = []string{"spec"}

// CustomResourceChangeMatcher fires when a listed custom resource's diffed
// fields change. It is the babysitter matcher for a kind the operator chose,
// with three differences:
//
//   - The Finding is unattributed (APIGroup set): no owner walk and no service
//     key. A custom resource is usually named after the workload it configures,
//     and linking by name would assert a causal link that may not exist.
//   - Churn suppression is on, with the custom-resource thresholds: some
//     controllers rewrite their own spec on a loop (KEDA, Flagger, Argo).
//   - The diffed fields are the entry's diffPaths, "spec" by default.
func CustomResourceChangeMatcher(kind, group string, diffPaths []string) MatcherSpec {
	if len(diffPaths) == 0 {
		diffPaths = CustomResourceDiffPaths
	}
	qualified := QualifiedKind(kind, group)
	diffOpt := SpecDiffOptions{
		FieldsToMonitor: diffPaths,
		FieldsToOmit: []string{
			"status",
			"metadata.generation",
			"metadata.resourceVersion",
			"metadata.managedFields",
		},
	}
	return MatcherSpec{
		Name:           "custom_change_" + strings.ToLower(kind) + "_" + group,
		Kind:           qualified,
		APIGroup:       group,
		SubjectKind:    kind,
		Operations:     []string{"update"},
		AggregationKey: "ConfigurationChange/KubernetesResource/Change",
		Priority:       "INFO",
		FindingType:    "configuration_change",
		RateLimit:      30 * time.Second,
		SuppressChurn:  true,
		Predicate: func(obj, oldObj map[string]any) bool {
			if oldObj == nil {
				return false
			}
			return len(ComputeSpecDiff(obj, oldObj, diffOpt)) > 0
		},
		FingerprintFn: func(obj map[string]any) string {
			// Same identity rule as the babysitter kinds: the resource that
			// changed, so repeat edits chain into one recurring entry. The
			// qualified kind keeps it apart from a built-in kind of the same
			// name in the same namespace.
			return fp("ConfigurationChange/KubernetesResource/Change",
				metaNS(obj), strings.ToLower(qualified), metaName(obj))
		},
		EnrichBlocks: func(obj, oldObj map[string]any, _ EnrichContext) []EvidenceBlock {
			diffs := ComputeSpecDiff(obj, oldObj, diffOpt)
			if len(diffs) == 0 {
				return nil
			}
			return []EvidenceBlock{BuildKubernetesDiffBlock(obj, oldObj, qualified, diffs)}
		},
	}
}

// AppliedCopyAnnotations lists the annotations that each hold a full copy of
// the object they sit on. The runner's custom-resource cache drops them, so
// fields outside the diffed ones never reach the evidence.
func AppliedCopyAnnotations() []string {
	return append([]string(nil), appliedCopyAnnotations...)
}
