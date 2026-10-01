package servicemap

import (
	"regexp"
	"strings"
)

// build populates a *world from the parsed Prometheus results. Mirrors the
// loadKubernetesMetadata + loadContainers passes, simplified.
//
// Inputs:
//
//	metrics  : map[query_name][]promResult — one slice per QUERIES key
//
// Output:
//
//	*world ready for renderApplications.
func build(metrics map[string][]promResult) *world {
	w := newWorld()

	// Phase 0 — the eBPF agent's own pod → workload identity. Edges are keyed
	// by it, so pods must be grouped under the same name for an edge's source
	// to find its node.
	podWorkload := podWorkloads(metrics["pod_workload"])

	// Phase 1 — kube_pod_info gives us pods, their IPs and, for pods the eBPF
	// agent has not reported, a fallback identity from the direct owner.
	// kube_pod_info labels include: pod, namespace, host_ip, pod_ip,
	// created_by_kind, created_by_name.
	for _, r := range metrics["kube_pod_info"] {
		l := r.Metric
		ns := labelOr(l, "namespace", "")
		podName := labelOr(l, "pod", "")
		podIP := labelOr(l, "pod_ip", "")

		id, ok := podWorkload[podRef(ns, podName)]
		if !ok {
			id = directOwner(l)
		}
		a := w.upsertApp(id)
		if podName != "" {
			a.instances[podName] = Instance{
				ID:       ApplicationID{Name: podName, Kind: id.Kind, Namespace: ns},
				IsFailed: false,
			}
			w.podApp[podRef(ns, podName)] = appKey(id)
		}
		if podIP != "" {
			w.podIPToApp[podIP] = appKey(id)
		}
	}

	// Phase 2 — kube_pod_labels (decorates apps with labels). It carries the
	// pod but no owner, so the app comes from the pod index; created_by_* is
	// honoured when present.
	for _, r := range metrics["kube_pod_labels"] {
		l := r.Metric
		var a *application
		if k, ok := w.podApp[podRef(labelOr(l, "namespace", ""), labelOr(l, "pod", ""))]; ok {
			a = w.applications[k]
		} else if labelOr(l, "created_by_name", "") != "" {
			a = w.upsertApp(directOwner(l))
		}
		if a == nil {
			continue
		}
		for k, v := range l {
			if matchesLabelPrefix(k) {
				a.labels[stripLabelPrefix(k)] = v
			}
		}
	}

	// Phase 3 — pod readiness → instance failure flag.
	for _, r := range metrics["kube_pod_status_ready"] {
		l := r.Metric
		podName := labelOr(l, "pod", "")
		k, ok := w.podApp[podRef(labelOr(l, "namespace", ""), podName)]
		if !ok {
			continue
		}
		app := w.applications[k]
		inst := app.instances[podName]
		inst.IsFailed = !r.HasVal || r.Last == 0
		app.instances[podName] = inst
	}

	// Phase 4 — Service ClusterIP → backing workload (best effort: services
	// label "selector_*" against pods, but kube_service_info doesn't expose
	// selectors directly. We map service IP to itself as a fallback so the
	// edge still resolves to a node, even if it can't be coalesced into the
	// workload.)
	for _, r := range metrics["kube_service_info"] {
		l := r.Metric
		clusterIP := labelOr(l, "cluster_ip", "")
		svcName := labelOr(l, "service", "")
		ns := labelOr(l, "namespace", "")
		if clusterIP == "" || svcName == "" {
			continue
		}
		id := ApplicationID{Name: svcName, Kind: "Service", Namespace: ns}
		w.upsertApp(id)
		w.serviceIPToApp[clusterIP] = appKey(id)
	}

	// Phase 5 — desired replica counts.
	applyReplicaCount(w, metrics["kube_deployment_spec_replicas"], "Deployment", "deployment")
	applyReplicaCount(w, metrics["kube_statefulset_replicas"], "StatefulSet", "statefulset")
	applyReplicaCount(w, metrics["kube_daemonset_status_desired_number_scheduled"], "DaemonSet", "daemonset")

	// Phase 6 — container resource stats summed per app (needs the pod index
	// from Phase 1).
	addContainerStat(w, metrics["container_oom_kills_total"], func(s *containerSums, v float64) { s.oomKills += v })
	addContainerStat(w, metrics["container_restarts"], func(s *containerSums, v float64) { s.restarts += v })
	addContainerStat(w, metrics["container_throttled_time"], func(s *containerSums, v float64) { s.cpuThrottlingTime += v })
	addContainerStat(w, metrics["container_volume_size"], func(s *containerSums, v float64) { s.volumeSize += v })
	addContainerStat(w, metrics["container_volume_used"], func(s *containerSums, v float64) { s.volumeUsed += v })

	// Phase 7 — edges. Labels: src_workload_kind, src_workload_name,
	// src_workload_namespace, destination_workload_* (or destination_ip).
	// New TCP connections are kept apart from protocol requests: an edge
	// reports requests when the agent decoded its protocol and falls back to
	// the connection rate only when it did not.
	forEachEdge(w, metrics["container_net_tcp_successful_connects"], func(la *linkAccum, r promResult) {
		la.connects += r.Last
	})
	for _, p := range l7Protocols {
		perEdge := map[*linkAccum]float64{}
		forEachEdge(w, metrics[l7RequestsKey(p)], func(la *linkAccum, r promResult) {
			perEdge[la] += r.Last
			if p.failed(labelOr(r.Metric, "status", "")) {
				la.failures += r.Last
			}
		})
		for la, n := range perEdge {
			la.requests += n
			// An edge speaking several protocols is labelled with its
			// busiest one.
			if n > la.protocolRequests {
				la.protocol = p.name
				la.protocolRequests = n
			}
		}
		forEachEdge(w, metrics[l7LatencyKey(p)], func(la *linkAccum, r promResult) {
			// Mean latency of the slowest series on the edge.
			if r.Last > la.latency {
				la.latency = r.Last
			}
		})
	}
	forEachEdge(w, metrics["container_net_tcp_bytes_sent"], func(la *linkAccum, r promResult) {
		la.bytesSent += r.Last
	})
	forEachEdge(w, metrics["container_net_tcp_bytes_received"], func(la *linkAccum, r promResult) {
		la.bytesRecv += r.Last
	})

	return w
}

