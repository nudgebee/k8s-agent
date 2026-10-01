package servicemap

import (
	"testing"
)

// metric is a tiny helper to construct a promResult.
func metric(labels map[string]string, last float64, has bool) promResult {
	return promResult{Metric: labels, Last: last, HasVal: has}
}

// TestBuild_SingleEdge wires up two pods owned by two Deployments and a
// connection between them. Verifies the world has both apps + an edge.
func TestBuild_SingleEdge(t *testing.T) {
	metrics := map[string][]promResult{
		"kube_pod_info": {
			metric(map[string]string{
				"pod": "frontend-abc-1", "namespace": "shop", "pod_ip": "10.0.0.1",
				"created_by_kind": "ReplicaSet", "created_by_name": "frontend-6f7d9c8b4",
			}, 1, true),
			metric(map[string]string{
				"pod": "backend-def-1", "namespace": "shop", "pod_ip": "10.0.0.2",
				"created_by_kind": "ReplicaSet", "created_by_name": "backend-5d8c7b9f6",
			}, 1, true),
		},
		"container_net_tcp_successful_connects": {
			metric(map[string]string{
				"src_workload_kind":              "Deployment",
				"src_workload_name":              "frontend",
				"src_workload_namespace":         "shop",
				"destination_workload_kind":      "Deployment",
				"destination_workload_name":      "backend",
				"destination_workload_namespace": "shop",
			}, 5.0, true),
		},
		"l7_requests:HTTP": {
			metric(map[string]string{
				"src_workload_name":              "frontend",
				"src_workload_namespace":         "shop",
				"destination_workload_name":      "backend",
				"destination_workload_namespace": "shop",
			}, 100.0, true),
		},
	}

	w := build(metrics)
	if len(w.applications) < 2 {
		t.Fatalf("expected ≥2 apps, got %d: %v", len(w.applications), keysOf(w.applications))
	}
	if _, ok := w.applications[appKey(ApplicationID{Name: "frontend", Kind: "Deployment", Namespace: "shop"})]; !ok {
		t.Error("frontend app missing")
	}
	if _, ok := w.applications[appKey(ApplicationID{Name: "backend", Kind: "Deployment", Namespace: "shop"})]; !ok {
		t.Error("backend app missing")
	}
	dsts, ok := w.edges[appKey(ApplicationID{Name: "frontend", Kind: "Deployment", Namespace: "shop"})]
	if !ok || len(dsts) != 1 {
		t.Fatalf("expected one edge from frontend, got %v", dsts)
	}
	for _, la := range dsts {
		if la.protocol != "HTTP" {
			t.Errorf("protocol = %q; want HTTP", la.protocol)
		}
		if la.requests <= 0 {
			t.Errorf("requests = %v; want >0", la.requests)
		}
	}
}

func TestBuild_BarePod(t *testing.T) {
	// Pod with no owner — falls back to Pod kind with the pod name as app name.
	metrics := map[string][]promResult{
		"kube_pod_info": {
			metric(map[string]string{
				"pod":       "lonely-pod",
				"namespace": "n",
				"pod_ip":    "10.0.0.5",
			}, 1, true),
		},
	}
	w := build(metrics)
	if _, ok := w.applications[appKey(ApplicationID{Name: "lonely-pod", Kind: "Pod", Namespace: "n"})]; !ok {
		t.Errorf("bare pod app missing: %v", keysOf(w.applications))
	}
}

// A pod owned by a ReplicaSet created directly belongs to that ReplicaSet,
// not to a Deployment named after a truncated prefix.
func TestBuild_BareReplicaSetOwner(t *testing.T) {
	w := build(map[string][]promResult{
		"kube_pod_info": {
			metric(map[string]string{
				"pod": "batch-worker-x2k9p", "namespace": "n", "pod_ip": "10.0.0.9",
				"created_by_kind": "ReplicaSet", "created_by_name": "batch-worker",
			}, 1, true),
		},
	})
	if _, ok := w.applications[appKey(ApplicationID{Name: "batch-worker", Kind: "ReplicaSet", Namespace: "n"})]; !ok {
		t.Errorf("bare ReplicaSet app missing: %v", keysOf(w.applications))
	}
}

