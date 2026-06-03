package discovery

import (
	"encoding/json"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// podLookupFn finds one representative Pod for a workload by the workload's
// UID, used by the workload converters to fill the qos_class / ip / conditions
// fields. It is backed by a snapshot-scoped index (built once per snapshot in
// O(pods), see service.go podIndex) so workload conversion is O(1) instead of
// the old O(workloads × pods-per-namespace) selector scan. Returns nil when no
// status-bearing Pod for the workload is in the index — callers treat that as
// "absent" and emit null defaults (self-heals at the next snapshot once a Pod
// reports status).
type podLookupFn func(workloadUID types.UID) *corev1.Pod

// replicaSetLookupFn fetches a ReplicaSet by (namespace, name) out of the
// shared informer cache. Used by the Pod converter to resolve a Pod's
// immediate ReplicaSet owner up to its controlling Deployment by
// reading the ReplicaSet's own ownerReferences — the authoritative
// answer, vs. heuristically stripping a pod-template-hash suffix from
// the RS name. Returns nil when the RS isn't (yet) in cache; callers
// fall back to emitting the RS as-is and rely on the next status event
// to self-heal once the cache syncs.
type replicaSetLookupFn func(namespace, name string) *appsv1.ReplicaSet

// Converters for each resource type. The collector reads keys like
// `service_key`, `node_creation_time`, `workload_count` directly and
// crashes with KeyError when they're missing — emit every documented
// field, even when zero.

// convertDeployment / convertStatefulSet / convertDaemonSet / convertReplicaSet
// / convertJob / convertCronJob all produce the "service" dict shape.

func convertDeployment(obj any) (any, bool) {
	return newDeploymentConverter(nil)(obj)
}

func newDeploymentConverter(lookup podLookupFn) func(any) (any, bool) {
	return func(obj any) (any, bool) {
		d, ok := obj.(*appsv1.Deployment)
		if !ok {
			return nil, false
		}
		replicas := int32(0)
		if d.Spec.Replicas != nil {
			replicas = *d.Spec.Replicas
		}
		return serviceDict("Deployment", d.Name, d.Namespace, d.ObjectMeta,
			replicas, d.Status.ReadyReplicas, d.Spec.Template, d.OwnerReferences,
			lookup), true
	}
}

func convertStatefulSet(obj any) (any, bool) {
	return newStatefulSetConverter(nil)(obj)
}

func newStatefulSetConverter(lookup podLookupFn) func(any) (any, bool) {
	return func(obj any) (any, bool) {
		s, ok := obj.(*appsv1.StatefulSet)
		if !ok {
			return nil, false
		}
		replicas := int32(0)
		if s.Spec.Replicas != nil {
			replicas = *s.Spec.Replicas
		}
		return serviceDict("StatefulSet", s.Name, s.Namespace, s.ObjectMeta,
			replicas, s.Status.ReadyReplicas, s.Spec.Template, s.OwnerReferences,
			lookup), true
	}
}

func convertDaemonSet(obj any) (any, bool) {
	return newDaemonSetConverter(nil)(obj)
}

func newDaemonSetConverter(lookup podLookupFn) func(any) (any, bool) {
	return func(obj any) (any, bool) {
		d, ok := obj.(*appsv1.DaemonSet)
		if !ok {
			return nil, false
		}
		return serviceDict("DaemonSet", d.Name, d.Namespace, d.ObjectMeta,
			d.Status.DesiredNumberScheduled, d.Status.NumberReady, d.Spec.Template, d.OwnerReferences,
			lookup), true
	}
}

func convertReplicaSet(obj any) (any, bool) {
	return newReplicaSetConverter(nil)(obj)
}

func newReplicaSetConverter(lookup podLookupFn) func(any) (any, bool) {
	return func(obj any) (any, bool) {
		r, ok := obj.(*appsv1.ReplicaSet)
		if !ok {
			return nil, false
		}
		replicas := int32(0)
		if r.Spec.Replicas != nil {
			replicas = *r.Spec.Replicas
		}
		// Only emit ReplicaSets with replicas > 0 — the rest are old
		// revisions kept by the owning Deployment.
		if replicas == 0 {
			return nil, false
		}
		return serviceDict("ReplicaSet", r.Name, r.Namespace, r.ObjectMeta,
			replicas, r.Status.ReadyReplicas, r.Spec.Template, r.OwnerReferences,
			lookup), true
	}
}

// convertJob produces the JobInfo wire shape the backend expects.
// Distinct from the service-dict because the collector's job branch reads
// job-specific keys: created_at, updated_at, cpu_req, mem_req, completions,
// status (dict), job_data (dict), service_key.
func convertJob(obj any) (any, bool) {
	j, ok := obj.(*batchv1.Job)
	if !ok {
		return nil, false
	}
	completions := int32(0)
	if j.Spec.Completions != nil {
		completions = *j.Spec.Completions
	}
	cpuReq, memReq := podSpecRequests(j.Spec.Template.Spec)
	return jobDict("Job", j.Name, j.Namespace, j.ObjectMeta,
		completions,
		jobStatusDict(j.Status, nil),
		jobDataDict(j.Spec.Template.Spec, j.Spec.BackoffLimit, j.Labels, j.OwnerReferences, "", nil),
		cpuReq, memReq), true
}

// convertCronJob produces the same JobInfo wire shape but with schedule +
// suspend nested inside job_data. The collector treats CronJob the same way
// as Job (parse_job_discovery — same path).
func convertCronJob(obj any) (any, bool) {
	c, ok := obj.(*batchv1.CronJob)
	if !ok {
		return nil, false
	}
	suspended := c.Spec.Suspend
	cpuReq, memReq := podSpecRequests(c.Spec.JobTemplate.Spec.Template.Spec)
	return jobDict("CronJob", c.Name, c.Namespace, c.ObjectMeta,
		0,
		jobStatusDict(batchv1.JobStatus{}, c.Status.LastScheduleTime),
		jobDataDict(c.Spec.JobTemplate.Spec.Template.Spec, c.Spec.JobTemplate.Spec.BackoffLimit,
			c.Labels, c.OwnerReferences, c.Spec.Schedule, suspended),
		cpuReq, memReq), true
}

// jobDict builds the common JobInfo-shaped dict. service_key, updated_at,
// and created_at are computed inline (not stored on the Pydantic model).
func jobDict(
	kind, name, namespace string,
	meta metav1.ObjectMeta,
	completions int32,
	status, jobData map[string]any,
	cpuReq float64, memReq int64,
) map[string]any {
	return map[string]any{
		"name":        name,
		"namespace":   namespace,
		"type":        kind,
		"service_key": namespace + "/" + kind + "/" + name,
		// RFC3339 (no " UTC" literal) so Postgres' timestamp parser accepts
		// the value at insert time. Same reason as node_creation_time.
		"created_at":       meta.CreationTimestamp.UTC().Format(time.RFC3339),
		"updated_at":       time.Now().UTC().UnixMilli(),
		"deleted":          false,
		"completions":      completions,
		"cpu_req":          cpuReq,
		"mem_req":          memReq,
		"status":           status,
		"job_data":         jobData,
		"resource_version": parseResourceVersion(meta),
		// `config` is read by .get for the labels fallback at line 1437. We
		// emit the same shape services use so existing parsers stay happy.
		"config": map[string]any{"labels": nonNilLabels(meta.Labels)},
	}
}

// jobStatusDict mirrors JobStatus. Times are str() of the timestamp,
// to match the legacy str(getattr(...)) coercion. lastSchedule is only
// populated for CronJob.
func jobStatusDict(s batchv1.JobStatus, lastSchedule *metav1.Time) map[string]any {
	// Time strings use RFC3339 (no " UTC" suffix) so Postgres timestamp
	// parsers accept them at insert time.
	completionTime := ""
	if s.CompletionTime != nil {
		completionTime = s.CompletionTime.UTC().Format(time.RFC3339)
	}
	lastSched := ""
	if lastSchedule != nil {
		lastSched = lastSchedule.UTC().Format(time.RFC3339)
	}
	conditions := make([]map[string]any, 0, len(s.Conditions))
	for _, c := range s.Conditions {
		if string(c.Status) != "True" {
			continue
		}
		conditions = append(conditions, map[string]any{
			"type":    string(c.Type),
			"message": c.Message,
		})
	}
	return map[string]any{
		"active":             s.Active,
		"failed":             s.Failed,
		"succeeded":          s.Succeeded,
		"completion_time":    completionTime,
		"failed_time":        "",
		"conditions":         conditions,
		"last_schedule_time": lastSched,
	}
}

// jobDataDict mirrors JobData. Per-container image +
// requests + limits, plus tolerations / node_selector / labels / pods /
// parents / schedule / suspend.
func jobDataDict(
	pod corev1.PodSpec,
	backoffLimit *int32,
	labels map[string]string,
	owners []metav1.OwnerReference,
	schedule string,
	suspend *bool,
) map[string]any {
	containers := make([]map[string]any, 0, len(pod.Containers))
	for _, c := range pod.Containers {
		req, lim := containerResources(c)
		containers = append(containers, map[string]any{
			"image":     c.Image,
			"cpu_req":   req.cpu,
			"cpu_limit": lim.cpu,
			"mem_req":   req.mem,
			"mem_limit": lim.mem,
		})
	}
	tolerations := make([]map[string]any, 0, len(pod.Tolerations))
	for _, t := range pod.Tolerations {
		tolerations = append(tolerations, map[string]any{
			"key": t.Key, "value": t.Value, "operator": string(t.Operator), "effect": string(t.Effect),
		})
	}
	bl := int32(0)
	if backoffLimit != nil {
		bl = *backoffLimit
	}
	parents := make([]map[string]any, 0, len(owners))
	for _, o := range owners {
		parents = append(parents, map[string]any{"name": o.Name, "kind": o.Kind})
	}
	if pod.NodeSelector == nil {
		pod.NodeSelector = map[string]string{}
	}
	return map[string]any{
		"backoff_limit": bl,
		"tolerations":   tolerations,
		"node_selector": pod.NodeSelector,
		"labels":        nonNilLabels(labels),
		"containers":    containers,
		"pods":          []string{},
		"parents":       parents,
		"schedule":      schedule,
		"suspend":       suspend,
	}
}

// containerResources extracts cpu (cores) + memory (bytes) requests + limits
// from a container. Handles the "no resources set" → zero case quietly.
type resPair struct {
	cpu float64
	mem int64
}

func containerResources(c corev1.Container) (req, lim resPair) {
	if v, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
		req.cpu = float64(v.MilliValue()) / 1000
	}
	if v, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
		req.mem = v.Value()
	}
	if v, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		lim.cpu = float64(v.MilliValue()) / 1000
	}
	if v, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		lim.mem = v.Value()
	}
	return req, lim
}

