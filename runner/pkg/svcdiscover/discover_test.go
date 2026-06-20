package svcdiscover

import (
	"context"
	"testing"

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

// TestFindFirstPreferPort_PicksAPIPortOverUI seeds an OpenCost Service whose
// first port is the UI (9090) and second is the cost-model API (9003, /healthz).
// Without a preference the first port wins and /healthz is probed on the UI port
// — reporting a healthy OpenCost as down. With prefer 9003 the API port wins.
func TestFindFirstPreferPort_PicksAPIPortOverUI(t *testing.T) {
	cs := fake.NewClientset(
		mkServicePorts("opencost", "opencost", map[string]string{"app": "opencost"}, 9090, 9003),
	)
	d := New(cs, "cluster.local")

	// Default behaviour: first port (UI) — the bug.
	if got, want := d.FindFirst(context.Background(), OpencostSelectors),
		"http://opencost.opencost.svc.cluster.local:9090"; got != want {
		t.Errorf("FindFirst got %q; want %q", got, want)
	}
	// With preference the cost-model API port wins.
	if got, want := d.FindFirstPreferPort(context.Background(), OpencostSelectors, 9003),
		"http://opencost.opencost.svc.cluster.local:9003"; got != want {
		t.Errorf("FindFirstPreferPort got %q; want %q", got, want)
	}
}

// TestFindFirstPreferPort_FallsBackToFirstPort verifies that when none of the
// preferred ports are exposed, selection falls back to the Service's first port.
func TestFindFirstPreferPort_FallsBackToFirstPort(t *testing.T) {
	cs := fake.NewClientset(
		mkServicePorts("opencost", "opencost", map[string]string{"app": "opencost"}, 9003),
	)
	d := New(cs, "cluster.local")
	if got, want := d.FindFirstPreferPort(context.Background(), OpencostSelectors, 12345),
		"http://opencost.opencost.svc.cluster.local:9003"; got != want {
		t.Errorf("got %q; want %q", got, want)
	}
}

func TestSelectPort(t *testing.T) {
	ports := func(ns ...int32) []corev1.ServicePort {
		out := make([]corev1.ServicePort, len(ns))
		for i, n := range ns {
			out[i] = corev1.ServicePort{Port: n}
		}
		return out
	}
	cases := []struct {
		name      string
		ports     []corev1.ServicePort
		preferred []int32
		want      int32
	}{
		{"empty preference uses first", ports(9090, 9003), nil, 9090},
		{"preferred present", ports(9090, 9003), []int32{9003}, 9003},
		{"preference order honoured", ports(9090, 9003), []int32{9003, 9090}, 9003},
		{"preferred absent falls back", ports(9090, 9003), []int32{8080}, 9090},
		{"second preference matches", ports(9090, 9003), []int32{8080, 9090}, 9090},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectPort(tc.ports, tc.preferred); got != tc.want {
				t.Errorf("selectPort = %d; want %d", got, tc.want)
			}
		})
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

func mkServicePorts(name, namespace string, labels map[string]string, ports ...int32) *corev1.Service {
	sp := make([]corev1.ServicePort, len(ports))
	for i, p := range ports {
		sp[i] = corev1.ServicePort{Port: p}
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec:       corev1.ServiceSpec{Ports: sp},
	}
}