func TestBuild_LabelsExtractedFromPodLabels(t *testing.T) {
	metrics := map[string][]promResult{
		"kube_pod_info": {
			metric(map[string]string{
				"pod": "frontend-a", "namespace": "shop", "pod_ip": "10.0.0.1",
				"created_by_kind": "ReplicaSet", "created_by_name": "frontend-6f7d9c8b4",
			}, 1, true),
		},
		"kube_pod_labels": {
			metric(map[string]string{
				"namespace":       "shop",
				"created_by_kind": "ReplicaSet", "created_by_name": "frontend-6f7d9c8b4",
				"label_app":   "frontend",
				"label_env":   "prod",
				"non_label_x": "ignore-me",
			}, 1, true),
		},
	}
	w := build(metrics)
	a := w.applications[appKey(ApplicationID{Name: "frontend", Kind: "Deployment", Namespace: "shop"})]
	if a == nil {
		t.Fatal("frontend app missing")
	}
	if a.labels["app"] != "frontend" || a.labels["env"] != "prod" {
		t.Errorf("labels = %v", a.labels)
	}
	if _, ok := a.labels["non_label_x"]; ok {
		t.Errorf("non-label-prefixed key should be skipped")
	}
}

func TestBuild_FailedInstance(t *testing.T) {
	metrics := map[string][]promResult{
		"kube_pod_info": {
			metric(map[string]string{
				"pod": "frontend-a", "namespace": "shop", "pod_ip": "10.0.0.1",
				"created_by_kind": "ReplicaSet", "created_by_name": "frontend-6f7d9c8b4",
			}, 1, true),
		},
		"kube_pod_status_ready": {
			// status_ready=0 → failed
			metric(map[string]string{"pod": "frontend-a", "namespace": "shop"}, 0, true),
		},
	}
	w := build(metrics)
	a := w.applications[appKey(ApplicationID{Name: "frontend", Kind: "Deployment", Namespace: "shop"})]
	if a == nil {
		t.Fatal("app missing")
	}
	if !a.instances["frontend-a"].IsFailed {
		t.Error("frontend-a should be failed when ready=0")
	}
}

// The eBPF agent's container metrics name only the container (and through
// it the pod); stats reach the app via the pod index.
func TestBuild_ContainerStatsAccumulate(t *testing.T) {
	stat := func(container string, v float64) promResult {
		return metric(map[string]string{"container_id": "/k8s/shop/" + container}, v, true)
	}
	metrics := map[string][]promResult{
		"kube_pod_info": {
			metric(map[string]string{
				"pod": "frontend-abc-1", "namespace": "shop", "pod_ip": "10.0.0.1",
				"created_by_kind": "ReplicaSet", "created_by_name": "frontend-6f7d9c8b4",
			}, 1, true),
			metric(map[string]string{
				"pod": "frontend-abc-2", "namespace": "shop", "pod_ip": "10.0.0.2",
				"created_by_kind": "ReplicaSet", "created_by_name": "frontend-6f7d9c8b4",
			}, 1, true),
		},
		"container_oom_kills_total": {
			stat("frontend-abc-1/app", 3),
			stat("frontend-abc-2/app", 2),
			// a pod that is not in kube_pod_info is skipped
			stat("gone-1/app", 7),
		},
		"container_restarts": {
			stat("frontend-abc-1/sidecar", 5),
			// not a pod container id
			metric(map[string]string{"container_id": "/system.slice/kubelet.service"}, 9, true),
		},
	}
	w := build(metrics)
	k := appKey(ApplicationID{Name: "frontend", Kind: "Deployment", Namespace: "shop"})
	s := w.containerStats[k]
	if s == nil {
		t.Fatal("container stats missing")
	}
	if s.oomKills != 5 || s.restarts != 5 {
		t.Errorf("oom=%v restarts=%v; want 5 5", s.oomKills, s.restarts)
	}
	if len(w.containerStats) != 1 {
		t.Errorf("stats attached to unexpected apps: %v", w.containerStats)
	}
}

