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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	fake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// rolloutScheme registers the Argo Rollout GVKs as unstructured so the
// dynamic fake can serve list/watch for them (mirrors mutate's workloadScheme).
func rolloutScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	gv := rolloutGVR.GroupVersion()
	s.AddKnownTypeWithName(gv.WithKind("Rollout"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(gv.WithKind("RolloutList"), &unstructured.UnstructuredList{})
	return s
}

// rolloutFixture builds an inline-template Rollout. Numeric fields must be
// int64 — unstructured.NestedInt64 is strict about the concrete type.
func rolloutFixture(name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Rollout",
		"metadata": map[string]any{
			"name":              name,
			"namespace":         ns,
			"uid":               "ro-uid-1",
			"resourceVersion":   "42",
			"creationTimestamp": "2026-01-01T00:00:00Z",
			"labels":            map[string]any{"app": name},
		},
		"spec": map[string]any{
			"replicas": int64(3),
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": "web", "image": "nginx:1.27"},
					},
				},
			},
		},
		"status": map[string]any{"readyReplicas": int64(2)},
	}}
}

// argoServedResources advertises argoproj.io/v1alpha1 on the fake discovery
// client so RegisterRollouts' CRD gate passes.
func argoServedResources() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{{
		GroupVersion: "argoproj.io/v1alpha1",
		APIResources: []metav1.APIResource{{Name: "rollouts", Kind: "Rollout", Namespaced: true}},
	}}
}

func TestRolloutConverter_InlineTemplate(t *testing.T) {
	item, ok := newRolloutConverter(nil)(rolloutFixture("checkout", "shop"))
	if !ok {
		t.Fatal("converter returned ok=false")
	}
	m := item.(map[string]any)
	if m["name"] != "checkout" || m["namespace"] != "shop" || m["type"] != "Rollout" {
		t.Errorf("wire shape: %+v", m)
	}
	if m["service_key"] != "shop/Rollout/checkout" {
		t.Errorf("service_key = %v", m["service_key"])
	}
	if m["total_pods"] != int32(3) {
		t.Errorf("total_pods = %v; want 3", m["total_pods"])
	}
	if m["ready_pods"] != int32(2) {
		t.Errorf("ready_pods = %v; want 2", m["ready_pods"])
	}
	cfg := m["config"].(map[string]any)
	containers := cfg["containers"].([]map[string]any)
	if len(containers) != 1 || containers[0]["image"] != "nginx:1.27" {
		t.Errorf("containers = %+v", containers)
	}
}

func TestRolloutConverter_WorkloadRefNoTemplate(t *testing.T) {
	u := rolloutFixture("checkout", "shop")
	unstructured.RemoveNestedField(u.Object, "spec", "template")
	_ = unstructured.SetNestedMap(u.Object, map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment", "name": "checkout",
	}, "spec", "workloadRef")

	item, ok := newRolloutConverter(nil)(u)
	if !ok {
		t.Fatal("workloadRef Rollout must still be emitted")
	}
	cfg := item.(map[string]any)["config"].(map[string]any)
	containers := cfg["containers"].([]map[string]any)
	if len(containers) != 0 {
		t.Errorf("containers = %+v; want empty for workloadRef Rollout", containers)
	}
}

func TestRolloutConverter_DefaultsReplicasToOne(t *testing.T) {
	u := rolloutFixture("checkout", "shop")
	unstructured.RemoveNestedField(u.Object, "spec", "replicas")

	item, ok := newRolloutConverter(nil)(u)
	if !ok {
		t.Fatal("converter returned ok=false")
	}
	if got := item.(map[string]any)["total_pods"]; got != int32(1) {
		t.Errorf("total_pods = %v; want 1 (Rollout default)", got)
	}
}

func TestRolloutConverter_RejectsNonUnstructured(t *testing.T) {
	if _, ok := newRolloutConverter(nil)(&corev1.Pod{}); ok {
		t.Error("typed object must be rejected")
	}
}

func TestRolloutConverter_PodLookup(t *testing.T) {
	lookup := func(uid types.UID) *corev1.Pod {
		if uid != "ro-uid-1" {
			return nil
		}
		return &corev1.Pod{Status: corev1.PodStatus{QOSClass: corev1.PodQOSBurstable}}
	}
	item, ok := newRolloutConverter(lookup)(rolloutFixture("checkout", "shop"))
	if !ok {
		t.Fatal("converter returned ok=false")
	}
	cfg := item.(map[string]any)["config"].(map[string]any)
	if cfg["qos_class"] != "Burstable" {
		t.Errorf("qos_class = %v; want Burstable (via UID-keyed lookup)", cfg["qos_class"])
	}
}

// Drives the full Service with a dynamic fake and verifies a Rollout lands in
// the initial TypeService snapshot — the informer/register/converter path
// end-to-end.
func TestService_EmitsRolloutSnapshot(t *testing.T) {
	cs := fake.NewClientset()
	cs.Resources = argoServedResources()
	dyn := dynamicfake.NewSimpleDynamicClient(rolloutScheme(), rolloutFixture("checkout", "shop"))

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
	defer srv.Close()

	sink := NewSink(srv.URL, "secret", "acc-1", "cluster-x", slog.Default())
	svc := NewService(cs, sink, time.Hour, slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if !svc.RegisterRollouts(ctx, dyn) {
		t.Fatal("RegisterRollouts returned false")
	}

	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(envelopes)
		mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(envelopes) == 0 {
		t.Fatal("no envelopes received")
	}
	first := envelopes[0]
	if first.Type != TypeService {
		t.Errorf("type = %q; want service", first.Type)
	}
	dataArr, ok := first.Data.([]any)
	if !ok || len(dataArr) != 1 {
		t.Fatalf("data = %+v; want one item", first.Data)
	}
	item := dataArr[0].(map[string]any)
	if item["service_key"] != "shop/Rollout/checkout" || item["type"] != "Rollout" {
		t.Errorf("rollout wire shape: %+v", item)
	}
}

func TestRegisterRollouts_SkipsWhenCRDAbsent(t *testing.T) {
	cs := fake.NewClientset() // no Fake.Resources → argoproj.io/v1alpha1 not served
	svc := NewService(cs, NewSink("http://unused", "", "", "", slog.Default()), time.Hour, slog.Default())
	dyn := dynamicfake.NewSimpleDynamicClient(rolloutScheme())

	if svc.RegisterRollouts(context.Background(), dyn) {
		t.Error("expected false when the Rollout CRD is not served")
	}
	if len(svc.handlers) != 0 {
		t.Errorf("handlers = %d; want 0", len(svc.handlers))
	}
}

func TestRegisterRollouts_SkipsWhenListForbidden(t *testing.T) {
	cs := fake.NewClientset()
	cs.Resources = argoServedResources()
	dyn := dynamicfake.NewSimpleDynamicClient(rolloutScheme())
	dyn.PrependReactor("list", "rollouts", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "argoproj.io", Resource: "rollouts"}, "", nil)
	})

	svc := NewService(cs, NewSink("http://unused", "", "", "", slog.Default()), time.Hour, slog.Default())
	if svc.RegisterRollouts(context.Background(), dyn) {
		t.Error("expected false when the list probe is Forbidden (RBAC missing)")
	}
	if len(svc.handlers) != 0 {
		t.Errorf("handlers = %d; want 0", len(svc.handlers))
	}
}
