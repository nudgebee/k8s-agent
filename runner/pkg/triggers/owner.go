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
	case "job":
		// "mycron-29123456" → "mycron". Same rationale as the ReplicaSet
		// strip: every run of a CronJob gets a Job named with the scheduled
		// time appended, and without normalization each run is a distinct
		// owner → distinct fingerprint → a new Finding that never dedupes
		// against the previous run's.
		return OwnerRef{Name: stripJobGeneratedSuffix(name), Kind: lk}
	case "deployment", "daemonset", "statefulset", "cronjob",
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

// cronJobScheduleSuffix matches the scheduled-time suffix the CronJob
// controller appends to the Jobs it creates — getJobName is
// `fmt.Sprintf("%s-%d", cronJob.Name, scheduledTime.Unix()/60)`, i.e.
// `-<unix-minutes>` (currently 8 digits; 10 tolerates unix-seconds
// variants in older/forked controllers). This is a controller-defined
// format, not a naming-convention guess — the same trust level as the
// ReplicaSet pod-template-hash strip above. We deliberately do NOT try
// to recognize other generated-name shapes (random hex tails etc.):
// those are conventions, not contracts, and stripping them risks
// merging genuinely distinct hand-named Jobs. The agent's own scan Jobs
// don't need it — they are skipped wholesale via their managed-by label
// (see Engine.Match).
var cronJobScheduleSuffix = regexp.MustCompile(`-\d{8,10}$`)

// stripJobGeneratedSuffix normalizes a CronJob-created Job name to the
// CronJob's own name, so every scheduled run resolves to the same owner.
// Names without the scheduled-time suffix pass through unchanged.
func stripJobGeneratedSuffix(name string) string {
	if stripped := cronJobScheduleSuffix.ReplaceAllString(name, ""); stripped != name && stripped != "" {
		return stripped
	}
	return name
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