// forEachEdge hands each series with a value to fn along with its edge.
// Series without one (no sample, or a NaN/Inf sample the parser dropped) are
// skipped before resolving, so they cannot register apps or empty edges.
func forEachEdge(w *world, results []promResult, fn func(la *linkAccum, r promResult)) {
	for _, r := range results {
		if !r.HasVal {
			continue
		}
		if la := edgeFromConnectionLabels(w, r.Metric); la != nil {
			fn(la, r)
		}
	}
}

// edgeFromConnectionLabels resolves the (src_app, dst_app) pair from one
// connection metric's labels. Returns the linkAccum for that edge or nil
// if either end can't be resolved to a known application.
//
// Coroot's eBPF metrics typically expose src/destination via:
//
//	src_workload_kind, src_workload_name, src_workload_namespace
//	destination_workload_kind, destination_workload_name, destination_workload_namespace
//	destination_ip (for traffic to non-K8s endpoints or unmatched workloads)
//
// In practice, `*_workload_kind` is sometimes omitted; we resolve by
// (name, namespace) when kind is missing.
func edgeFromConnectionLabels(w *world, l map[string]string) *linkAccum {
	srcKind := labelOr(l, "src_workload_kind", labelOr(l, "src_kind", ""))
	srcName := labelOr(l, "src_workload_name", "")
	srcNS := labelOr(l, "src_workload_namespace", "")
	if srcName == "" {
		return nil
	}
	if nonWorkloadKinds[srcKind] {
		return nil
	}
	srcK, ok := w.resolveApp(srcKind, srcName, srcNS)
	if !ok {
		// A source with no pod in the window (its pods are gone, or
		// kube-state-metrics does not see them) is still a real caller:
		// register it from the edge labels, as unknown destinations are.
		id := workloadID(srcKind, srcName, srcNS)
		id.Kind = orDefault(id.Kind, "Deployment")
		w.upsertApp(id)
		srcK = appKey(id)
	}

	dstKind := labelOr(l, "destination_workload_kind", "")
	dstName := labelOr(l, "destination_workload_name", "")
	dstNS := labelOr(l, "destination_workload_namespace", "")
	var dstK string
	switch {
	case dstName != "":
		if k, ok := w.resolveApp(dstKind, dstName, dstNS); ok {
			dstK = k
		} else {
			// Workload labelled but not yet known — register it (as a
			// Deployment when the kind is missing, the most common case) so
			// downstream lookups resolve. The name is kept whole: external
			// hostnames like "us-central1-aiplatform.googleapis.com",
			// Services and StatefulSets must not be cut at their last "-".
			id := workloadID(dstKind, dstName, dstNS)
			id.Kind = orDefault(id.Kind, "Deployment")
			w.upsertApp(id)
			dstK = appKey(id)
		}
	default:
		dstK = w.resolveAppByIP(labelOr(l, "destination_ip", ""))
	}
	if dstK == "" || dstK == srcK {
		return nil
	}
	return w.addEdge(srcK, dstK)
}

// resolveApp finds an existing app key by (kind, name, namespace). When
// kind is empty (Coroot eBPF metrics often omit it), falls back to a
// name+namespace search across known apps.
func (w *world) resolveApp(kind, name, namespace string) (string, bool) {
	id := workloadID(kind, name, namespace)
	if id.Kind != "" {
		k := appKey(id)
		if _, ok := w.applications[k]; ok {
			return k, true
		}
	}
	for k, a := range w.applications {
		if a.id.Name == id.Name && a.id.Namespace == namespace {
			return k, true
		}
	}
	return "", false
}