// podSpecRequests sums request CPU (cores) and memory (bytes) across all
// containers in a pod template — used for cpu_req / mem_req on the JobInfo.
func podSpecRequests(spec corev1.PodSpec) (cpu float64, mem int64) {
	for _, c := range spec.Containers {
		r, _ := containerResources(c)
		cpu += r.cpu
		mem += r.mem
	}
	return cpu, mem
}

// serviceDict produces the service map for any controller-kind.
// `service_key` is computed (not stored on ServiceInfo), `type`
// (not service_type) and `config` (not service_config) are the
// on-the-wire field names, and `update_time` is filled in here so the
// collector can compare against its DB row.
func serviceDict(
	kind, name, namespace string,
	meta metav1.ObjectMeta,
	totalPods, readyPods int32,
	tpl corev1.PodTemplateSpec,
	owners []metav1.OwnerReference,
	lookup podLookupFn,
) map[string]any {
	// Pull qos_class / ip / conditions off a representative running Pod owned
	// by this workload. These three fields are not on the PodTemplateSpec
	// (they're per-instance Status fields), so they require a Pod lookup —
	// resolved O(1) via the snapshot-scoped owner→Pod index keyed by the
	// workload's UID. Stays nil when the lookup is unset (test paths) or no
	// owned Pod has reported status yet.
	var (
		qosClass   any = nil
		ip         any = nil
		conditions     = []map[string]any{}
	)
	if lookup != nil {
		if pod := lookup(meta.UID); pod != nil {
			if pod.Status.QOSClass != "" {
				qosClass = string(pod.Status.QOSClass)
			}
			if pod.Status.PodIP != "" {
				ip = pod.Status.PodIP
			}
			for _, c := range pod.Status.Conditions {
				conditions = append(conditions, map[string]any{
					"type":   string(c.Type),
					"status": string(c.Status),
				})
			}
		}
	}
	return map[string]any{
		"name":             name,
		"namespace":        namespace,
		"type":             kind,
		"service_key":      namespace + "/" + kind + "/" + name,
		"resource_version": parseResourceVersion(meta),
		"creation_time":    meta.CreationTimestamp.UTC().Format(time.RFC3339),
		"update_time":      time.Now().UTC().UnixMilli(),
		"deleted":          false,
		"classification":   "None",
		"total_pods":       totalPods,
		"ready_pods":       readyPods,
		"is_helm_release":  isHelmRelease(meta.Labels, meta.Annotations),
		"status":           nil,
		"node_name":        nil,
		"restart_count":    nil,
		"status_dict":      nil,
		"config": map[string]any{
			// `labels` must be a non-nil map — k8s_workloads.labels is
			// NOT NULL, and Go's nil map serializes as JSON null which
			// fails the insert (collector logs NotNullViolation).
			"labels":     nonNilLabels(meta.Labels),
			"containers": containersFromTemplate(tpl),
			"owner":      ownerInfos(owners),
			// Mirror the legacy ServiceConfig. The UI
			// (KubernetesWorkloads.jsx:2488) reads `volumes.length` without
			// optional chaining, so emitting an empty array — rather than
			// omitting the key — prevents an "undefined.length" crash on
			// the K8s Applications drilldown. Tolerations, affinity,
			// annotations, and service_account are derivable directly
			// from the PodTemplateSpec we already have on hand; qos_class /
			// ip / conditions live on PodStatus and require a separate
			// Pod-driven emission path (out of scope here — default to
			// empty so the keys are present and the UI panels render).
			"volumes":         volumesFromTemplate(tpl),
			"toleration":      tolerationsFromTemplate(tpl),
			"affinity":        affinityFromTemplate(tpl),
			"annotations":     nonNilLabels(meta.Annotations),
			"service_account": tpl.Spec.ServiceAccountName,
			"qos_class":       qosClass,
			"ip":              ip,
			"conditions":      conditions,
		},
	}
}

