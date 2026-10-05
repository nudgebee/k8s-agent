// Package mutate — replica_rightsizing.
//
// replica_rightsizing changes a workload's replica count. It arrives only via
// the agent_task poller (trusted path), built by the api-server for two flows:
//   - abandoned_resource / scale-to-zero (replica_count int 0)
//   - event-resolution increase_replicas (replica_count string, e.g. "3")
//
// so the handler coerces replica_count from either a JSON number or a numeric
// string. Only Deployment/StatefulSet/Rollout are scalable (the legacy robusta
// action no-op'd Pod/Job/DaemonSet). We merge-patch spec.replicas through the
// dynamic client, which covers the built-in workloads and the Argo Rollout CRD
// behind one code path.
package mutate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// scalableWorkloadGVRs maps the lowercased kind to its GVR. Keyed lowercase so
// the lookup tolerates whatever casing the api-server forwards (controllerKind
// / SubjectOwnerKind are usually capitalized, but we don't depend on it).
// Deliberately excludes DaemonSet (no spec.replicas) and ReplicaSet (legacy
// excluded it; scaling a bare RS fights its owning controller).
var scalableWorkloadGVRs = map[string]schema.GroupVersionResource{
	"deployment":  {Group: "apps", Version: "v1", Resource: "deployments"},
	"statefulset": {Group: "apps", Version: "v1", Resource: "statefulsets"},
	"rollout":     {Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"},
}

// ScaleWorkload sets spec.replicas on a Deployment/StatefulSet/Rollout via a
// strategic-/merge-patch through the dynamic client. replicas may be 0
// (scale-to-zero). namespace + name required; kind is case-insensitive.
func (m *Mutator) ScaleWorkload(ctx context.Context, kind, namespace, name string, replicas int64) (any, error) {
	if m.dynamic == nil {
		return nil, errors.New("mutate: dynamic client not configured")
	}
	if namespace == "" || name == "" {
		return nil, errors.New("mutate: namespace and name required")
	}
	if replicas < 0 {
		return nil, fmt.Errorf("mutate: replica_count must be >= 0, got %d", replicas)
	}
	gvr, ok := scalableWorkloadGVRs[strings.ToLower(kind)]
	if !ok {
		return nil, fmt.Errorf("replica_rightsizing: unsupported kind %q (Deployment|StatefulSet|Rollout)", kind)
	}

	// Read the count we are about to overwrite, so an undo has something to restore. The patch is
	// blind by nature — a merge-patch reports the new state, never the old — so without this the
	// scale is a one-way door.
	//
	// Best-effort, and captured before the patch: failing to record how to undo a scale must not turn
	// a successful scale into a failed one. An unreadable object simply reports no previous count, and
	// the UI then offers no Undo rather than one that would restore a guess.
	previousReplicas, hadPrevious := m.currentReplicas(ctx, gvr, namespace, name)

	patch := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas)
	updated, err := m.dynamic.Resource(gvr).Namespace(namespace).
		Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		return nil, fmt.Errorf("mutate: scale %s/%s/%s to %d: %w", kind, namespace, name, replicas, err)
	}
	out := map[string]any{
		"success": true,
		"message": fmt.Sprintf("%s/%s/%s scaled to %d replicas", kind, namespace, name, replicas),
		"updated": updated.UnstructuredContent(),
	}
	if hadPrevious {
		out["previous_replicas"] = previousReplicas
	}
	return out, nil
}

// currentReplicas reads spec.replicas before a scale. Reports false when the object cannot be read,
// so the caller records nothing rather than a guess.
//
// An absent spec.replicas is reported as 1: that is the value the apiserver defaults it to for every
// kind here, so restoring 1 restores the state that was actually running. Treating absent as 0 would
// undo a scale-up by scaling the workload to nothing.
func (m *Mutator) currentReplicas(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (int64, bool) {
	obj, err := m.dynamic.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, false
	}
	replicas, found, err := unstructured.NestedInt64(obj.UnstructuredContent(), "spec", "replicas")
	if err != nil {
		return 0, false
	}
	if !found {
		return 1, true
	}
	return replicas, true
}

// toInt64 coerces a JSON-decoded value that may be a float64 (number) or a
// string (the event-resolution path sends replica_count as a string). Returns
// false when the value is absent or not a whole number.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		parsed, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}
