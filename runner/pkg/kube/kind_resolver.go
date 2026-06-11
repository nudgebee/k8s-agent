package kube

import (
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// kindToGVR maps a user-facing resource Kind (lowercased) to its canonical
// GroupVersionResource.
//
// Why this exists: the read family (get_resource / get_resource_yaml /
// list_resource_names) addresses resources by an explicit GVR
// (group/version/resource_type). But the legacy `kind`-based contract these
// actions replaced — and every existing caller built against it (the UI's
// KubernetesPodYaml.tsx / KubernetesWorkloads.jsx, api-server enrichers) —
// addresses resources by `kind` ("Deployment"), not GVR. Requiring GVR was a
// silent contract regression: a {name, namespace, kind} call fails with
// "version and resource_type are required". This table restores kind-based
// addressing without forcing every caller to learn GVRs.
//
// The agent's MUTATING handlers already accept `kind` via
// mutate.supportedWorkloadKinds; this is the read-side counterpart. The two
// maps are intentionally separate (mutate's carries namespaced/expectedKind
// fields for write semantics and is workload-only). If a third kind→GVR table
// shows up, consolidate then — see CLAUDE.md on not re-encoding the same fact.
//
// Scope: the common built-in kinds (a superset of the legacy enumerated set
// node/deployment/statefulset/daemonset/job/persistentvolume/
// persistentvolumeclaim/service/configmap/networkpolicy) plus the workload
// CRDs the mutate side understands. Generic / arbitrary-CRD reads still use an
// explicit GVR — which always wins over `kind` (see ParseGetParams).
var kindToGVR = map[string]schema.GroupVersionResource{
	// core/v1
	"pod":                   {Group: "", Version: "v1", Resource: "pods"},
	"service":               {Group: "", Version: "v1", Resource: "services"},
	"configmap":             {Group: "", Version: "v1", Resource: "configmaps"},
	"secret":                {Group: "", Version: "v1", Resource: "secrets"},
	"persistentvolumeclaim": {Group: "", Version: "v1", Resource: "persistentvolumeclaims"},
	"persistentvolume":      {Group: "", Version: "v1", Resource: "persistentvolumes"},
	"node":                  {Group: "", Version: "v1", Resource: "nodes"},
	"namespace":             {Group: "", Version: "v1", Resource: "namespaces"},
	"serviceaccount":        {Group: "", Version: "v1", Resource: "serviceaccounts"},
	"endpoints":             {Group: "", Version: "v1", Resource: "endpoints"},
	"event":                 {Group: "", Version: "v1", Resource: "events"},
	"replicationcontroller": {Group: "", Version: "v1", Resource: "replicationcontrollers"},
	"limitrange":            {Group: "", Version: "v1", Resource: "limitranges"},
	"resourcequota":         {Group: "", Version: "v1", Resource: "resourcequotas"},

	// apps/v1
	"deployment":  {Group: "apps", Version: "v1", Resource: "deployments"},
	"daemonset":   {Group: "apps", Version: "v1", Resource: "daemonsets"},
	"statefulset": {Group: "apps", Version: "v1", Resource: "statefulsets"},
	"replicaset":  {Group: "apps", Version: "v1", Resource: "replicasets"},

	// batch/v1
	"job":     {Group: "batch", Version: "v1", Resource: "jobs"},
	"cronjob": {Group: "batch", Version: "v1", Resource: "cronjobs"},

	// networking.k8s.io/v1
	"ingress":       {Group: "networking.k8s.io", Version: "v1", Resource: "ingresses"},
	"ingressclass":  {Group: "networking.k8s.io", Version: "v1", Resource: "ingressclasses"},
	"networkpolicy": {Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"},

	// autoscaling
	"horizontalpodautoscaler": {Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"},

	// policy/v1
	"poddisruptionbudget": {Group: "policy", Version: "v1", Resource: "poddisruptionbudgets"},

	// rbac.authorization.k8s.io/v1
	"role":               {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
	"rolebinding":        {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
	"clusterrole":        {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	"clusterrolebinding": {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},

	// storage.k8s.io/v1
	"storageclass": {Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"},

	// Workload CRDs — kept in sync with mutate.supportedWorkloadKinds so a
	// resource the agent can mutate by kind is also readable by kind.
	"rollout":      {Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"},
	"nodepool":     {Group: "karpenter.sh", Version: "v1", Resource: "nodepools"},
	"ec2nodeclass": {Group: "karpenter.k8s.aws", Version: "v1", Resource: "ec2nodeclasses"},
}

// resolveKind maps a Kind string to its canonical GVR. Matching is
// case-insensitive ("Deployment" and "deployment" both resolve) because the
// UI sends the apiserver's CamelCase Kind while the legacy list_resource_names
// contract used lowercase. Returns ok=false for kinds not in the table —
// callers needing an arbitrary/CRD kind must pass an explicit GVR.
func resolveKind(kind string) (schema.GroupVersionResource, bool) {
	gvr, ok := kindToGVR[strings.ToLower(strings.TrimSpace(kind))]
	return gvr, ok
}