// volumesFromTemplate mirrors VolumeInfo.get_volume_info
// . Emits `{name, persistent_volume_claim:
// {claim_name}}` for PVC volumes, `{name}` for anything else. Returns
// an empty slice rather than nil so the UI's missing-optional-chain at
// KubernetesWorkloads.jsx:2488 (`.volumes.length > 0`) doesn't crash.
func volumesFromTemplate(tpl corev1.PodTemplateSpec) []map[string]any {
	out := make([]map[string]any, 0, len(tpl.Spec.Volumes))
	for _, v := range tpl.Spec.Volumes {
		entry := map[string]any{"name": v.Name}
		if v.PersistentVolumeClaim != nil {
			entry["persistent_volume_claim"] = map[string]any{
				"claim_name": v.PersistentVolumeClaim.ClaimName,
			}
		}
		out = append(out, entry)
	}
	return out
}

// tolerationsFromTemplate emits the four primary keys the Kubernetes
// Python client serializes for V1Toleration.
func tolerationsFromTemplate(tpl corev1.PodTemplateSpec) []map[string]any {
	out := make([]map[string]any, 0, len(tpl.Spec.Tolerations))
	for _, t := range tpl.Spec.Tolerations {
		entry := map[string]any{
			"key":      t.Key,
			"operator": string(t.Operator),
			"value":    t.Value,
			"effect":   string(t.Effect),
		}
		if t.TolerationSeconds != nil {
			entry["toleration_seconds"] = *t.TolerationSeconds
		}
		out = append(out, entry)
	}
	return out
}

