package kube

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func newFakeDynamic(t *testing.T, objs ...runtime.Object) *Client {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	dyn := dynamicfake.NewSimpleDynamicClient(scheme, objs...)
	return NewClient(dyn, nil)
}

func TestParseGetParams(t *testing.T) {
	got := ParseGetParams(map[string]any{
		"group":          "rbac.authorization.k8s.io",
		"version":        "v1",
		"resource_type":  "roles,rolebindings",
		"namespace":      "kube-system",
		"name":           "admin",
		"all_namespaces": true,
	})
	want := GetParams{
		Group: "rbac.authorization.k8s.io", Version: "v1",
		ResourceType: "roles,rolebindings",
		Namespace:    "kube-system", Name: "admin",
		AllNamespaces: true,
	}
	if got != want {
		t.Errorf("ParseGetParams\n got:  %+v\n want: %+v", got, want)
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" a , b ,c", []string{"a", "b", "c"}},
		{",,a,,", []string{"a"}},
	}
	for _, c := range cases {
		got := splitCSV(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitCSV(%q) = %v; want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitCSV(%q)[%d] = %q; want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestGetResource_RequiresVersionAndResourceType(t *testing.T) {
	c := newFakeDynamic(t)
	if _, err := c.GetResource(context.Background(), GetParams{}); err == nil {
		t.Error("expected error for missing version/resource_type")
	}
}

func TestGetResource_NamedPod_ReturnsSingleElementArray(t *testing.T) {
	// list_kubernetes_resources lists then filters by name; for name-based
	// lookup it returns [obj], not the bare object — so UI/api-server
	// callers always handle a flat array.
	pod := newPod("frontend", "shop")
	c := newFakeDynamic(t, pod)

	got, err := c.GetResource(context.Background(), GetParams{
		Version: "v1", ResourceType: "pods", Namespace: "shop", Name: "frontend",
	})
	if err != nil {
		t.Fatal(err)
	}
	arr, ok := got.([]any)
	if !ok {
		t.Fatalf("got %T; want []any", got)
	}
	if len(arr) != 1 {
		t.Fatalf("len = %d; want 1", len(arr))
	}
	m, _ := arr[0].(map[string]any)
	meta, _ := m["metadata"].(map[string]any)
	if meta["name"] != "frontend" {
		t.Errorf("name = %v; want frontend", meta["name"])
	}
}

func TestGetResource_ListAllNamespaces_ReturnsFlatArray(t *testing.T) {
	pod1 := newPod("a", "ns1")
	pod2 := newPod("b", "ns2")
	c := newFakeDynamic(t, pod1, pod2)

	got, err := c.GetResource(context.Background(), GetParams{
		Version: "v1", ResourceType: "pods", AllNamespaces: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	arr, ok := got.([]any)
	if !ok {
		t.Fatalf("got %T; want []any (flat array)", got)
	}
	if len(arr) != 2 {
		t.Errorf("len = %d; want 2", len(arr))
	}
}

func TestGetResource_CommaSeparated_FlattensIntoOneArray(t *testing.T) {
	// all_resources is a concatenated flat array, not
	// {pods: [...], services: [...]}.
	pod := newPod("a", "ns1")
	c := newFakeDynamic(t, pod)
	got, err := c.GetResource(context.Background(), GetParams{
		Version: "v1", ResourceType: "pods,services", Namespace: "ns1",
	})
	if err != nil {
		t.Fatal(err)
	}
	arr, ok := got.([]any)
	if !ok {
		t.Fatalf("got %T; want []any", got)
	}
	if len(arr) != 1 {
		t.Errorf("len = %d; want 1 (the pod; services empty)", len(arr))
	}
}

func TestGetResourceYAML_NonEmpty(t *testing.T) {
	pod := newPod("a", "ns1")
	c := newFakeDynamic(t, pod)
	y, err := c.GetResourceYAML(context.Background(), GetParams{
		Version: "v1", ResourceType: "pods", Namespace: "ns1", Name: "a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(y) == 0 {
		t.Error("YAML output is empty")
	}
}

func TestListResourceNames(t *testing.T) {
	c := newFakeDynamic(t, newPod("a", "ns1"), newPod("b", "ns1"))
	got, err := c.ListResourceNames(context.Background(), GetParams{
		Version: "v1", ResourceType: "pods", Namespace: "ns1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d names; want 2", len(got))
	}
	names := map[string]bool{got[0].Name: true, got[1].Name: true}
	if !names["a"] || !names["b"] {
		t.Errorf("missing names; got %+v", got)
	}
}

func newPod(name, namespace string) *unstructured.Unstructured {
	pod := &unstructured.Unstructured{}
	pod.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Pod"})
	pod.SetName(name)
	pod.SetNamespace(namespace)
	pod.Object["metadata"] = map[string]any{
		"name":              name,
		"namespace":         namespace,
		"creationTimestamp": metav1.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	return pod
}