func TestBuild_ServiceClusterIPMappedToService(t *testing.T) {
	metrics := map[string][]promResult{
		"kube_service_info": {
			metric(map[string]string{
				"service": "frontend-svc", "namespace": "shop", "cluster_ip": "10.96.0.5",
			}, 1, true),
		},
	}
	w := build(metrics)
	got := w.serviceIPToApp["10.96.0.5"]
	want := appKey(ApplicationID{Name: "frontend-svc", Kind: "Service", Namespace: "shop"})
	if got != want {
		t.Errorf("serviceIP map: got=%q want=%q", got, want)
	}
}

// TestBuild_UnknownDestinationNameKeptWhole covers destinations that
// kube_pod_info never registered. Only a ReplicaSet name ending in a
// pod-template hash may lose its suffix; external hostnames, other kinds and
// a ReplicaSet created directly must keep the full name, or distinct
// workloads collapse into one node (e.g. every
// "us-central1-*.googleapis.com" endpoint becoming "us-central1").
func TestBuild_UnknownDestinationNameKeptWhole(t *testing.T) {
	conn := func(kind, name, ns string) promResult {
		return metric(map[string]string{
			"src_workload_kind":              "Deployment",
			"src_workload_name":              "frontend",
			"src_workload_namespace":         "shop",
			"destination_workload_kind":      kind,
			"destination_workload_name":      name,
			"destination_workload_namespace": ns,
		}, 1.0, true)
	}
	metrics := map[string][]promResult{
		"kube_pod_info": {
			metric(map[string]string{
				"pod": "frontend-abc-1", "namespace": "shop", "pod_ip": "10.0.0.1",
				"created_by_kind": "ReplicaSet", "created_by_name": "frontend-6f7d9c8b4",
			}, 1, true),
		},
		"container_net_tcp_successful_connects": {
			conn("external", "us-central1-aiplatform.googleapis.com", "external"),
			conn("external", "us-central1-artifactregistry.googleapis.com", "external"),
			conn("external", "kms.us-east-1.amazonaws.com", "external"),
			conn("Service", "redis-master", "shop"),
			conn("ReplicaSet", "api-7c9f8bd5d", "shop"),
			conn("ReplicaSet", "batch-worker", "shop"),
		},
	}

	w := build(metrics)

	want := []ApplicationID{
		{Name: "us-central1-aiplatform.googleapis.com", Kind: "external", Namespace: "external"},
		{Name: "us-central1-artifactregistry.googleapis.com", Kind: "external", Namespace: "external"},
		{Name: "kms.us-east-1.amazonaws.com", Kind: "external", Namespace: "external"},
		{Name: "redis-master", Kind: "Service", Namespace: "shop"},
		{Name: "api", Kind: "Deployment", Namespace: "shop"},
		{Name: "batch-worker", Kind: "ReplicaSet", Namespace: "shop"},
	}
	for _, id := range want {
		if _, ok := w.applications[appKey(id)]; !ok {
			t.Errorf("app %s missing; have %v", appKey(id), keysOf(w.applications))
		}
	}
	for _, truncated := range []ApplicationID{
		{Name: "us-central1", Kind: "external", Namespace: "external"},
		{Name: "kms.us-east", Kind: "external", Namespace: "external"},
		{Name: "redis", Kind: "Service", Namespace: "shop"},
		{Name: "batch", Kind: "Deployment", Namespace: "shop"},
	} {
		if _, ok := w.applications[appKey(truncated)]; ok {
			t.Errorf("truncated app %s should not exist", appKey(truncated))
		}
	}
	src := appKey(ApplicationID{Name: "frontend", Kind: "Deployment", Namespace: "shop"})
	if got := len(w.edges[src]); got != len(want) {
		t.Errorf("edges from frontend = %d; want %d", got, len(want))
	}
}

