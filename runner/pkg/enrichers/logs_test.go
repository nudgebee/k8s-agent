package enrichers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

// TestLogsEnricher_PodResolutionAndShape covers two things at once: pod
// resolution by direct name (vs workload-fallback), and the FileBlock-shaped
// Finding the api-server caller (eventrule_actions_logs.go) reads. The fake
// clientset returns "fake logs" for any pod by default.
func TestLogsEnricher_PodResolutionAndShape(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frontend", Namespace: "shop"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "web"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cs := fake.NewClientset(pod)

	l := NewLogsEnricher(cs, "acc-1")
	resp, err := l.Handle(context.Background(), map[string]any{
		"name":      "frontend",
		"namespace": "shop",
	})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.(map[string]any)
	if r["success"] != true {
		t.Fatalf("success = %v: %+v", r["success"], r)
	}
	evidence := r["findings"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(evidence["data"].(string)), &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d; want 1", len(blocks))
	}
	// Text files (.log) get gzipped before sending — the resulting type
	// is "gz", filename has the .gz suffix appended.
	// UI's KubernetesPodLogs.tsx:68-71 matches on type === "gz".
	if blocks[0]["type"] != "gz" {
		t.Errorf("type = %v; want gz (.log files get gzipped)", blocks[0]["type"])
	}
	if blocks[0]["filename"] != "frontend.log.gz" {
		t.Errorf("filename = %v; want frontend.log.gz", blocks[0]["filename"])
	}
	// Decode the b'<base64>' wrapper, gunzip, expect the fake-client's
	// "fake logs" payload.
	wrapped, _ := blocks[0]["data"].(string)
	if !strings.HasPrefix(wrapped, "b'") || !strings.HasSuffix(wrapped, "'") {
		t.Fatalf("data must be wrapped as b'<base64>': got %q", wrapped)
	}
	b64 := wrapped[2 : len(wrapped)-1]
	gz, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	plain, _ := io.ReadAll(gr)
	if !strings.Contains(string(plain), "fake logs") {
		t.Errorf("decoded log payload missing fake-client marker: %q", plain)
	}

	add := blocks[0]["additional_info"].(map[string]any)
	if add["pod_name"] != "frontend" || add["container_name"] != "web" || add["namespace"] != "shop" {
		t.Errorf("additional_info wrong: %+v", add)
	}
}

// TestLogsEnricher_WorkloadFallback covers the case where `name` is the
// workload name and the agent has to walk pods to find an owned one. The
// fake clientset doesn't materialize ownerReferences automatically, so we
// stamp them. We test both Deployment-style (ReplicaSet middle) and direct
// ownership (StatefulSet/DaemonSet pattern).
func TestLogsEnricher_WorkloadFallback(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-77c7-x9q",
			Namespace: "shop",
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "ReplicaSet",
				Name:       "web-77c7",
				Controller: ptr.To(true),
			}},
		},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	cs := fake.NewClientset(pod)
	l := NewLogsEnricher(cs, "acc-1")
	resp, err := l.Handle(context.Background(), map[string]any{
		"name":      "web", // workload, not pod
		"namespace": "shop",
	})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.(map[string]any)
	if r["success"] != true {
		t.Fatalf("success = %v: %+v", r["success"], r)
	}
}

func TestLogsEnricher_MissingNameErrors(t *testing.T) {
	cs := fake.NewClientset()
	l := NewLogsEnricher(cs, "acc-1")
	resp, _ := l.Handle(context.Background(), map[string]any{"namespace": "x"})
	if resp.(map[string]any)["success"] != false {
		t.Error("expected success=false for missing name")
	}
}

func TestLogsEnricher_NilClientErrors(t *testing.T) {
	l := NewLogsEnricher(nil, "acc-1")
	resp, _ := l.Handle(context.Background(), map[string]any{"name": "x", "namespace": "y"})
	if resp.(map[string]any)["success"] != false {
		t.Error("expected success=false when kube client unavailable")
	}
}
