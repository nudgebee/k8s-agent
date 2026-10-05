package triggers

import (
	"regexp"
	"strings"
)

// ResolveOwner walks one level of obj.metadata.ownerReferences and
// returns the top-level workload (Deployment / DaemonSet / StatefulSet /
// Job / CronJob) when it can derive it.
//
// We only have the current obj from kubewatch — we don't fetch parent
// objects from the API server because the matcher path is hot and we
// want zero K8s API calls. That limits us to one walk step. The
// ReplicaSet → Deployment/Rollout derivation is heuristic (strip the
// pod-template-hash suffix from the ReplicaSet name, with the
// rollouts-pod-template-hash label distinguishing Argo Rollout pods);
// same heuristic the legacy implementation uses.
//
// Returns zero OwnerRef when no controlling owner is set (bare Pod,
// no-controller resource).
//
// Why the owner matters: without it, every Pod restart from a ReplicaSet
// gets a different Pod name → different fingerprint → new Finding for
// every restart. Resolving to the Deployment lets us dedupe at the
// workload level, matching how the UI groups findings.
func ResolveOwner(obj map[string]any) OwnerRef {
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		return OwnerRef{}
	}
	refsRaw, _ := meta["ownerReferences"].([]any)
	for _, r := range refsRaw {
		ref, _ := r.(map[string]any)
		if ref == nil {
			continue
		}
		// Pick the controller=true ref. If `controller` is missing
		// entirely (pre-1.16 manifests, or some operators), fall
		// through and accept the first ref.
		isController, hasField := ref["controller"].(bool)
		if hasField && !isController {
			continue
		}
		name, _ := ref["name"].(string)
		kind, _ := ref["kind"].(string)
		if name == "" || kind == "" {
			continue
		}
		labels, _ := meta["labels"].(map[string]any)
		return canonicalOwner(kind, name, labels)
	}
	return OwnerRef{}
}

// canonicalOwner normalizes the (kind, name) pair to the top-level
// workload. ReplicaSet names are derived from their owning Deployment
// (or Argo Rollout) by appending a `-<10-char-hash>` suffix, so we
// strip that to get the controller name. Other kinds pass through
// unchanged. labels are the owned object's labels, used to tell a
// Rollout-owned ReplicaSet from a Deployment-owned one without an API
// call.
func canonicalOwner(kind, name string, labels map[string]any) OwnerRef {
	lk := strings.ToLower(kind)
	switch lk {
	case "replicaset":
		// Argo Rollouts stamps its pods with `rollouts-pod-template-hash`
		// (the Deployment controller uses `pod-template-hash`), so the
		// label identifies the ReplicaSet's controller from the pod alone.
		// The label value is the exact suffix on the ReplicaSet name.
		if hash, _ := labels["rollouts-pod-template-hash"].(string); hash != "" {
			return OwnerRef{Name: strings.TrimSuffix(name, "-"+hash), Kind: "rollout"}
		}
		// "web-7f9d8c5b6" → "web".
		return OwnerRef{Name: stripPodTemplateHash(name), Kind: "deployment"}
	case "deployment", "daemonset", "statefulset", "job", "cronjob",
		"rollout", "horizontalpodautoscaler", "node":
		return OwnerRef{Name: name, Kind: lk}
	default:
		// Unknown owner kind (Operator-managed CR, etc.). Keep as-is —
		// dedup still works at the immediate-owner level, just not
		// rolled up to the controller's controller.
		return OwnerRef{Name: name, Kind: lk}
	}
}

// podTemplateHashSuffix matches the hash suffix the Deployment controller
// appends to ReplicaSet names: `-<10 chars [bcdfghjklmnpqrstvwxz2456789]>`.
// The character set is the controller's hash alphabet (rand_string in
// k8s.io/apimachinery/pkg/util/rand). 10 chars is the current length, but
// the regex is permissive (8-12) to tolerate older clusters.
var podTemplateHashSuffix = regexp.MustCompile(`-[bcdfghjklmnpqrstvwxz2456789]{8,12}$`)

func stripPodTemplateHash(name string) string {
	return podTemplateHashSuffix.ReplaceAllString(name, "")
}

// generatedJobSuffix matches the per-run suffix appended to Job names, in the
// two shapes we actually observe in the field:
//
//   - `-<digits>` — what the CronJob controller appends (a unix-minute stamp,
//     e.g. `blinq-api-healthchecks-29764215`), and what indexed batch creators
//     use (`grade-astropy--astropy-13398`).
//   - `-<8+ hex>` — what our own one-Job-per-unit creators append
//     (`trivy-image-scan-24e032a5`, `kube-bench-scan-177dde3a`).
//
// 5 digits is the floor because a shorter numeric tail is more likely to be a
// meaningful part of the name (`postgres-15`) than a generated one.
var generatedJobSuffix = regexp.MustCompile(`-([0-9]{5,}|[0-9a-f]{8,})$`)

// generatedUUIDSuffix matches a full UUID tail (`nb-llm-ct-7e4998e5-2e91-4579-
// 989e-16926b6158e1`). It has to run before generatedJobSuffix, which would
// otherwise strip only the final hex group and leave a still-unique partial
// UUID behind.
var generatedUUIDSuffix = regexp.MustCompile(`-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// JobFamily strips the generated per-run suffix from a Job name so every run
// of the same logical job shares one identity.
//
// Why this is needed on top of ResolveOwner: a Job created directly (not by a
// CronJob) has no ownerReferences to walk, so ResolveOwner returns nothing and
// the Job name itself is the only identity available — and that name is unique
// per run. Fingerprinting on it makes every run a brand-new problem, so the
// occurrence chain in event_duplicates never forms (issue #36647). CronJob-owned
// Jobs are unaffected: ResolveOwner already lifts those to the CronJob, and
// callers should prefer the resolved owner and fall back to this.
//
// This is a heuristic, in the same spirit as stripPodTemplateHash above, and it
// errs toward collapsing: two genuinely different Jobs that differ only by a
// generated-looking tail become one family, which is the grouping we want.
func JobFamily(name string) string {
	stripped := generatedUUIDSuffix.ReplaceAllString(name, "")
	stripped = generatedJobSuffix.ReplaceAllString(stripped, "")
	// Never strip the name down to nothing — a Job literally named `12345678`
	// keeps its name rather than becoming an empty fingerprint component.
	if stripped == "" {
		return name
	}
	return stripped
}

// SubjectFromObj extracts the (name, namespace, lowercased-kind, node)
// for an obj. node is "" when not a Pod or when not yet scheduled.
// Used by Engine.Match to populate Match.Subject* fields uniformly.
func SubjectFromObj(kind string, obj map[string]any) (name, namespace, lowerKind, node string) {
	meta, _ := obj["metadata"].(map[string]any)
	if meta != nil {
		name, _ = meta["name"].(string)
		namespace, _ = meta["namespace"].(string)
	}
	lowerKind = strings.ToLower(kind)
	if lowerKind == "pod" {
		spec, _ := obj["spec"].(map[string]any)
		if spec != nil {
			node, _ = spec["nodeName"].(string)
		}
	}
	return
}
