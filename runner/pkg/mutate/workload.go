// Package mutate implements the K8s mutation handlers:
// replace_workload + delete_workload + create_workload. The agent uses
// one dynamic-client code path per kind — the dynamic client handles
// both built-in workloads (AppsV1) and CRDs (Argo Rollouts, Karpenter)
// behind the same interface, so the per-kind switch is just a GVR +
// scope (namespaced vs cluster) lookup.
package mutate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// workloadKindEntry maps the user-facing kind string to (GVR, namespaced).
type workloadKindEntry struct {
	gvr        schema.GroupVersionResource
	namespaced bool
	// expectedKind is what the resulting unstructured object's `kind` field
	// will look like once the apiserver echoes it back. Used for the
	// success markdown formatted as `<Kind>/<ns>/<name> updated`.
	expectedKind string
}

// supportedWorkloadKinds enumerates the kinds replace_workload / delete_workload
// understand. Any kind not in this map gets the "RESOURCE_NOT_SUPPORTED"
// error.
var supportedWorkloadKinds = map[string]workloadKindEntry{
	"Deployment": {
		gvr:          schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
		namespaced:   true,
		expectedKind: "Deployment",
	},
	"DaemonSet": {
		gvr:          schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"},
		namespaced:   true,
		expectedKind: "DaemonSet",
	},
	"StatefulSet": {
		gvr:          schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"},
		namespaced:   true,
		expectedKind: "StatefulSet",
	},
	// Note: the legacy replace_workload dispatched ReplicaSet to
	// `replace_namespaced_stateful_set` — a wrong API call for the kind.
	// We use the correct ReplicaSet replace endpoint. Tests cover the
	// right behaviour explicitly so a future cross-port doesn't
	// reintroduce the typo.
	"ReplicaSet": {
		gvr:          schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"},
		namespaced:   true,
		expectedKind: "ReplicaSet",
	},
	// Argo Rollouts CRD — namespace-scoped.
	"Rollout": {
		gvr:          schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"},
		namespaced:   true,
		expectedKind: "Rollout",
	},
	// Karpenter NodePool CRD — cluster-scoped. The Karpenter API moved
	// from `karpenter.sh/v1alpha5` → `karpenter.sh/v1beta1` → `karpenter.sh/v1`
	// across versions; v1 is the current GA group as of Karpenter 1.x and
	// the current target for the NodePool model.
	"NodePool": {
		gvr:          schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"},
		namespaced:   false,
		expectedKind: "NodePool",
	},
	// Karpenter EC2NodeClass CRD — cluster-scoped, AWS-specific.
	"EC2NodeClass": {
		gvr:          schema.GroupVersionResource{Group: "karpenter.k8s.aws", Version: "v1", Resource: "ec2nodeclasses"},
		namespaced:   false,
		expectedKind: "EC2NodeClass",
	},
}

// ReplaceWorkload runs a Replace (full-spec PUT) against the named
// resource using the dynamic client. The body is the new manifest the
// caller wants applied — it must be a complete spec (Replace is not a
// patch). On success the apiserver returns the updated object;
// ReplaceWorkload returns its UnstructuredContent so the dispatch
// handler can render a success Finding.
//
// The legacy playbook special-cases each kind with a different
// typed-client call; the dynamic client unifies them. ResourceVersion
// is preserved from the existing object so concurrent edits get a 409
// Conflict (same behaviour the typed client gets implicitly).
func (m *Mutator) ReplaceWorkload(ctx context.Context, kind, namespace, name string, body any) (any, error) {
	if m.dynamic == nil {
		return nil, errors.New("mutate: dynamic client not configured")
	}
	if name == "" {
		return nil, errors.New("mutate: name required")
	}
	entry, ok := supportedWorkloadKinds[kind]
	if !ok {
		return nil, fmt.Errorf("mutate: replace_workload not supported for kind %q", kind)
	}
	if entry.namespaced && namespace == "" {
		return nil, fmt.Errorf("mutate: namespace required for %s", kind)
	}

	u, err := unstructuredFromBody(body)
	if err != nil {
		return nil, err
	}
	// Force kind/apiVersion + name/namespace so a body without them still
	// hits the right URL (kind is derived from the trigger event, not
	// from the body).
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   entry.gvr.Group,
		Version: entry.gvr.Version,
		Kind:    entry.expectedKind,
	})
	u.SetName(name)
	if entry.namespaced {
		u.SetNamespace(namespace)
	}

	ri := m.dynamic.Resource(entry.gvr)
	var existing *unstructured.Unstructured
	if entry.namespaced {
		existing, err = ri.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	} else {
		existing, err = ri.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("mutate: get existing %s/%s: %w", kind, name, err)
	}
	// Carry the existing ResourceVersion forward so the apiserver accepts
	// the update; without this every replace racing with a controller
	// reconcile would 409. The legacy typed client got this for free by
	// reading the resource into the action handler first.
	u.SetResourceVersion(existing.GetResourceVersion())

	var updated *unstructured.Unstructured
	if entry.namespaced {
		updated, err = ri.Namespace(namespace).Update(ctx, u, metav1.UpdateOptions{})
	} else {
		updated, err = ri.Update(ctx, u, metav1.UpdateOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("mutate: replace %s/%s: %w", kind, name, err)
	}
	return updated.UnstructuredContent(), nil
}

// unstructuredFromBody normalises whatever the caller passed (typed
// struct, map[string]any, JSON bytes/string) into an unstructured object
// the dynamic client accepts. Returns an error when the body can't be
// JSON-encoded into a valid Kubernetes object shape.
func unstructuredFromBody(body any) (*unstructured.Unstructured, error) {
	if body == nil {
		return nil, errors.New("mutate: replace body required")
	}
	var raw []byte
	switch v := body.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		raw = b
	}
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal(raw, &u.Object); err != nil {
		return nil, fmt.Errorf("unmarshal body: %w", err)
	}
	return u, nil
}
