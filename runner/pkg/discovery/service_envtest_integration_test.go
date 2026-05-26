//go:build integration

package discovery

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Real-apiserver test: starts envtest (etcd + kube-apiserver), runs the
// discovery Service against it, creates Pods through the typed clientset,
// and asserts the sink receives correctly-shaped envelopes.
//
// envtest is the Go-native equivalent for testing K8s informers/controllers
// — it boots real kube-apiserver + etcd binaries, no Docker required.
//
// Prereq: KUBEBUILDER_ASSETS env points at a directory containing
// `kube-apiserver`, `etcd`, `kubectl`. The Makefile's `make test-integration`
// target wires this up via `setup-envtest`.
//
// Build tag `integration` keeps this out of the default `go test ./...` run.
func TestService_RealAPIServer_Pod_Lifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; -short")
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest.Start: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Logf("envtest.Stop: %v", err)
		}
	})

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}

	// Sink that captures all incoming envelopes.
	var (
		mu        sync.Mutex
		envelopes []Envelope
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env Envelope
		_ = json.Unmarshal(body, &env)
		mu.Lock()
		envelopes = append(envelopes, env)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	sink := NewSink(srv.URL, "secret", "acc-it", "cluster-it", slog.Default())

	// Pre-create one Pod before discovery starts so we exercise the
	// initial-snapshot path (factory caches sync, then we walk the indexer).
	preExisting := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pre-existing", Namespace: "default", Labels: map[string]string{"role": "pre"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "nginx:1.27"}},
		},
	}
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), preExisting, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pre-existing pod: %v", err)
	}

	svc := NewService(cs, sink, time.Hour, slog.Default())
	svc.RegisterAll() // Pod + Deployment + StatefulSet + DaemonSet + Node + Namespace

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	// 1) Wait for initial full-load snapshot containing pre-existing.
	waitFor(t, &mu, &envelopes, 5*time.Second, func(envs []Envelope) bool {
		for _, e := range envs {
			if e.FullLoad && e.IsFirstBatch && hasPodNamed(e, "pre-existing") {
				return true
			}
		}
		return false
	}, "initial snapshot containing pre-existing pod")

	// 2) Create a NEW Pod after discovery is running; informer event handler
	//    should enqueue it and the worker should post an incremental envelope
	//    (no batch metadata, FullLoad=false).
	mu.Lock()
	envelopes = nil // reset so the wait below isn't satisfied by snapshot
	mu.Unlock()

	added := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "added-by-test", Namespace: "default", Labels: map[string]string{"role": "added"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "busybox:1.36"}},
		},
	}
	if _, err := cs.CoreV1().Pods("default").Create(context.Background(), added, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create added pod: %v", err)
	}

	waitFor(t, &mu, &envelopes, 10*time.Second, func(envs []Envelope) bool {
		for _, e := range envs {
			if !e.FullLoad && hasPodNamed(e, "added-by-test") {
				return true
			}
		}
		return false
	}, "incremental envelope for added-by-test")

	// 3) Verify converter shape on the live envelope.
	mu.Lock()
	defer mu.Unlock()
	for _, e := range envelopes {
		if hasPodNamed(e, "added-by-test") {
			item := findPodItem(e, "added-by-test")
			if item["service_type"] != "Pod" {
				t.Errorf("service_type = %v; want Pod", item["service_type"])
			}
			if item["namespace"] != "default" {
				t.Errorf("namespace = %v", item["namespace"])
			}
			cfg, _ := item["service_config"].(map[string]any)
			labels, _ := cfg["labels"].(map[string]any)
			if labels["role"] != "added" {
				t.Errorf("labels.role = %v; want added", labels["role"])
			}
			containers, _ := cfg["containers"].([]any)
			if len(containers) != 1 {
				t.Errorf("containers = %d; want 1", len(containers))
			}
			return
		}
	}
	t.Fatal("converter shape never asserted (envelope not found post-loop)")
}

func hasPodNamed(e Envelope, name string) bool {
	for _, raw := range e.Data {
		m, _ := raw.(map[string]any)
		if m != nil && m["name"] == name {
			return true
		}
	}
	return false
}

func findPodItem(e Envelope, name string) map[string]any {
	for _, raw := range e.Data {
		m, _ := raw.(map[string]any)
		if m != nil && m["name"] == name {
			return m
		}
	}
	return nil
}

// waitFor polls until pred returns true or times out. Re-evaluates against
// a snapshot of the current envelopes slice on every tick.
func waitFor(t *testing.T, mu *sync.Mutex, envs *[]Envelope, timeout time.Duration, pred func([]Envelope) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mu.Lock()
		snap := make([]Envelope, len(*envs))
		copy(snap, *envs)
		mu.Unlock()
		if pred(snap) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", what)
}
