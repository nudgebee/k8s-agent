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
	fake "k8s.io/client-go/kubernetes/fake"
)

// Drives the Service against a fake clientset pre-populated with one Pod
// and verifies that:
//  1. an initial full-load snapshot envelope arrives at the sink
//  2. the envelope's data contains the converted Pod
//  3. the converter produced the expected wire shape
func TestService_EmitsInitialFullSnapshot(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "frontend",
			Namespace:         "shop",
			ResourceVersion:   "42",
			CreationTimestamp: metav1.Now(),
			Labels:            map[string]string{"app": "frontend"},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "frontend-rs-1"}},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-1",
			Containers: []corev1.Container{
				{Name: "web", Image: "nginx:1.27"},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	cs := fake.NewClientset(pod)

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
	svc.RegisterPods()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	// Poll for the initial snapshot.
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
	if !first.FullLoad || !first.IsFirstBatch || !first.IsLastBatch {
		t.Errorf("expected full-load batch envelope, got %+v", first)
	}
	dataArr, ok := first.Data.([]any)
	if !ok {
		t.Fatalf("data = %T; want []any (batched items)", first.Data)
	}
	if len(dataArr) != 1 {
		t.Fatalf("data items = %d; want 1", len(dataArr))
	}
	item, ok := dataArr[0].(map[string]any)
	if !ok {
		t.Fatalf("data[0] = %T; want map", dataArr[0])
	}
	// Wire shape: `type` (not service_type) and computed `service_key` are
	// what the collector reads at the backend.
	if item["name"] != "frontend" || item["namespace"] != "shop" || item["type"] != "Pod" {
		t.Errorf("converter shape: %+v", item)
	}
	if item["service_key"] != "shop/Pod/frontend" {
		t.Errorf("service_key = %v", item["service_key"])
	}
}
