//go:build integration

package kube

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Round-trips the Group-B primitives against a real kube-apiserver:
//  1. Create a ConfigMap via the typed clientset.
//  2. Fetch it back via the agent's GetResource (dynamic client) — JSON.
//  3. Fetch it via GetResourceYAML — YAML.
//  4. List names via ListResourceNames.
func TestKubeClient_RealAPIServer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; -short")
	}

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

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	// Seed: a ConfigMap with a marker we can recognise.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-test", Namespace: "default"},
		Data:       map[string]string{"marker": "round-trip-ok"},
	}
	if _, err := typed.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed configmap: %v", err)
	}

	c := NewClient(dyn, typed)

	t.Run("GetResource_NamedConfigMap", func(t *testing.T) {
		got, err := c.GetResource(ctx, GetParams{
			Version: "v1", ResourceType: "configmaps",
			Namespace: "default", Name: "agent-test",
		})
		if err != nil {
			t.Fatal(err)
		}
		m, _ := got.(map[string]any)
		data, _ := m["data"].(map[string]any)
		if data["marker"] != "round-trip-ok" {
			t.Errorf("marker = %v; want round-trip-ok", data["marker"])
		}
	})

	t.Run("GetResource_ListByNamespace", func(t *testing.T) {
		got, err := c.GetResource(ctx, GetParams{
			Version: "v1", ResourceType: "configmaps", Namespace: "default",
		})
		if err != nil {
			t.Fatal(err)
		}
		m, _ := got.(map[string]any)
		items, _ := m["items"].([]any)
		if len(items) == 0 {
			t.Errorf("expected ≥1 configmaps in default ns; got 0")
		}
	})

	t.Run("GetResourceYAML", func(t *testing.T) {
		y, err := c.GetResourceYAML(ctx, GetParams{
			Version: "v1", ResourceType: "configmaps",
			Namespace: "default", Name: "agent-test",
		})
		if err != nil {
			t.Fatal(err)
		}
		s := string(y)
		if !strings.Contains(s, "marker: round-trip-ok") {
			t.Errorf("YAML missing expected key:\n%s", s)
		}
	})

	t.Run("ListResourceNames", func(t *testing.T) {
		names, err := c.ListResourceNames(ctx, GetParams{
			Version: "v1", ResourceType: "configmaps", Namespace: "default",
		})
		if err != nil {
			t.Fatal(err)
		}
		seen := false
		for _, n := range names {
			if n.Name == "agent-test" {
				seen = true
			}
		}
		if !seen {
			t.Errorf("agent-test not in names: %+v", names)
		}
	})

	t.Run("Handlers_DispatchEndToEnd_FindingShape", func(t *testing.T) {
		// Handlers wraps the raw K8s payload in a Finding envelope so UI/api-server
		// callers can walk findings[0].evidence[0].data → JSON-string → blocks → data.
		hs := Handlers(c, nil, "test-account")
		got, err := hs["get_resource"](ctx, map[string]any{
			"version":       "v1",
			"resource_type": "configmaps",
			"namespace":     "default",
			"name":          "agent-test",
		})
		if err != nil {
			t.Fatal(err)
		}
		envelope, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("handler did not return Finding envelope; got %T", got)
		}
		findings, ok := envelope["findings"].([]any)
		if !ok || len(findings) != 1 {
			t.Fatalf("envelope.findings shape wrong: %+v", envelope)
		}
		finding := findings[0].(map[string]any)
		evidence := finding["evidence"].([]any)
		if len(evidence) != 1 {
			t.Fatalf("evidence count = %d; want 1", len(evidence))
		}
		ev := evidence[0].(map[string]any)
		if ev["account_id"] != "test-account" {
			t.Errorf("evidence.account_id = %v; want test-account", ev["account_id"])
		}
		if ev["file_type"] != "structured_data" {
			t.Errorf("evidence.file_type = %v; want structured_data", ev["file_type"])
		}
		// evidence.data is a JSON-stringified array of typed blocks.
		blocksJSON, _ := ev["data"].(string)
		if !strings.Contains(blocksJSON, `"type":"json"`) {
			t.Errorf("evidence.data missing json block: %s", blocksJSON)
		}
		if !strings.Contains(blocksJSON, "agent-test") {
			t.Errorf("evidence.data missing actual K8s payload: %s", blocksJSON)
		}
	})
}
