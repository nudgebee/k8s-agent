package servicemap

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

	// Phase 1 — kube_pod_info gives us pod → workload identity, IP → app.
	// kube_pod_info labels include: pod, namespace, host_ip, pod_ip,
	// created_by_kind, created_by_name (the workload).
	for _, r := range metrics["kube_pod_info"] {
		l := r.Metric
		ownerKind := labelOr(l, "created_by_kind", "")
		ownerName := labelOr(l, "created_by_name", "")
		if ownerKind == "ReplicaSet" {
			// ReplicaSet maps back to its Deployment by name prefix — the
			// RS name encodes the Deployment hash.
			ownerKind = "Deployment"
			ownerName = trimReplicaSetSuffix(ownerName)
		}
		ns := labelOr(l, "namespace", "")
		podName := labelOr(l, "pod", "")
		podIP := labelOr(l, "pod_ip", "")

		if ownerName == "" {
			// Bare pod (no owner); use the pod name as the application name.
			ownerKind = "Pod"
			ownerName = podName
		}
		id := ApplicationID{Name: ownerName, Kind: ownerKind, Namespace: ns}
		a := w.upsertApp(id)
		if podName != "" {
			a.instances[podName] = Instance{
				ID:       ApplicationID{Name: podName, Kind: ownerKind, Namespace: ns},
				IsFailed: false,
			}
		}
		if podIP != "" {
			w.podIPToApp[podIP] = appKey(id)
		}
	}

	// Phase 2 — kube_pod_labels (decorates apps with labels).
	for _, r := range metrics["kube_pod_labels"] {
		l := r.Metric
		ownerKind := labelOr(l, "created_by_kind", "")
		ownerName := labelOr(l, "created_by_name", "")
		if ownerKind == "ReplicaSet" {
			ownerKind = "Deployment"
			ownerName = trimReplicaSetSuffix(ownerName)
		}
		ns := labelOr(l, "namespace", "")
		if ownerName == "" {
			continue
		}
		a := w.upsertApp(ApplicationID{Name: ownerName, Kind: ownerKind, Namespace: ns})
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
		ns := labelOr(l, "namespace", "")
		if podName == "" {
			continue
		}
		// Find the app this pod belongs to (any instance match).
		for _, app := range w.applications {
			if inst, ok := app.instances[podName]; ok && app.id.Namespace == ns {
				inst.IsFailed = !r.HasVal || r.Last == 0
				app.instances[podName] = inst
				break
			}
		}
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

	// Phase 7 — connection edges from container_net_tcp_successful_connects.
	// Labels: src_workload_kind, src_workload_name, src_workload_namespace,
	//         destination_ip (or destination_workload_*).
	for _, r := range metrics["container_net_tcp_successful_connects"] {
		la := edgeFromConnectionLabels(w, r.Metric)
		if la == nil {
			continue
		}
		la.hasRequests = true
		la.requests += r.Last
	}
	for _, r := range metrics["container_http_requests_count"] {
		la := edgeFromConnectionLabels(w, r.Metric)
		if la == nil {
			continue
		}
		la.requests += r.Last
		la.protocol = "HTTP"
	}
	for _, r := range metrics["container_http_requests_failure_count"] {
		la := edgeFromConnectionLabels(w, r.Metric)
		if la == nil {
			continue
		}
		la.failures += r.Last
	}
	for _, r := range metrics["container_http_requests_latency"] {
		la := edgeFromConnectionLabels(w, r.Metric)
		if la == nil {
			continue
		}
		// Use max observed latency (best-effort "most-recent" projection
		// per connection).
		if r.Last > la.latency {
			la.latency = r.Last
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
	srcK, ok := w.resolveApp(srcKind, srcName, srcNS)
	if !ok {
		return nil
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
			id := ApplicationID{
				Name:      trimReplicaSetSuffix(dstName),
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