// TestBuild_SourceGroupedByEBPFIdentity covers pods whose direct owner is
// not the workload the eBPF agent reports: a runner pod owned by a per-job
// custom resource that the agent attributes to its long-lived scale set.
// The pod must join the scale set's node so the edge (keyed by the scale
// set) keeps its source instead of being dropped.
func TestBuild_SourceGroupedByEBPFIdentity(t *testing.T) {
	metrics := map[string][]promResult{
		"pod_workload": {
			metric(map[string]string{
				"container_id":      "/k8s/ci/runner-x7k2p/runner",
				"src_workload_kind": "AutoscalingRunnerSet", "src_workload_name": "linux-runners", "src_workload_namespace": "ci",
			}, 1, true),
		},
		"kube_pod_info": {
			metric(map[string]string{
				"pod": "runner-x7k2p", "namespace": "ci", "pod_ip": "10.0.0.7",
				"created_by_kind": "EphemeralRunner", "created_by_name": "runner-x7k2p",
			}, 1, true),
		},
		"kube_pod_labels": {
			metric(map[string]string{"pod": "runner-x7k2p", "namespace": "ci", "label_team": "platform"}, 1, true),
		},
		"kube_pod_status_ready": {
			metric(map[string]string{"pod": "runner-x7k2p", "namespace": "ci"}, 1, true),
		},
		"l7_requests:HTTP": {
			metric(map[string]string{
				"src_workload_kind": "AutoscalingRunnerSet", "src_workload_name": "linux-runners", "src_workload_namespace": "ci",
				"destination_workload_kind": "external", "destination_workload_name": "api.example.com", "destination_workload_namespace": "external",
			}, 2.5, true),
		},
	}
	w := build(metrics)

	setK := appKey(ApplicationID{Name: "linux-runners", Kind: "AutoscalingRunnerSet", Namespace: "ci"})
	set := w.applications[setK]
	if set == nil {
		t.Fatalf("scale-set app missing; have %v", keysOf(w.applications))
	}
	if _, ok := set.instances["runner-x7k2p"]; !ok {
		t.Errorf("runner pod should be an instance of the scale set: %+v", set.instances)
	}
	if set.labels["team"] != "platform" {
		t.Errorf("pod labels should attach via the pod: %v", set.labels)
	}
	if _, ok := w.applications[appKey(ApplicationID{Name: "runner-x7k2p", Kind: "EphemeralRunner", Namespace: "ci"})]; ok {
		t.Error("per-job owner should not become its own app when the eBPF identity is known")
	}
	if w.podIPToApp["10.0.0.7"] != setK {
		t.Errorf("pod IP should resolve to the scale set, got %q", w.podIPToApp["10.0.0.7"])
	}
	dst := appKey(ApplicationID{Name: "api.example.com", Kind: "external", Namespace: "external"})
	la := w.edges[setK][dst]
	if la == nil || la.requests != 2.5 || la.protocol != "HTTP" {
		t.Errorf("edge from scale set = %+v; want 2.5 HTTP requests", la)
	}
}

// A caller with no pod in the window is still a real source: register it
// from the edge labels instead of dropping its traffic.
func TestBuild_UnknownSourceRegistered(t *testing.T) {
	metrics := map[string][]promResult{
		"l7_requests:HTTP": {
			metric(map[string]string{
				"src_workload_kind": "OpenTelemetryCollector", "src_workload_name": "collector", "src_workload_namespace": "obs",
				"destination_workload_kind": "Deployment", "destination_workload_name": "backend", "destination_workload_namespace": "shop",
			}, 4, true),
		},
	}
	w := build(metrics)
	src := appKey(ApplicationID{Name: "collector", Kind: "OpenTelemetryCollector", Namespace: "obs"})
	dst := appKey(ApplicationID{Name: "backend", Kind: "Deployment", Namespace: "shop"})
	if la := w.edges[src][dst]; la == nil || la.requests != 4 {
		t.Errorf("edge collector→backend = %+v; want 4 requests; apps=%v", la, keysOf(w.applications))
	}
}

