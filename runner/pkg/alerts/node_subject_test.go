package alerts

import (
	"context"
	"testing"
)

// stubLocator records what it was asked and answers from fixed maps.
type stubLocator struct {
	podToNode map[string]string
	ipToNode  map[string]string
	podCalls  int
	ipCalls   int
}

func (s *stubLocator) NodeForPod(_ context.Context, namespace, podName string) string {
	s.podCalls++
	return s.podToNode[namespace+"/"+podName]
}

func (s *stubLocator) NodeForIP(_ context.Context, ip string) string {
	s.ipCalls++
	return s.ipToNode[ip]
}

func TestIsNodeScopedExporterJob(t *testing.T) {
	cases := map[string]bool{
		"node-exporter":                     true,
		"node_exporter":                     true,
		"victoria-prometheus-node-exporter": true,
		"Node-Exporter":                     true,
		"kube-state-metrics":                false,
		"kubelet":                           false,
		"checkout-api":                      false,
		"":                                  false,
	}
	for job, want := range cases {
		if got := isNodeScopedExporterJob(job); got != want {
			t.Errorf("isNodeScopedExporterJob(%q) = %v, want %v", job, got, want)
		}
	}
}

func TestHostFromInstance(t *testing.T) {
	cases := map[string]string{
		"10.128.0.14:9100": "10.128.0.14",
		"10.128.0.14":      "10.128.0.14",
		"[fe80::1]:9100":   "fe80::1",
		"fe80::1":          "fe80::1",
		"node-1.local:80":  "node-1.local",
		"":                 "",
	}
	for in, want := range cases {
		if got := hostFromInstance(in); got != want {
			t.Errorf("hostFromInstance(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestResolveNodeSubject uses the label shapes these alerts actually carry:
// every node alert observed on dev has job=node-exporter plus the exporter's
// own pod/namespace, and no node label.
func TestResolveNodeSubject(t *testing.T) {
	tests := []struct {
		name         string
		labels       map[string]string
		locator      *stubLocator
		want         string
		wantPodCalls int
		wantIPCalls  int
	}{
		{
			name: "node label short-circuits, no lookup",
			labels: map[string]string{
				"job": "node-exporter", "node": "gke-pool-abc",
				"pod": "node-exporter-skqnn", "namespace": "monitoring",
			},
			locator: &stubLocator{},
			want:    "gke-pool-abc",
		},
		{
			name: "resolves through the exporter pod",
			labels: map[string]string{
				"job": "node-exporter", "pod": "victoria-prometheus-node-exporter-skqnn",
				"namespace": "monitoring", "instance": "10.128.0.14:9100",
			},
			locator: &stubLocator{
				podToNode: map[string]string{"monitoring/victoria-prometheus-node-exporter-skqnn": "gke-pool-abc"},
			},
			want:         "gke-pool-abc",
			wantPodCalls: 1,
		},
		{
			name: "falls back to the instance address",
			labels: map[string]string{
				"job": "node-exporter", "instance": "10.128.0.14:9100",
			},
			locator:     &stubLocator{ipToNode: map[string]string{"10.128.0.14": "gke-pool-abc"}},
			want:        "gke-pool-abc",
			wantIPCalls: 1,
		},
		{
			name: "IPv6 instance is split correctly",
			labels: map[string]string{
				"job": "node-exporter", "instance": "[fe80::1]:9100",
			},
			locator:     &stubLocator{ipToNode: map[string]string{"fe80::1": "node-v6"}},
			want:        "node-v6",
			wantIPCalls: 1,
		},
		{
			name: "kube-state-metrics alert is left alone — the pod is the real subject",
			labels: map[string]string{
				"job": "kube-state-metrics", "pod": "victoria-prometheus-node-exporter-skqnn",
				"namespace": "monitoring",
			},
			locator: &stubLocator{
				podToNode: map[string]string{"monitoring/victoria-prometheus-node-exporter-skqnn": "gke-pool-abc"},
			},
			want: "",
		},
		{
			name: "application alert is left alone",
			labels: map[string]string{
				"job": "checkout-api", "pod": "checkout-api-7d9f", "namespace": "shop",
			},
			locator: &stubLocator{},
			want:    "",
		},
		{
			name:   "unresolvable node returns empty rather than a guess",
			labels: map[string]string{"job": "node-exporter", "instance": "10.128.0.99:9100"},
			locator: &stubLocator{
				ipToNode: map[string]string{"10.128.0.14": "gke-pool-abc"},
			},
			want:        "",
			wantIPCalls: 1,
		},
		{
			name:   "no labels",
			labels: nil,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var locator NodeLocator
			if tt.locator != nil {
				locator = tt.locator
			}
			got := resolveNodeSubject(context.Background(), tt.labels, locator)
			if got != tt.want {
				t.Errorf("resolveNodeSubject() = %q, want %q", got, tt.want)
			}
			if tt.locator != nil {
				if tt.locator.podCalls != tt.wantPodCalls {
					t.Errorf("pod lookups = %d, want %d", tt.locator.podCalls, tt.wantPodCalls)
				}
				if tt.locator.ipCalls != tt.wantIPCalls {
					t.Errorf("ip lookups = %d, want %d", tt.locator.ipCalls, tt.wantIPCalls)
				}
			}
		})
	}
}

// TestResolveNodeSubjectWithoutLocator — an agent that booted without a typed
// k8s client must still correct what it can from labels, and must not panic
// reaching for a resolver it does not have.
func TestResolveNodeSubjectWithoutLocator(t *testing.T) {
	labels := map[string]string{"job": "node-exporter", "node": "gke-pool-abc"}
	if got := resolveNodeSubject(context.Background(), labels, nil); got != "gke-pool-abc" {
		t.Errorf("with node label = %q, want gke-pool-abc", got)
	}

	labels = map[string]string{"job": "node-exporter", "pod": "node-exporter-skqnn", "namespace": "monitoring"}
	if got := resolveNodeSubject(context.Background(), labels, nil); got != "" {
		t.Errorf("without a locator = %q, want empty", got)
	}
}
