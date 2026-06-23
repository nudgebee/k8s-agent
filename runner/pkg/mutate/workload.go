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
	"strings"

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

// resolveWorkloadKind does a case-insensitive lookup of the user-facing kind
// against supportedWorkloadKinds. Callers aren't consistent about casing: the
// edit/create UI sends TitleCase ("Deployment", "NodePool") while the delete UI
// sends `kind.toLowerCase()` ("deployment") — see
// app/src/components/k8s/details/KubernetesWorkloads.jsx. It returns the
// canonical map key alongside the entry so callers render messages and pick the
// per-kind body field with the canonical spelling.
func resolveWorkloadKind(kind string) (canonical string, entry workloadKindEntry, ok bool) {
	for k, e := range supportedWorkloadKinds {
		if strings.EqualFold(k, kind) {
			return k, e, true
		}
	}
	return "", workloadKindEntry{}, false
}

// DeleteWorkload deletes the named workload via the dynamic client, reusing
// ReplaceWorkload's per-kind GVR + scope lookup. Propagation is Background so
// the apiserver garbage-collects dependents (ReplicaSets/Pods) — matching
// `kubectl delete deployment` rather than orphaning them.
func (m *Mutator) DeleteWorkload(ctx context.Context, kind, namespace, name string) (any, error) {
	if m.dynamic == nil {
		return nil, errors.New("mutate: dynamic client not configured")
	}
	if name == "" {
		return nil, errors.New("mutate: name required")
	}
	canonical, entry, ok := resolveWorkloadKind(kind)
	if !ok {
		return nil, fmt.Errorf("mutate: delete_workload not supported for kind %q", kind)
	}
	if entry.namespaced && namespace == "" {
		return nil, fmt.Errorf("mutate: namespace required for %s", canonical)
	}

	prop := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &prop}
	ri := m.dynamic.Resource(entry.gvr)
	var err error
	if entry.namespaced {
		err = ri.Namespace(namespace).Delete(ctx, name, opts)
	} else {
		err = ri.Delete(ctx, name, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("mutate: delete %s/%s: %w", canonical, name, err)
	}

	loc := name
	if namespace != "" {
		loc = namespace + "/" + name
	}
	return map[string]any{
		"success": true,
		"message": fmt.Sprintf("%s/%s deleted", canonical, loc),
	}, nil
}

// CreateWorkload creates a new workload from the supplied manifest via the
// dynamic client. It shares unstructuredFromBody + the per-kind GVR lookup with
// ReplaceWorkload so the create/edit UI can send the same payload shape to
// either action — the only difference is Create vs Update (no Get/ResourceVersion
// dance, since there is no existing object). On success the apiserver-echoed
// object is returned so the dispatch handler can render a success Finding.
func (m *Mutator) CreateWorkload(ctx context.Context, kind, namespace, name string, body any) (any, error) {
	if m.dynamic == nil {
		return nil, errors.New("mutate: dynamic client not configured")
	}
	canonical, entry, ok := resolveWorkloadKind(kind)
	if !ok {
		return nil, fmt.Errorf("mutate: create_workload not supported for kind %q", kind)
	}
	if entry.namespaced && namespace == "" {
		return nil, fmt.Errorf("mutate: namespace required for %s", canonical)
	}

	u, err := unstructuredFromBody(body)
	if err != nil {
		return nil, err
	}
	// Force kind/apiVersion so a body that omits them still targets the right
	// GVR; name/namespace from params win over the body when provided (the
	// delete/replace handlers do the same), otherwise we trust metadata.name.
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   entry.gvr.Group,
		Version: entry.gvr.Version,
		Kind:    entry.expectedKind,
	})
	if name != "" {
		u.SetName(name)
	}
	if entry.namespaced {
		u.SetNamespace(namespace)
	}

	ri := m.dynamic.Resource(entry.gvr)
	var created *unstructured.Unstructured
	if entry.namespaced {
		created, err = ri.Namespace(namespace).Create(ctx, u, metav1.CreateOptions{})
	} else {
		created, err = ri.Create(ctx, u, metav1.CreateOptions{})
	}
	if err != nil {
		return nil, fmt.Errorf("mutate: create %s/%s: %w", canonical, u.GetName(), err)
	}
	return created.UnstructuredContent(), nil
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