// Unknown sources are named the way the eBPF agent names them: a
// ReplicaSet created directly keeps its name; a hash-suffixed one (as an
// older agent could report) joins its Deployment.
func TestBuild_UnknownReplicaSetSourceNamed(t *testing.T) {
	conn := func(name string) promResult {
		return metric(map[string]string{
			"src_workload_kind": "ReplicaSet", "src_workload_name": name, "src_workload_namespace": "shop",
			"destination_workload_kind": "Deployment", "destination_workload_name": "backend", "destination_workload_namespace": "shop",
		}, 1, true)
	}
	w := build(map[string][]promResult{"container_net_tcp_successful_connects": {conn("batch-worker"), conn("api-7c9f8bd5d")}})
	for _, id := range []ApplicationID{
		{Name: "batch-worker", Kind: "ReplicaSet", Namespace: "shop"},
		{Name: "api", Kind: "Deployment", Namespace: "shop"},
	} {
		if len(w.edges[appKey(id)]) != 1 {
			t.Errorf("source %s should have one edge; apps=%v", appKey(id), keysOf(w.applications))
		}
	}
	for _, id := range []ApplicationID{
		{Name: "batch", Kind: "Deployment", Namespace: "shop"},
		{Name: "api-7c9f8bd5d", Kind: "Deployment", Namespace: "shop"},
	} {
		if _, ok := w.applications[appKey(id)]; ok {
			t.Errorf("app %s should not exist", appKey(id))
		}
	}
}

// A series with no finite value (the parser drops NaN/Inf samples) must not
// register its endpoints or an empty edge.
func TestBuild_SeriesWithoutValueIgnored(t *testing.T) {
	l := map[string]string{
		"src_workload_kind": "Deployment", "src_workload_name": "api", "src_workload_namespace": "shop",
		"destination_workload_kind": "Deployment", "destination_workload_name": "db", "destination_workload_namespace": "shop",
	}
	w := build(map[string][]promResult{
		"container_net_tcp_successful_connects": {metric(l, 0, false)},
		"l7_requests:HTTP":                      {metric(l, 0, false)},
		"l7_latency:HTTP":                       {metric(l, 0, false)},
		"container_net_tcp_bytes_sent":          {metric(l, 0, false)},
		"container_net_tcp_bytes_received":      {metric(l, 0, false)},
	})
	if len(w.edges) != 0 || len(w.applications) != 0 {
		t.Errorf("valueless series created apps %v / edges %v", keysOf(w.applications), w.edges)
	}
}

func TestBuild_NonWorkloadSourcesDropped(t *testing.T) {
	var conns []promResult
	for _, kind := range []string{"localhost", "node", "external"} {
		conns = append(conns, metric(map[string]string{
			"src_workload_kind": kind, "src_workload_name": "x", "src_workload_namespace": kind,
			"destination_workload_kind": "Deployment", "destination_workload_name": "backend", "destination_workload_namespace": "shop",
		}, 1, true))
	}
	w := build(map[string][]promResult{"container_net_tcp_successful_connects": conns})
	if len(w.edges) != 0 {
		t.Errorf("non-workload sources should not produce edges: %v", w.edges)
	}
}

func TestPodWorkloads(t *testing.T) {
	got := podWorkloads([]promResult{
		metric(map[string]string{"container_id": "/k8s/shop/api-7c9f8-abcde/app", "src_workload_kind": "Deployment", "src_workload_name": "api", "src_workload_namespace": "shop"}, 1, true),
		// host-network traffic is attributed to the node, not the pod's workload
		metric(map[string]string{"container_id": "/k8s/kube-system/proxy-1/proxy", "src_workload_kind": "node", "src_workload_name": "node-1", "src_workload_namespace": "node"}, 1, true),
		// not a pod container id
		metric(map[string]string{"container_id": "/system.slice/containerd.service", "src_workload_kind": "Deployment", "src_workload_name": "x"}, 1, true),
		metric(map[string]string{"container_id": "/k8s-cronjob/shop/report/app", "src_workload_kind": "CronJob", "src_workload_name": "report", "src_workload_namespace": "shop"}, 1, true),
	})
	want := map[string]ApplicationID{"shop/api-7c9f8-abcde": {Name: "api", Kind: "Deployment", Namespace: "shop"}}
	if len(got) != len(want) || got["shop/api-7c9f8-abcde"] != want["shop/api-7c9f8-abcde"] {
		t.Errorf("podWorkloads = %v; want %v", got, want)
	}
}

