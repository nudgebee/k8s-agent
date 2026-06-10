package rightsize

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// rightsizing_resource — apply a backend-computed recommendation (explicit
// per-container CPU/memory request+limit, plus correlation annotations) to a
// workload. A faithful port of the legacy Python playbook action
// playbooks/nudgebee_playbooks/rightsizing.py:rightsizing_resource.
//
// Distinct from continuous_rightsizing (rightsize.go): that action samples
// Prometheus and *computes* the recommendation; this one *applies* values the
// backend already decided. Driven by api-server's ApplyRecommendation
// (account/adapter/kuberntes.go) over the agent_task poller (pkg/tasks) — a
// trusted GET/POST path — so it carries no signature and is registered as a
// plain handler, NOT a lightAction (an unsigned WS delivery of a mutation is
// correctly rejected).

// applyKind describes how to read + patch one workload kind on the apply path.
type applyKind struct {
	gvr           schema.GroupVersionResource
	containerPath []string // nested path to the []container slice
	noop          bool     // Job: accepted but intentionally not applied (legacy parity)
}

// workloadContainerPath is where every controller kind exposes its pod template
// containers. Pods expose them one level up (spec.containers).
var workloadContainerPath = []string{"spec", "template", "spec", "containers"}

// applyKinds mirrors the legacy RIGHTSIZING_HANDLERS map exactly: Deployment,
// Pod, Job (no-op), DaemonSet, StatefulSet, Rollout. ReplicaSet is deliberately
// absent — the Python handler map had no ReplicaSet entry, so the backend got
// RESOURCE_NOT_SUPPORTED for it; we preserve that rather than silently start
// patching a Deployment-owned ReplicaSet (which the controller would revert).
var applyKinds = map[string]applyKind{
	"Deployment":  {gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, containerPath: workloadContainerPath},
	"StatefulSet": {gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, containerPath: workloadContainerPath},
	"DaemonSet":   {gvr: schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}, containerPath: workloadContainerPath},
	"Rollout":     {gvr: schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}, containerPath: workloadContainerPath},
	"Pod":         {gvr: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}, containerPath: []string{"spec", "containers"}},
	"Job":         {noop: true},
}

// Applier applies explicit rightsizing recommendations. It needs only a dynamic
// client (no Prometheus) — main registers it whenever the dynamic client is up.
type Applier struct {
	dyn dynamic.Interface
}

// NewApplier builds an Applier. dyn is required.
func NewApplier(dyn dynamic.Interface) *Applier { return &Applier{dyn: dyn} }

// containerChange is one container's target resources from the backend. An
// empty string means "clear that key" — the backend sends JSON null for a
// resource it doesn't want set, which decodes to "" here, and we delete it,
// matching the legacy update_container_resources (None → del, value → set).
type containerChange struct {
	name   string
	reqCPU string
	limCPU string
	reqMem string
	limMem string
}

// Handle is the dispatch entry point for action_name "rightsizing_resource".
// On success it returns {success:true, response:"Succeeded"}; the task poller
// stores the map as the task response and the collector keys task status off
// data["success"] (k8s-collector task_handler.save_task_status).
func (a *Applier) Handle(ctx context.Context, params map[string]any) (any, error) {
	kind := strField(params, "kind")
	name := strField(params, "name")
	namespace := strField(params, "namespace")
	if kind == "" || name == "" || namespace == "" {
		return nil, fmt.Errorf("rightsizing_resource: kind, name and namespace are required (got kind=%q name=%q namespace=%q)", kind, name, namespace)
	}

	spec, ok := applyKinds[kind]
	if !ok {
		return nil, fmt.Errorf("rightsizing_resource: %s rightsizing is not supported", kind)
	}
	if spec.noop {
		// Legacy rightsize_job was a logged no-op. Report success so the
		// recommendation resolves instead of the task retrying forever.
		return map[string]any{"success": true, "response": "Succeeded", "msg": fmt.Sprintf("%s rightsizing not supported yet", kind)}, nil
	}

	changes, err := parseContainerChanges(params["containers"])
	if err != nil {
		return nil, fmt.Errorf("rightsizing_resource: %w", err)
	}
	annotations := parseAnnotations(params["annotations"])

	ri := a.dyn.Resource(spec.gvr).Namespace(namespace)
	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("rightsizing_resource: get %s %s/%s: %w", kind, namespace, name, err)
	}

	if err := applyContainerChanges(obj, spec.containerPath, changes); err != nil {
		return nil, fmt.Errorf("rightsizing_resource: %s %s/%s: %w", kind, namespace, name, err)
	}
	mergeAnnotations(obj, annotations)

	// Dry-run first (the legacy handlers did a dry_run=All replace before the
	// real apply) so an invalid spec fails the task cleanly instead of
	// half-applying across containers.
	if _, err := ri.Update(ctx, obj, metav1.UpdateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
		return nil, fmt.Errorf("rightsizing_resource: dry-run apply %s %s/%s: %w", kind, namespace, name, err)
	}
	if _, err := ri.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
		return nil, fmt.Errorf("rightsizing_resource: apply %s %s/%s: %w", kind, namespace, name, err)
	}

	return map[string]any{"success": true, "response": "Succeeded"}, nil
}

