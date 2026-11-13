package servicemap

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

func ptr[T any](v T) *T { return &v }

func ownedPod(ns, name, ownerKind, ownerName, ip string, ready bool, lbls map[string]string) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbls},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip, HostIP: "192.168.0.1"},
	}
	if ownerKind != "" {
		p.OwnerReferences = []metav1.OwnerReference{{Kind: ownerKind, Name: ownerName, Controller: ptr(true)}}
	}
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}
	return p
}

// testCluster is a shop with a Deployment backend, a StatefulSet postgres that
// only accepts connections, a DaemonSet, a bare pod, a finished Job pod and a
// Service.
func testCluster() []runtime.Object {
	done := ownedPod("shop", "report-1-x", "Job", "report-1", "", false, nil)
	done.Status.Phase = corev1.PodSucceeded
	return []runtime.Object{
		ownedPod("shop", "backend-def-1", "ReplicaSet", "backend-5d8c7b9f6", "10.0.0.2", true, map[string]string{"app.kubernetes.io/name": "backend"}),
		ownedPod("shop", "postgres-0", "StatefulSet", "postgres", "10.0.0.3", false, nil),
		ownedPod("shop", "debug", "", "", "10.0.0.4", true, nil),
		done,
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "backend"}, Spec: appsv1.DeploymentSpec{Replicas: ptr(int32(3))}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "defaulted"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "postgres"}, Spec: appsv1.StatefulSetSpec{Replicas: ptr(int32(1))}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "proxy"}, Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 4}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "backend-lb"}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.10"}},
	}
}

func newTestKubeCache(t *testing.T, objs ...runtime.Object) *KubeCache {
	t.Helper()
	f := informers.NewSharedInformerFactory(fake.NewClientset(objs...), 0)
	k := NewKubeCache(f)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	f.Start(stop)
	f.WaitForCacheSync(stop)
	return k
}

func TestKubeCacheRows(t *testing.T) {
	rows, err := newTestKubeCache(t, testCluster()...).rows()
	if err != nil {
		t.Fatal(err)
	}
	find := func(key, labelKey, name string) map[string]string {
		t.Helper()
		for _, r := range rows[key] {
			if r.Metric[labelKey] == name {
				return r.Metric
			}
		}
		return nil
	}

	if got := find("kube_pod_info", "pod", "backend-def-1"); !reflect.DeepEqual(got, map[string]string{
		"namespace": "shop", "pod": "backend-def-1", "pod_ip": "10.0.0.2", "host_ip": "192.168.0.1",
		"created_by_kind": "ReplicaSet", "created_by_name": "backend-5d8c7b9f6",
	}) {
		t.Errorf("backend kube_pod_info = %v", got)
	}
	if got := find("kube_pod_info", "pod", "debug"); got == nil || got["created_by_name"] != "" {
		t.Errorf("bare pod kube_pod_info = %v; want no owner", got)
	}
	if find("kube_pod_info", "pod", "report-1-x") != nil {
		t.Error("a succeeded pod is in kube_pod_info; the query leaves it out")
	}
	if got := find("kube_pod_labels", "pod", "backend-def-1"); got["label_app_kubernetes_io_name"] != "backend" {
		t.Errorf("backend labels = %v; want label_app_kubernetes_io_name, as kube-state-metrics names it", got)
	}
	for _, r := range rows["kube_pod_status_ready"] {
		want := map[string]float64{"backend-def-1": 1, "postgres-0": 0, "debug": 1, "report-1-x": 0}[r.Metric["pod"]]
		if r.Last != want {
			t.Errorf("%s ready = %v; want %v", r.Metric["pod"], r.Last, want)
		}
	}
	if got := find("kube_service_info", "service", "backend-lb"); got["cluster_ip"] != "10.96.0.10" {
		t.Errorf("service = %v", got)
	}
	replica := func(key, labelKey, name string) float64 {
		for _, r := range rows[key] {
			if r.Metric[labelKey] == name {
				return r.Last
			}
		}
		return -1
	}
	for _, tc := range []struct {
		key, label, name string
		want             float64
	}{
		{"kube_deployment_spec_replicas", "deployment", "backend", 3},
		{"kube_deployment_spec_replicas", "deployment", "defaulted", 1},
		{"kube_statefulset_replicas", "statefulset", "postgres", 1},
		{"kube_daemonset_status_desired_number_scheduled", "daemonset", "proxy", 4},
	} {
		if got := replica(tc.key, tc.label, tc.name); got != tc.want {
			t.Errorf("%s %s = %v; want %v", tc.key, tc.name, got, tc.want)
		}
	}
}

func TestKubeCacheNotSynced(t *testing.T) {
	k := NewKubeCache(informers.NewSharedInformerFactory(fake.NewClientset(), 0)) // never started
	if _, err := k.rows(); !errors.Is(err, errKubeCacheNotSynced) {
		t.Errorf("err = %v; want errKubeCacheNotSynced", err)
	}
}

// fakeProm answers each query with the first response whose key the query
// contains, and records the queries it was sent.
type fakeProm struct {
	mu      sync.Mutex
	queries []string
}

