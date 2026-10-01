package servicemap

import "strings"

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

	// Phase 6 — container resource stats summed per app.
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
	for _, r := range metrics["container_net_tcp_successful_connects"] {
		la := edgeFromConnectionLabels(w, r.Metric)
		if la == nil {
			continue
		}
		la.connects += r.Last
	}
	for _, p := range l7Protocols {
		perEdge := map[*linkAccum]float64{}
		for _, r := range metrics[l7RequestsKey(p)] {
			la := edgeFromConnectionLabels(w, r.Metric)
			if la == nil {
				continue
			}
			perEdge[la] += r.Last
			if p.failed(labelOr(r.Metric, "status", "")) {
				la.failures += r.Last
			}
		}
		for la, n := range perEdge {
			la.requests += n
			// An edge speaking several protocols is labelled with its
			// busiest one.
			if n > la.protocolRequests {
				la.protocol = p.name
				la.protocolRequests = n
			}
		}
		for _, r := range metrics[l7LatencyKey(p)] {
			la := edgeFromConnectionLabels(w, r.Metric)
			if la == nil {
				continue
			}
			// Mean latency of the slowest series on the edge.
			if r.Last > la.latency {
				la.latency = r.Last
			}
		}
	}
	for _, r := range metrics["container_net_tcp_bytes_sent"] {
		la := edgeFromConnectionLabels(w, r.Metric)
		if la == nil {
			continue
		}
		la.bytesSent += r.Last
	}
	for _, r := range metrics["container_net_tcp_bytes_received"] {
		la := edgeFromConnectionLabels(w, r.Metric)
		if la == nil {
			continue
		}
		la.bytesRecv += r.Last
	}

	return w
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
		id := ApplicationID{Name: srcName, Kind: orDefault(normalizeKind(srcKind), "Deployment"), Namespace: srcNS}
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
			// Workload labelled but not yet known — register it as a
			// Deployment (most common case) so downstream lookups resolve.
			// Only a ReplicaSet name carries a hash suffix; every other
			// destination (external hostnames like
			// "us-central1-aiplatform.googleapis.com", Services,
			// StatefulSets) is already the real name and must not be cut
			// at its last "-".
			name := dstName
			if dstKind == "ReplicaSet" {
				name = trimReplicaSetSuffix(dstName)
			}
			id := ApplicationID{
				Name:      name,
				Kind:      orDefault(normalizeKind(dstKind), "Deployment"),
				Namespace: dstNS,
			}
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
	kind = normalizeKind(kind)
	if kind != "" {
		k := appKey(ApplicationID{Name: name, Kind: kind, Namespace: namespace})
		if _, ok := w.applications[k]; ok {
			return k, true
		}
	}
	for k, a := range w.applications {
		if a.id.Name == name && a.id.Namespace == namespace {
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
		out[podRef(ns, pod)] = ApplicationID{
			Name:      name,
			Kind:      orDefault(normalizeKind(kind), "Deployment"),
			Namespace: labelOr(l, "src_workload_namespace", ns),
		}
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
// describe it: one hop up, with a ReplicaSet mapped to its Deployment and an
// unowned pod standing for itself.
func directOwner(l map[string]string) ApplicationID {
	kind := labelOr(l, "created_by_kind", "")
	name := labelOr(l, "created_by_name", "")
	if kind == "ReplicaSet" {
		// ReplicaSet maps back to its Deployment by name prefix — the RS
		// name encodes the Deployment hash.
		kind = "Deployment"
		name = trimReplicaSetSuffix(name)
	}
	if name == "" {
		// Bare pod (no owner); use the pod name as the application name.
		kind = "Pod"
		name = labelOr(l, "pod", "")
	}
	return ApplicationID{Name: name, Kind: kind, Namespace: labelOr(l, "namespace", "")}
}

// normalizeKind collapses "ReplicaSet" → "Deployment" (RS pods belong to a
// Deployment from a topology perspective).
func normalizeKind(k string) string {
	if k == "ReplicaSet" {
		return "Deployment"
	}
	return k
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

func addContainerStat(w *world, results []promResult, accumulate func(*containerSums, float64)) {
	for _, r := range results {
		l := r.Metric
		ownerKind := labelOr(l, "owner_kind", labelOr(l, "workload_kind", ""))
		ownerName := labelOr(l, "owner_name", labelOr(l, "workload_name", ""))
		ns := labelOr(l, "namespace", "")
		if ownerKind == "ReplicaSet" {
			ownerKind = "Deployment"
			ownerName = trimReplicaSetSuffix(ownerName)
		}
		if ownerName == "" || ownerKind == "" {
			continue
		}
		k := appKey(ApplicationID{Name: ownerName, Kind: ownerKind, Namespace: ns})
		if _, ok := w.applications[k]; !ok {
			continue
		}
		s := w.containerStatsFor(k)
		if r.HasVal {
			accumulate(s, r.Last)
		}
	}
}

// trimReplicaSetSuffix strips the typical "-<hash>" suffix Kubernetes
// appends to ReplicaSet names so we can derive the Deployment name. Best-
// effort: assumes the suffix is the last "-<chars>" segment.
func trimReplicaSetSuffix(rsName string) string {
	if rsName == "" {
		return rsName
	}
	// Walk back to the last '-' and assume the suffix is a hash.
	for i := len(rsName) - 1; i > 0; i-- {
		if rsName[i] == '-' {
			return rsName[:i]
		}
	}
	return rsName
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