// parseContainerChanges decodes the action_params.containers list. It rejects a
// missing/empty list and an all-empty container entry, mirroring the legacy
// RightsizingParams.validate_containers + Container.__init__ guards.
func parseContainerChanges(raw any) ([]containerChange, error) {
	list, _ := raw.([]any)
	if len(list) == 0 {
		return nil, fmt.Errorf("containers can not be empty")
	}
	out := make([]containerChange, 0, len(list))
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		c := containerChange{
			name:   strField(m, "container_name"),
			reqCPU: asString(m["cpu_request"]),
			limCPU: asString(m["cpu_limit"]),
			reqMem: asString(m["memory_request"]),
			limMem: asString(m["memory_limit"]),
		}
		if c.name == "" {
			return nil, fmt.Errorf("container entry missing container_name")
		}
		if c.reqCPU == "" && c.limCPU == "" && c.reqMem == "" && c.limMem == "" {
			return nil, fmt.Errorf("container %q: all resource values can not be empty", c.name)
		}
		if err := validateChange(c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("containers can not be empty")
	}
	return out, nil
}

// validateChange enforces request <= limit per resource when both are set,
// mirroring the legacy validate_changes.
func validateChange(c containerChange) error {
	if err := requestNotAboveLimit(c.reqCPU, c.limCPU); err != nil {
		return fmt.Errorf("container %q cpu: %w", c.name, err)
	}
	if err := requestNotAboveLimit(c.reqMem, c.limMem); err != nil {
		return fmt.Errorf("container %q memory: %w", c.name, err)
	}
	return nil
}

// requestNotAboveLimit errors when both request and limit are set and the
// request exceeds the limit. Either side empty (not being changed) passes.
func requestNotAboveLimit(req, lim string) error {
	if req == "" || lim == "" {
		return nil
	}
	rq, err := resource.ParseQuantity(req)
	if err != nil {
		return fmt.Errorf("invalid request quantity %q: %w", req, err)
	}
	lq, err := resource.ParseQuantity(lim)
	if err != nil {
		return fmt.Errorf("invalid limit quantity %q: %w", lim, err)
	}
	if rq.Cmp(lq) > 0 {
		return fmt.Errorf("request %s must be <= limit %s", req, lim)
	}
	return nil
}

// applyContainerChanges sets the request/limit values on each named container
// found at path. Reuses setContainerResources (rightsize.go) for the set-or-
// delete semantics. Errors if the workload has no containers, or none of the
// requested containers exist.
func applyContainerChanges(obj *unstructured.Unstructured, path []string, changes []containerChange) error {
	containers, found, err := unstructured.NestedSlice(obj.Object, path...)
	if err != nil {
		return fmt.Errorf("read containers: %w", err)
	}
	if !found || len(containers) == 0 {
		return fmt.Errorf("no containers at %s", strings.Join(path, "."))
	}

	byName := make(map[string]containerChange, len(changes))
	for _, c := range changes {
		byName[c.name] = c
	}

	matched := 0
	for i := range containers {
		c, _ := containers[i].(map[string]any)
		if c == nil {
			continue
		}
		cname, _ := c["name"].(string)
		ch, ok := byName[cname]
		if !ok {
			continue
		}
		matched++
		setContainerResources(c, ch.reqCPU, ch.reqMem, ch.limCPU, ch.limMem)
		containers[i] = c
	}
	if matched == 0 {
		return fmt.Errorf("none of the requested containers were found in the workload")
	}

	if err := unstructured.SetNestedSlice(obj.Object, containers, path...); err != nil {
		return fmt.Errorf("rebuild containers: %w", err)
	}
	return nil
}

// parseAnnotations coerces action_params.annotations into a string map,
// dropping nil / "None" / empty values exactly like the legacy
// nb_annotations_cleaned filter.
func parseAnnotations(raw any) map[string]string {
	m, _ := raw.(map[string]any)
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if v == nil {
			continue
		}
		s := asString(v)
		if s == "" || s == "None" {
			continue
		}
		out[k] = s
	}
	return out
}

// mergeAnnotations merges the cleaned annotations onto the workload metadata,
// preserving any existing annotations (legacy: metadata.annotations.update()).
func mergeAnnotations(obj *unstructured.Unstructured, annotations map[string]string) {
	if len(annotations) == 0 {
		return
	}
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	for k, v := range annotations {
		ann[k] = v
	}
	obj.SetAnnotations(ann)
}
