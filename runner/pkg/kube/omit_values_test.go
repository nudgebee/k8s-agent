package kube

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

func appConfigMap() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{Kind: "ConfigMap", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "settings", Namespace: "app",
			Annotations: map[string]string{
				lastAppliedAnnotation:             `{"data":{"timeout":"30s"}}`,
				"objectset.rio.cattle.io/applied": "H4sIAAAA",
				"team":                            "payments",
			},
		},
		Data:       map[string]string{"timeout": "30s", "retries": "3"},
		BinaryData: map[string][]byte{"cert": {0x01, 0x02}},
	}
}

func getConfigMapsJSON(t *testing.T, c *Client, params map[string]any) map[string]any {
	t.Helper()
	got, err := c.GetResourceJSON(context.Background(), ParseGetParams(params))
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(got, &arr); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	if len(arr) != 1 {
		t.Fatalf("len = %d; want 1", len(arr))
	}
	return arr[0]
}

func annotationsOf(obj map[string]any) map[string]any {
	ann, _ := obj["metadata"].(map[string]any)["annotations"].(map[string]any)
	return ann
}

func TestGetResource_OmitDataValuesBlanksConfigMapValues(t *testing.T) {
	c := newFakeDynamic(t, appConfigMap())

	obj := getConfigMapsJSON(t, c, map[string]any{
		"version": "v1", "resource_type": "configmaps", "all_namespaces": true, "omit_data_values": true,
	})

	for field, want := range map[string]int{"data": 2, "binary_data": 1} {
		m, _ := obj[field].(map[string]any)
		if len(m) != want {
			t.Fatalf("%s = %v; want %d keys kept", field, m, want)
		}
		for k, v := range m {
			if v != "" {
				t.Errorf("%s[%q] = %v; want blank", field, k, v)
			}
		}
	}
	ann := annotationsOf(obj)
	for _, k := range objectCopyAnnotations {
		if _, ok := ann[k]; ok {
			t.Errorf("annotation %s kept; it copies the values", k)
		}
	}
	if ann["team"] != "payments" {
		t.Errorf("unrelated annotation lost: %v", ann)
	}
}

// Without the option the reply is what it has always been: callers that read
// ConfigMap values (the UI, kubectl-style tools) are unaffected.
func TestGetResource_ConfigMapValuesKeptByDefault(t *testing.T) {
	c := newFakeDynamic(t, appConfigMap())

	obj := getConfigMapsJSON(t, c, map[string]any{
		"version": "v1", "resource_type": "configmaps", "namespace": "app",
	})

	if data, _ := obj["data"].(map[string]any); data["timeout"] != "30s" {
		t.Errorf("data = %v; want values kept", data)
	}
	if _, ok := annotationsOf(obj)[lastAppliedAnnotation]; !ok {
		t.Error("last-applied-configuration dropped without the option")
	}
}

// The option is ConfigMap-only: on any other kind it changes nothing.
func TestGetResource_OmitDataValuesIgnoresOtherKinds(t *testing.T) {
	pod := newPod("web-0", "app")
	pod.SetAnnotations(map[string]string{lastAppliedAnnotation: `{"spec":{}}`})
	c := newFakeDynamic(t, pod)

	got, err := c.GetResourceJSON(context.Background(), ParseGetParams(map[string]any{
		"version": "v1", "resource_type": "pods", "namespace": "app", "omit_data_values": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(got, &arr); err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("len = %d; want 1", len(arr))
	}
	if _, ok := annotationsOf(arr[0])[lastAppliedAnnotation]; !ok {
		t.Error("pod annotation dropped; the option must only touch ConfigMaps")
	}
}
