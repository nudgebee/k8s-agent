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
				"created_by_kind": "ReplicaSet", "created_by_name": "frontend-abc",
			}, 1, true),
			metric(map[string]string{
				"pod": "backend-def-1", "namespace": "shop", "pod_ip": "10.0.0.2",
				"created_by_kind": "ReplicaSet", "created_by_name": "backend-def",
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
		"container_http_requests_count": {
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

func TestBuild_LabelsExtractedFromPodLabels(t *testing.T) {
	metrics := map[string][]promResult{
		"kube_pod_info": {
			metric(map[string]string{
				"pod": "frontend-a", "namespace": "shop", "pod_ip": "10.0.0.1",
				"created_by_kind": "ReplicaSet", "created_by_name": "frontend-abc",
			}, 1, true),
		},
		"kube_pod_labels": {
			metric(map[string]string{
				"namespace":       "shop",
				"created_by_kind": "ReplicaSet", "created_by_name": "frontend-abc",
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
				"created_by_kind": "ReplicaSet", "created_by_name": "frontend-abc",
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

func TestBuild_ContainerStatsAccumulate(t *testing.T) {
	metrics := map[string][]promResult{
		"kube_pod_info": {
			metric(map[string]string{
				"pod": "frontend-a", "namespace": "shop", "pod_ip": "10.0.0.1",
				"created_by_kind": "ReplicaSet", "created_by_name": "frontend-abc",
			}, 1, true),
		},
		"container_oom_kills_total": {
			metric(map[string]string{
				"workload_kind": "Deployment", "workload_name": "frontend", "namespace": "shop",
			}, 3, true),
			metric(map[string]string{
				"workload_kind": "Deployment", "workload_name": "frontend", "namespace": "shop",
			}, 2, true),
		},
		"container_restarts": {
			metric(map[string]string{
				"workload_kind": "Deployment", "workload_name": "frontend", "namespace": "shop",
			}, 5, true),
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

func TestTrimReplicaSetSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"frontend-abc123", "frontend"},
		{"my-app", "my"},
		{"single", "single"},
		{"", ""},
	}
	for _, c := range cases {
		if got := trimReplicaSetSuffix(c.in); got != c.want {
			t.Errorf("trimReplicaSetSuffix(%q) = %q; want %q", c.in, got, c.want)
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