// affinityFromTemplate returns the V1Affinity dict representation, or
// an empty map when unset. The UI doesn't depend on a specific nested
// shape (it renders a JSON view), so a shallow marshal-via-JSON suffices.
func affinityFromTemplate(tpl corev1.PodTemplateSpec) map[string]any {
	if tpl.Spec.Affinity == nil {
		return map[string]any{}
	}
	raw, err := json.Marshal(tpl.Spec.Affinity)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

// nonNilLabels guarantees at least an empty map so consumers' NOT NULL
// label columns don't reject the insert.
func nonNilLabels(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// convertNode produces the NodeInfo wire shape the backend expects.
// Several fields (memory_allocated, cpu_allocated, pods, pods_count,
// memory_limits, cpu_limits) require cross-resource aggregation (sum over
// pods scheduled on the node). The Go discovery isn't pod-aware yet, so they
// emit zeros — the collector accepts numeric zeros, just shows zero
// allocation in the UI. Phase-4 will compute these from the pod informer.
func convertNode(obj any) (any, bool) {
	n, ok := obj.(*corev1.Node)
	if !ok {
		return nil, false
	}
	internal, external := nodeAddresses(n)
	return map[string]any{
		"name": n.Name,
		// Postgres' timestamp parser rejects the literal " UTC" suffix that
		// Go's time.Time.String() appends ("2026-05-07 00:43:41 +0000 UTC"
		// → InvalidDatetimeFormat at insert). RFC3339 ("2026-05-07T00:43:41Z")
		// parses cleanly and matches str(creation_timestamp) output from
		// the Python kubernetes client (no UTC suffix).
		"node_creation_time": n.CreationTimestamp.UTC().Format(time.RFC3339),
		// `updated_at` is read directly by the collector via
		// datetime.fromtimestamp(...)/1000 — must be a unix-millis number,
		// set fresh per snapshot.
		"updated_at":         time.Now().UTC().UnixMilli(),
		"internal_ip":        internal,
		"external_ip":        external,
		"taints":             taintString(n.Spec.Taints),
		"conditions":         conditionString(n.Status.Conditions),
		"memory_capacity":    mbFromResourceList(n.Status.Capacity, corev1.ResourceMemory),
		"memory_allocatable": mbFromResourceList(n.Status.Allocatable, corev1.ResourceMemory),
		"memory_allocated":   0,
		"memory_limits":      0,
		"cpu_capacity":       cpuFromResourceList(n.Status.Capacity),
		"cpu_allocatable":    cpuFromResourceList(n.Status.Allocatable),
		"cpu_allocated":      0.0,
		"cpu_limits":         0.0,
		"pods_count":         0,
		"pods":               "",
		"node_info":          nodeInfoMap(n),
		"deleted":            false,
		"resource_version":   parseResourceVersion(n.ObjectMeta),
	}, true
}

// convertNamespace adds `workload_count` + `pod_count` (zeros until we sum
// across the service+pod caches) so the collector's
// `k8s_data["workload_count"]` access doesn't KeyError.
func convertNamespace(obj any) (any, bool) {
	n, ok := obj.(*corev1.Namespace)
	if !ok {
		return nil, false
	}
	return map[string]any{
		"name":           n.Name,
		"workload_count": 0,
		"pod_count":      0,
		"creation_time":  n.CreationTimestamp.UTC().Format(time.RFC3339),
		// `updated_at` must be a unix-millis number — required by the
		// collector via direct index whenever it touches namespace rows.
		"updated_at":       time.Now().UTC().UnixMilli(),
		"deleted":          false,
		"resource_version": parseResourceVersion(n.ObjectMeta),
		"phase":            string(n.Status.Phase),
		"labels":           nonNilLabels(n.Labels),
		"annotations":      nonNilLabels(n.Annotations),
	}, true
}

// nodeAddresses splits node.status.addresses into the comma-joined
// internal_ip / external_ip strings.
func nodeAddresses(n *corev1.Node) (internal, external string) {
	var ints, exts []string
	for _, addr := range n.Status.Addresses {
		switch strings.ToLower(string(addr.Type)) {
		case "internalip":
			ints = append(ints, addr.Address)
		case "externalip":
			exts = append(exts, addr.Address)
		}
	}
	return strings.Join(ints, ","), strings.Join(exts, ",")
}

// taintString joins taints as "key=value:effect,…".
func taintString(taints []corev1.Taint) string {
	parts := make([]string, 0, len(taints))
	for _, t := range taints {
		parts = append(parts, t.Key+"="+t.Value+":"+string(t.Effect))
	}
	return strings.Join(parts, ",")
}

// conditionString emits "<type>:<status>" for any node condition that
// isn't a quiet false, plus always emits Ready regardless of status.
func conditionString(conds []corev1.NodeCondition) string {
	parts := make([]string, 0, len(conds))
	for _, c := range conds {
		if string(c.Status) == "False" && c.Type != corev1.NodeReady {
			continue
		}
		parts = append(parts, string(c.Type)+":"+string(c.Status))
	}
	return strings.Join(parts, ",")
}

// mbFromResourceList parses a ResourceList memory entry and returns its size
// in MB. The collector stores the integer directly.
func mbFromResourceList(rl corev1.ResourceList, key corev1.ResourceName) int64 {
	q, ok := rl[key]
	if !ok {
		return 0
	}
	bytes, ok := q.AsInt64()
	if !ok {
		bytes = q.Value()
	}
	return bytes / (1024 * 1024)
}

func cpuFromResourceList(rl corev1.ResourceList) float64 {
	q, ok := rl[corev1.ResourceCPU]
	if !ok {
		return 0
	}
	// Convert to cores. ResourceList CPU is typically in cores ("4") or
	// millicores ("4000m"); MilliValue gives us a uniform integer.
	mv := q.MilliValue()
	return float64(mv) / 1000
}

// nodeInfoMap wraps V1NodeSystemInfo plus labels/annotations/addresses
// in a dict the collector consumes.
func nodeInfoMap(n *corev1.Node) map[string]any {
	addrs := make([]string, 0, len(n.Status.Addresses))
	for _, a := range n.Status.Addresses {
		addrs = append(addrs, a.Address)
	}
	return map[string]any{
		"system": map[string]any{
			"architecture":              n.Status.NodeInfo.Architecture,
			"container_runtime_version": n.Status.NodeInfo.ContainerRuntimeVersion,
			"kernel_version":            n.Status.NodeInfo.KernelVersion,
			"kubelet_version":           n.Status.NodeInfo.KubeletVersion,
			"os_image":                  n.Status.NodeInfo.OSImage,
			"operating_system":          n.Status.NodeInfo.OperatingSystem,
			"machine_id":                n.Status.NodeInfo.MachineID,
			"system_uuid":               n.Status.NodeInfo.SystemUUID,
			"boot_id":                   n.Status.NodeInfo.BootID,
		},
		"labels":      nonNilLabels(n.Labels),
		"annotations": nonNilLabels(n.Annotations),
		"addresses":   addrs,
	}
}

// containersFromTemplate, ownerInfos, isHelmRelease, parseResourceVersion are
// shared helpers — kept below so the converters above are the authoritative
// list of fields the wire format carries.

func containersFromTemplate(tpl corev1.PodTemplateSpec) []map[string]any {
	out := make([]map[string]any, 0, len(tpl.Spec.Containers))
	for _, c := range tpl.Spec.Containers {
		out = append(out, map[string]any{
			"name":  c.Name,
			"image": c.Image,
		})
	}
	return out
}

func ownerInfos(owners []metav1.OwnerReference) []map[string]any {
	return ownerInfosWithRSLookup(owners, "", nil)
}

// ownerInfosWithRSLookup is the Pod-path variant: when an owner ref is a
// ReplicaSet and the RS is in the informer cache, the RS's own
// controller ownerReference (typically a Deployment) is emitted
// instead. This is the authoritative ReplicaSet→Deployment resolution
// — no name-suffix heuristics. When rsLookup is nil or the RS isn't in
// cache, falls back to passing the owner ref through unchanged; the
// next pod-status event self-heals once the RS cache syncs.
//
// `namespace` is the namespace of the owned object (OwnerReference is
// always same-namespace per K8s rules), used to scope the RS lookup.
func ownerInfosWithRSLookup(owners []metav1.OwnerReference, namespace string, rsLookup replicaSetLookupFn) []map[string]any {
	out := make([]map[string]any, 0, len(owners))
	for _, o := range owners {
		if rsLookup != nil && o.Kind == "ReplicaSet" {
			if rs := rsLookup(namespace, o.Name); rs != nil {
				if ctrl, ok := controllerOwner(rs.OwnerReferences); ok {
					out = append(out, map[string]any{"kind": ctrl.Kind, "name": ctrl.Name})
					continue
				}
			}
		}
		out = append(out, map[string]any{"kind": o.Kind, "name": o.Name})
	}
	return out
}

// controllerOwner returns the controller=true OwnerReference, or the
// first ref when none is explicitly marked (pre-1.16 manifests / some
// operators omit the field). The bool is false when refs is empty.
func controllerOwner(refs []metav1.OwnerReference) (metav1.OwnerReference, bool) {
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller {
			return ref, true
		}
	}
	if len(refs) > 0 {
		return refs[0], true
	}
	return metav1.OwnerReference{}, false
}

// isHelmRelease checks the standard label/annotation conventions.
func isHelmRelease(labels, annotations map[string]string) bool {
	if labels["app.kubernetes.io/managed-by"] == "Helm" {
		return true
	}
	for k := range labels {
		if hasPrefix(k, "helm.") || hasPrefix(k, "meta.helm.") {
			return true
		}
	}
	for k := range annotations {
		if hasPrefix(k, "helm.") || hasPrefix(k, "meta.helm.") {
			return true
		}
	}
	return false
}

func hasPrefix(s, p string) bool {
	if len(s) < len(p) {
		return false
	}
	return s[:len(p)] == p
}

// parseResourceVersion is defined in service.go alongside other informer
// helpers and reused here.
