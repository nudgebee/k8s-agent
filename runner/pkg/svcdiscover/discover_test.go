package svcdiscover

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestFindFirst_FirstMatchingSelectorWins seeds two services that match
// different selectors in PrometheusSelectors. The first selector with a hit
// takes precedence.
func TestFindFirst_FirstMatchingSelectorWins(t *testing.T) {
	cs := fake.NewClientset(
		// Matches the second selector in PrometheusSelectors.
		mkService("prom-server", "monitoring", 9090, map[string]string{"app": "prometheus-server"}),
		// Matches the first selector — should win.
		mkService("prom-stack", "kps", 9090, map[string]string{"app": "kube-prometheus-stack-prometheus"}),
	)
	d := New(cs, "cluster.local")
	url := d.FindFirst(context.Background(), PrometheusSelectors)
	want := "http://prom-stack.kps.svc.cluster.local:9090"
	if url != want {
		t.Errorf("got %q; want %q", url, want)
	}
}

func TestFindFirst_NoMatchReturnsEmpty(t *testing.T) {
	cs := fake.NewClientset()
	d := New(cs, "cluster.local")
	if url := d.FindFirst(context.Background(), PrometheusSelectors); url != "" {
		t.Errorf("expected empty URL; got %q", url)
	}
}

// TestFindFirst_CachesNegativeResults asserts that a miss isn't re-listed on
// every call — misses are cached for an hour to avoid pummeling the API
// server. We verify by pre-empting the cache and noting the next call should
// return cached miss even if a service is added.
func TestFindFirst_CachesResults(t *testing.T) {
	cs := fake.NewClientset()
	d := New(cs, "cluster.local")
	if u := d.FindFirst(context.Background(), PrometheusSelectors); u != "" {
		t.Fatalf("first call should miss: got %q", u)
	}
	// Add a service AFTER the cache is populated. With a fresh cache, this
	// would be discoverable — but we expect the cache to still report empty.
	if _, err := cs.CoreV1().Services("default").Create(context.Background(),
		mkService("p", "default", 9090, map[string]string{"app": "kube-prometheus-stack-prometheus"}),
		metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if u := d.FindFirst(context.Background(), PrometheusSelectors); u != "" {
		t.Errorf("expected cached miss; got %q", u)
	}
}

func TestNew_DefaultsClusterDomain(t *testing.T) {
	cs := fake.NewClientset(
		mkService("p", "ns", 9090, map[string]string{"app": "loki"}),
	)
	d := New(cs, "")
	if u := d.FindFirst(context.Background(), LokiSelectors); u == "" {
		t.Fatal("expected URL")
	}
}

func TestCoalesce(t *testing.T) {
	if got := Coalesce("", " ", "real", "later"); got != "real" {
		t.Errorf("Coalesce = %q", got)
	}
	if got := Coalesce(""); got != "" {
		t.Errorf("Coalesce all-empty = %q; want empty", got)
	}
}

// TestFindFirst_PrometheusCommunityChart covers the prometheus-community/prometheus
// chart >= 25.x, which labels the server svc with app.kubernetes.io/name=prometheus
// and app.kubernetes.io/component=server (no legacy `app=` label).
func TestFindFirst_PrometheusCommunityChart(t *testing.T) {
	cs := fake.NewClientset(
		mkService("prometheus-server", "prometheus", 80, map[string]string{
			"app.kubernetes.io/name":      "prometheus",
			"app.kubernetes.io/component": "server",
			"app.kubernetes.io/instance":  "prometheus",
		}),
	)
	d := New(cs, "cluster.local")
	want := "http://prometheus-server.prometheus.svc.cluster.local:80"
	if got := d.FindFirst(context.Background(), PrometheusSelectors); got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestNilDiscovererReturnsEmpty(t *testing.T) {
	var d *Discoverer
	if u := d.FindFirst(context.Background(), PrometheusSelectors); u != "" {
		t.Errorf("nil Discoverer returned %q", u)
	}
}

func mkService(name, namespace string, port int32, labels map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{Port: port}},
		},
	}
}

// A Prometheus that appears after the agent started must still be found.
// Boot-time discovery ran exactly once, so an agent that came up before its
// Prometheus stayed blind to it until the pod was restarted.
func TestWatchUntilFound_PicksUpAServiceAddedLater(t *testing.T) {
	cs := fake.NewClientset()
	found := make(chan string, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go WatchUntilFound(ctx, cs, "cluster.local", PrometheusSelectors, 20*time.Millisecond, func(u string) {
		found <- u
	})

	// Nothing to find yet.
	select {
	case u := <-found:
		t.Fatalf("reported %q before any Prometheus existed", u)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := cs.CoreV1().Services("kps").Create(ctx,
		mkService("prom-stack", "kps", 9090, map[string]string{"app": "kube-prometheus-stack-prometheus"}),
		metav1.CreateOptions{}); err != nil {
		t.Fatalf("create service: %v", err)
	}

	select {
	case u := <-found:
		if want := "http://prom-stack.kps.svc.cluster.local:9090"; u != want {
			t.Errorf("got %q; want %q", u, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a Prometheus added after the watch started was never found")
	}
}

// The negative-result cache in FindFirst is an hour wide. The watcher must not
// inherit it across attempts, or the first miss would freeze the retry for the
// rest of the process's life — the bug this watcher exists to prevent.
func TestWatchUntilFound_DoesNotInheritTheNegativeCache(t *testing.T) {
	cs := fake.NewClientset()
	// Prime a Discoverer's cache with a miss, the way boot-time discovery does.
	if u := New(cs, "cluster.local").FindFirst(context.Background(), PrometheusSelectors); u != "" {
		t.Fatalf("expected no match on an empty cluster; got %q", u)
	}
	if _, err := cs.CoreV1().Services("kps").Create(context.Background(),
		mkService("prom-stack", "kps", 9090, map[string]string{"app": "kube-prometheus-stack-prometheus"}),
		metav1.CreateOptions{}); err != nil {
		t.Fatalf("create service: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	found := make(chan string, 1)
	go WatchUntilFound(ctx, cs, "cluster.local", PrometheusSelectors, 20*time.Millisecond, func(u string) { found <- u })

	select {
	case <-found:
	case <-time.After(1 * time.Second):
		t.Fatal("watcher never found a Prometheus that was present the whole time")
	}
}