func (f *fakeProm) kubeQueries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, q := range f.queries {
		if strings.Contains(q, "kube_") {
			n++
		}
	}
	return n
}

func newKubeTestService(t *testing.T, responses map[string]string, objs ...runtime.Object) (*Service, *fakeProm) {
	t.Helper()
	fp := &fakeProm{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		fp.mu.Lock()
		fp.queries = append(fp.queries, q)
		fp.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		result := "[]"
		for k, v := range responses {
			if strings.Contains(q, k) {
				result = v
				break
			}
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":` + result + `}}`))
	}))
	t.Cleanup(srv.Close)
	s := New(prometheus.New(srv.URL, &http.Client{Timeout: 5 * time.Second}), "")
	s.Inventory = InventoryKubernetes
	s.Kube = newTestKubeCache(t, objs...)
	return s, fp
}

// Traffic as the node agent reports it: frontend calls backend, backend calls
// postgres. postgres-0 only accepts connections, so it has no pod_workload
// row; it restarted 5 times.
var trafficResponses = map[string]string{
	"rate(container_http_requests_total{":    `[{"metric":{"src_workload_name":"frontend","src_workload_namespace":"shop","destination_workload_name":"backend","destination_workload_namespace":"shop","status":"200"},"value":[1,"7"]}]`,
	"rate(container_postgres_queries_total{": `[{"metric":{"src_workload_kind":"Deployment","src_workload_name":"backend","src_workload_namespace":"shop","destination_workload_kind":"StatefulSet","destination_workload_name":"postgres","destination_workload_namespace":"shop","status":"ok"},"value":[1,"3"]}]`,
	"container_restarts":                     `[{"metric":{"container_id":"/k8s/shop/postgres-0/postgres"},"value":[1,"5"]}]`,
}

func appNamed(apps []Application, kind, name string) *Application {
	for i := range apps {
		if apps[i].ID.Kind == kind && apps[i].ID.Name == name {
			return &apps[i]
		}
	}
	return nil
}

// The inventory comes from the cache: no kube_* query is sent, and the map
// has the pods, readiness and replicas the queries gave.
func TestBuild_KubernetesInventory(t *testing.T) {
	s, fp := newKubeTestService(t, trafficResponses, testCluster()...)

	apps, err := s.Build(context.Background(), FilterParams{Duration: 60})
	if err != nil {
		t.Fatal(err)
	}
	if n := fp.kubeQueries(); n != 0 {
		t.Errorf("kube_* queries sent = %d; want 0", n)
	}
	backend := appNamed(apps, "Deployment", "backend")
	if backend == nil || backend.DesiredInstances != 3 || len(backend.Instances) != 1 || backend.Instances[0].IsFailed {
		t.Fatalf("backend = %+v; want 1 ready instance of 3 desired", backend)
	}
	if backend.Labels["app_kubernetes_io_name"] != "backend" {
		t.Errorf("backend labels = %v", backend.Labels)
	}
	if frontendLink(t, apps).RequestCount != 7 {
		t.Errorf("frontend → backend requests = %v; want 7", frontendLink(t, apps).RequestCount)
	}
}

// A pod that only accepts connections (no pod_workload row) is still listed,
// and its restarts land on its app.
func TestBuild_KubernetesInventoryServerOnlyPod(t *testing.T) {
	s, _ := newKubeTestService(t, trafficResponses, testCluster()...)

	apps, err := s.Build(context.Background(), FilterParams{Duration: 60})
	if err != nil {
		t.Fatal(err)
	}
	pg := appNamed(apps, "StatefulSet", "postgres")
	if pg == nil {
		t.Fatal("postgres is not in the map")
	}
	if pg.Restarts != 5 || len(pg.Instances) != 1 || !pg.Instances[0].IsFailed {
		t.Errorf("postgres = restarts %d, instances %+v; want 5 restarts and one unready instance", pg.Restarts, pg.Instances)
	}
}

// service_map_enricher and traces_dependency_map build the same map, so
// their Finding carries the cache's pods too.
func TestHandlers_EnricherKubernetesInventory(t *testing.T) {
	s, fp := newKubeTestService(t, trafficResponses, testCluster()...)
	for _, action := range []string{"service_map_enricher", "traces_dependency_map"} {
		resp, err := Handlers(s, "acct")[action](context.Background(), map[string]any{"duration": float64(60)})
		if err != nil {
			t.Fatal(action, err)
		}
		b, _ := json.Marshal(resp)
		for _, want := range []string{"backend-def-1", "postgres-0"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("%s response has no %s instance", action, want)
			}
		}
	}
	if n := fp.kubeQueries(); n != 0 {
		t.Errorf("kube_* queries sent = %d; want 0", n)
	}
}