// An edge reports decoded protocol requests when there are any, and new TCP
// connections only when the agent decoded nothing; the two are never added.
// Failures come from the protocol's own status values.
func TestBuild_RequestRateAndProtocol(t *testing.T) {
	edge := func(dst string, extra map[string]string) map[string]string {
		l := map[string]string{
			"src_workload_kind": "Deployment", "src_workload_name": "api", "src_workload_namespace": "shop",
			"destination_workload_kind": "Deployment", "destination_workload_name": dst, "destination_workload_namespace": "shop",
		}
		for k, v := range extra {
			l[k] = v
		}
		return l
	}
	metrics := map[string][]promResult{
		"container_net_tcp_successful_connects": {
			metric(edge("db", nil), 0.5, true),
			metric(edge("legacy", nil), 0.2, true),
		},
		"l7_requests:Postgres": {
			metric(edge("db", map[string]string{"status": "ok"}), 30, true),
			metric(edge("db", map[string]string{"status": "failed"}), 2, true),
		},
		"l7_latency:Postgres": {
			metric(edge("db", nil), 0.004, true),
		},
		// a little HTTP on the same edge: still labelled with the busier protocol
		"l7_requests:HTTP": {
			metric(edge("db", map[string]string{"status": "500"}), 1, true),
		},
	}
	w := build(metrics)
	src := appKey(ApplicationID{Name: "api", Kind: "Deployment", Namespace: "shop"})

	db := w.edges[src][appKey(ApplicationID{Name: "db", Kind: "Deployment", Namespace: "shop"})]
	if db == nil {
		t.Fatal("api→db edge missing")
	}
	if db.protocol != "Postgres" || db.requestRate() != 33 || db.failures != 3 || db.latency != 0.004 {
		t.Errorf("api→db = protocol %q rate %v failures %v latency %v; want Postgres 33 3 0.004",
			db.protocol, db.requestRate(), db.failures, db.latency)
	}

	legacy := w.edges[src][appKey(ApplicationID{Name: "legacy", Kind: "Deployment", Namespace: "shop"})]
	if legacy == nil {
		t.Fatal("api→legacy edge missing")
	}
	if legacy.protocol != "" || legacy.requestRate() != 0.2 {
		t.Errorf("api→legacy = protocol %q rate %v; want undecoded edge at its connection rate 0.2", legacy.protocol, legacy.requestRate())
	}
}

func TestWorkloadID(t *testing.T) {
	cases := []struct {
		kind, name string
		want       ApplicationID
	}{
		// a Deployment's ReplicaSet stands for the Deployment
		{"ReplicaSet", "frontend-7c9f8bd5d", ApplicationID{Name: "frontend", Kind: "Deployment", Namespace: "n"}},
		{"ReplicaSet", "foo-2456789-7d9f8bcdfg", ApplicationID{Name: "foo-2456789", Kind: "Deployment", Namespace: "n"}},
		// a ReplicaSet created directly keeps its name and kind
		{"ReplicaSet", "my-app", ApplicationID{Name: "my-app", Kind: "ReplicaSet", Namespace: "n"}},
		{"ReplicaSet", "single", ApplicationID{Name: "single", Kind: "ReplicaSet", Namespace: "n"}},
		// other kinds are untouched, hash-like suffix or not
		{"StatefulSet", "db-7c9f8bd5d", ApplicationID{Name: "db-7c9f8bd5d", Kind: "StatefulSet", Namespace: "n"}},
		{"", "api", ApplicationID{Name: "api", Kind: "", Namespace: "n"}},
	}
	for _, c := range cases {
		if got := workloadID(c.kind, c.name, "n"); got != c.want {
			t.Errorf("workloadID(%q, %q) = %+v; want %+v", c.kind, c.name, got, c.want)
		}
	}
}

func keysOf(m map[string]*application) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