// nonWorkloadKinds are the eBPF agent's sentinel source kinds for traffic
// that does not come from a workload: loopback, host-network processes, and
// addresses outside the cluster.
var nonWorkloadKinds = map[string]bool{"localhost": true, "node": true, "external": true}

// podWorkloads maps each pod to the workload the eBPF agent attributes its
// connections to. The agent climbs owner chains kube_pod_info cannot (Pod →
// Job → CronJob, or custom controllers such as runner scale sets) and
// aggregates bare pods, so its name is the one edges carry.
func podWorkloads(results []promResult) map[string]ApplicationID {
	out := make(map[string]ApplicationID, len(results))
	for _, r := range results {
		l := r.Metric
		ns, pod, ok := podFromContainerID(labelOr(l, "container_id", ""))
		kind := labelOr(l, "src_workload_kind", "")
		name := labelOr(l, "src_workload_name", "")
		if !ok || name == "" || nonWorkloadKinds[kind] {
			continue
		}
		id := workloadID(kind, name, labelOr(l, "src_workload_namespace", ns))
		id.Kind = orDefault(id.Kind, "Deployment")
		out[podRef(ns, pod)] = id
	}
	return out
}

// podFromContainerID splits the eBPF agent's container id,
// /k8s/<namespace>/<pod>/<container>, into namespace and pod.
func podFromContainerID(id string) (namespace, pod string, ok bool) {
	parts := strings.Split(id, "/")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "k8s" || parts[2] == "" || parts[3] == "" {
		return "", "", false
	}
	return parts[2], parts[3], true
}

func podRef(namespace, pod string) string { return namespace + "/" + pod }

// directOwner is a pod's workload as kube_pod_info's created_by_* labels
// describe it: one hop up, with a Deployment's ReplicaSet mapped to the
// Deployment and an unowned pod standing for itself.
func directOwner(l map[string]string) ApplicationID {
	ns := labelOr(l, "namespace", "")
	name := labelOr(l, "created_by_name", "")
	if name == "" {
		// Bare pod (no owner); use the pod name as the application name.
		return ApplicationID{Name: labelOr(l, "pod", ""), Kind: "Pod", Namespace: ns}
	}
	return workloadID(labelOr(l, "created_by_kind", ""), name, ns)
}

// podTemplateHashRe matches the pod-template hash a Deployment appends to
// the names of the ReplicaSets it creates; Kubernetes draws it from this
// alphabet. The eBPF agent's resolver uses the same pattern.
var podTemplateHashRe = regexp.MustCompile(`^[bcdfghjklmnpqrstvwxz2456789]{6,11}$`)

// workloadID names a workload the way the eBPF agent does. A ReplicaSet
// whose name ends in a pod-template hash stands for its Deployment
// ("api-7c9f8bd5d" → Deployment "api"); a ReplicaSet created directly, with
// no such suffix, keeps its own kind and name ("my-app" stays "my-app").
// Every other kind is returned unchanged.
func workloadID(kind, name, namespace string) ApplicationID {
	if kind == "ReplicaSet" {
		if i := strings.LastIndex(name, "-"); i > 0 && podTemplateHashRe.MatchString(name[i+1:]) {
			return ApplicationID{Name: name[:i], Kind: "Deployment", Namespace: namespace}
		}
	}
	return ApplicationID{Name: name, Kind: kind, Namespace: namespace}
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func applyReplicaCount(w *world, results []promResult, kind, labelKey string) {
	for _, r := range results {
		name := labelOr(r.Metric, labelKey, "")
		ns := labelOr(r.Metric, "namespace", "")
		if name == "" {
			continue
		}
		a := w.upsertApp(ApplicationID{Name: name, Kind: kind, Namespace: ns})
		if r.HasVal {
			a.desired = int(r.Last)
		}
	}
}

// addContainerStat sums a per-container series into the app of the
// container's pod. The eBPF agent's container metrics carry no workload
// labels, only the container id, which names the pod.
func addContainerStat(w *world, results []promResult, accumulate func(*containerSums, float64)) {
	for _, r := range results {
		if !r.HasVal {
			continue
		}
		ns, pod, ok := podFromContainerID(labelOr(r.Metric, "container_id", ""))
		if !ok {
			continue
		}
		k, ok := w.podApp[podRef(ns, pod)]
		if !ok {
			continue
		}
		accumulate(w.containerStatsFor(k), r.Last)
	}
}

// matchesLabelPrefix returns true when a Prometheus label name is a pod
// label exposed via kube-state-metrics's `label_*` projection.
func matchesLabelPrefix(name string) bool {
	const prefix = "label_"
	return len(name) > len(prefix) && name[:len(prefix)] == prefix
}

func stripLabelPrefix(name string) string {
	const prefix = "label_"
	return name[len(prefix):]
}