// A filtered request keeps its filter on the traffic queries, as before.
func TestBuild_KubernetesInventoryFiltered(t *testing.T) {
	s, fp := newKubeTestService(t, trafficResponses, testCluster()...)

	if _, err := s.Build(context.Background(), FilterParams{Duration: 60, WorkloadName: "backend", WorkloadNamespace: "shop"}); err != nil {
		t.Fatal(err)
	}
	if n := fp.kubeQueries(); n != 0 {
		t.Errorf("kube_* queries sent = %d; want 0", n)
	}
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if len(fp.queries) != len(withoutKube(ApplicationQueries)) {
		t.Errorf("queries sent = %d; want the %d filtered non-kube queries", len(fp.queries), len(withoutKube(ApplicationQueries)))
	}
}

// The cache holds the cluster as it is now; a window that ended long ago
// takes its inventory from Prometheus, as do a cache not synced yet and the
// prometheus default.
func TestBuild_KubernetesInventoryFallsBackToQueries(t *testing.T) {
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	for _, tc := range []struct {
		name  string
		setup func(*Service)
		p     FilterParams
	}{
		{"window ended an hour ago", func(*Service) {}, FilterParams{Duration: 60, EndTime: old}},
		{"cache not synced", func(s *Service) {
			s.Kube = NewKubeCache(informers.NewSharedInformerFactory(fake.NewClientset(), 0))
		}, FilterParams{Duration: 60}},
		{"prometheus inventory", func(s *Service) { s.Inventory = InventoryPrometheus }, FilterParams{Duration: 60}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fp := newKubeTestService(t, trafficResponses, testCluster()...)
			tc.setup(s)
			if _, err := s.Build(context.Background(), tc.p); err != nil {
				t.Fatal(err)
			}
			if n, want := fp.kubeQueries(), len(kubeKeys(Queries)); n != want {
				t.Errorf("kube_* queries sent = %d; want %d", n, want)
			}
		})
	}
}

// With ClickHouse serving the traffic and the cache the inventory, no
// Prometheus is needed; a window ClickHouse cannot serve is then an error.
func TestBuild_KubernetesInventoryWithoutPrometheus(t *testing.T) {
	end := time.Now().UTC().Truncate(coverageBucket)
	p := FilterParams{EndTime: end.Format(time.RFC3339), Duration: 60}
	for _, tc := range []struct {
		name    string
		ch      *fakeClickHouse
		wantErr bool
	}{
		{"covered", &fakeClickHouse{buckets: fullBuckets(end, 3)}, false},
		{"not covered", &fakeClickHouse{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestService(t, tc.ch, SourceClickHouse)
			s.Prom = nil
			s.Inventory = InventoryKubernetes
			s.Kube = newTestKubeCache(t, testCluster()...)
			s.NodeAgentCount = func(context.Context, time.Time) (int, error) { return 3, nil }

			apps, err := s.Build(context.Background(), p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v; want error %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if b := appNamed(apps, "Deployment", "backend"); b == nil || len(b.Instances) != 1 || b.Restarts != 2 {
				t.Errorf("backend = %+v; want its pod from the cache and 2 restarts from ClickHouse", b)
			}
		})
	}
}

func TestParseInventory(t *testing.T) {
	// The value is trimmed and case-folded, as SERVICE_MAP_SOURCE is.
	for _, tc := range []struct {
		in   string
		want Inventory
	}{
		{"", InventoryPrometheus},
		{"prometheus", InventoryPrometheus},
		{"kubernetes", InventoryKubernetes},
		{" Kubernetes ", InventoryKubernetes},
	} {
		if got, ok := ParseInventory(tc.in); !ok || got != tc.want {
			t.Errorf("ParseInventory(%q) = %q, %v; want %q, true", tc.in, got, ok, tc.want)
		}
	}
	if got, ok := ParseInventory("server"); ok || got != InventoryPrometheus {
		t.Errorf("ParseInventory(server) = %q, %v; want prometheus, false", got, ok)
	}
}

// A cache that never syncs (missing RBAC, say) is reported at most once a
// minute, not on every request.
func TestBuild_KubernetesCacheUnavailableWarnsOnce(t *testing.T) {
	s, _ := newKubeTestService(t, trafficResponses, testCluster()...)
	s.Kube = NewKubeCache(informers.NewSharedInformerFactory(fake.NewClientset(), 0)) // never started
	var buf strings.Builder
	s.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	for range 3 {
		if _, err := s.Build(context.Background(), FilterParams{Duration: 60}); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "kubernetes cache unavailable"); n != 1 {
		t.Errorf("warnings = %d; want 1", n)
	}
}

// Without a Prometheus, an unsynced cache is named as the reason the map
// cannot be drawn.
func TestBuild_KubernetesCacheNotSyncedWithoutPrometheus(t *testing.T) {
	s, _ := newTestService(t, &fakeClickHouse{}, SourceClickHouse)
	s.Prom = nil
	s.Inventory = InventoryKubernetes
	s.Kube = NewKubeCache(informers.NewSharedInformerFactory(fake.NewClientset(), 0))

	_, err := s.Build(context.Background(), FilterParams{Duration: 60})
	if want := "servicemap: kubernetes cache not synced and no prometheus to fall back to"; err == nil || err.Error() != want {
		t.Errorf("err = %v; want %q", err, want)
	}
}
