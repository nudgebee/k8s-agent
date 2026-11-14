package k8swatch

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Only managedFields goes, as with kubewatch: even a ConfigMap far over the
// evidence cap is cached whole, so its diff evidence is what kubewatch's was.
func TestOwnTransform_StripsOnlyManagedFields(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "app", ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}},
		Data:       map[string]string{"dash.json": strings.Repeat("x", 300<<10), "small": "v"},
		BinaryData: map[string][]byte{"blob": []byte(strings.Repeat("z", 5000))},
	}
	want := cm.DeepCopy()
	want.ManagedFields = nil
	if _, err := OwnTransform(cm); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cm, want) {
		t.Error("ConfigMap changed beyond dropping managedFields")
	}
}

func TestOwnTransform_StripsManagedFieldsFromRollouts(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"kind":     "Rollout",
		"metadata": map[string]any{"name": "r", "managedFields": []any{map[string]any{"manager": "argo"}}},
		"spec":     map[string]any{"replicas": int64(2)},
	}}
	_, _ = OwnTransform(u)
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "metadata", "managedFields"); found {
		t.Error("managedFields kept")
	}
	if r, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas"); r != 2 {
		t.Error("spec changed")
	}
}
