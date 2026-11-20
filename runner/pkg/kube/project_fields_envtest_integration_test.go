//go:build integration

package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Lists real pods from a kube-apiserver whole and with a projection, and checks
// the projection keeps exactly what was asked for (plus identity) and is a
// fraction of the size. The pods carry what the API server adds on its own:
// managedFields, defaults, a resourceVersion.
func TestGetResourceFields_RealAPIServer(t *testing.T) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest.Start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	const pods = 50
	for i := range pods {
		pod := barePod()
		pod.Name = fmt.Sprintf("web-%d", i)
		pod.Namespace = "default"
		pod.UID = ""
		pod.Status = corev1.PodStatus{}
		pod.ManagedFields = nil
		pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-abc", UID: "00000000-0000-0000-0000-000000000001"}}
		if _, err := typed.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create pod: %v", err)
		}
	}

	c := NewClient(dyn, typed)
	params := map[string]any{"version": "v1", "resource_type": "pods", "all_namespaces": true}
	whole, err := c.GetResourceJSON(ctx, ParseGetParams(params))
	if err != nil {
		t.Fatal(err)
	}
	params["fields"] = []any{"metadata.ownerReferences", "spec.volumes", "spec.containers.env", "spec.containers.envFrom"}
	projected, err := c.GetResourceJSON(ctx, ParseGetParams(params))
	if err != nil {
		t.Fatal(err)
	}

	var got []map[string]any
	if err := json.Unmarshal(projected, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != pods {
		t.Fatalf("projected %d pods, want %d", len(got), pods)
	}
	for _, pod := range got {
		meta := pod["metadata"].(map[string]any)
		for _, key := range []string{"name", "namespace", "uid", "owner_references"} {
			if _, ok := meta[key]; !ok {
				t.Fatalf("metadata.%s missing from %v", key, meta)
			}
		}
		if _, ok := meta["managed_fields"]; ok {
			t.Fatal("managed_fields kept")
		}
		if _, ok := pod["status"]; ok {
			t.Fatal("status kept")
		}
		container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		if _, ok := container["image"]; ok {
			t.Fatal("container image kept")
		}
		if len(container["env"].([]any)) != 1 || len(container["env_from"].([]any)) != 1 {
			t.Fatalf("env/envFrom lost: %v", container)
		}
	}
	t.Logf("%d pods: whole %d bytes, projected %d bytes (%.0f%%)", pods, len(whole), len(projected),
		100*float64(len(projected))/float64(len(whole)))
	if len(projected)*2 > len(whole) {
		t.Errorf("projection is %d of %d bytes; want well under half", len(projected), len(whole))
	}
}
