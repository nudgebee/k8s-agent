package k8swatch

import (
	"strings"
	"testing"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestOwnTransform_LeavesOrdinaryConfigMapsWhole(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "app", ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}},
		Data:       map[string]string{"config.yaml": strings.Repeat("a: b\n", 20000)}, // 100 KB
	}
	want := cm.Data["config.yaml"]
	if _, err := OwnTransform(cm); err != nil {
		t.Fatal(err)
	}
	if cm.ManagedFields != nil {
		t.Error("managedFields kept")
	}
	if cm.Data["config.yaml"] != want {
		t.Error("a ConfigMap under the cap was changed")
	}
}

// Over the cap, each long value keeps its head and a hash of the whole value:
// an edit far into the value still changes what the diff compares.
func TestOwnTransform_TrimsOversizedConfigMapsByValue(t *testing.T) {
	big := strings.Repeat("x", 300<<10)
	edited := big[:len(big)-1] + "y"
	mk := func(v string) *corev1.ConfigMap {
		return &corev1.ConfigMap{
			Data:       map[string]string{"dash.json": v, "small": "keep me"},
			BinaryData: map[string][]byte{"blob": []byte(strings.Repeat("z", 5000))},
		}
	}
	a, b := mk(big), mk(edited)
	_, _ = OwnTransform(a)
	_, _ = OwnTransform(b)

	if got := len(a.Data["dash.json"]); got > configMapHeadBytes+200 {
		t.Errorf("trimmed value is %d bytes", got)
	}
	if !strings.HasPrefix(a.Data["dash.json"], big[:configMapHeadBytes]) {
		t.Error("trimmed value lost its head")
	}
	if a.Data["dash.json"] == b.Data["dash.json"] {
		t.Error("an edit past the head is invisible after trimming")
	}
	if a.Data["small"] != "keep me" {
		t.Error("a short value was changed")
	}
	if len(a.BinaryData["blob"]) > configMapHeadBytes+200 {
		t.Error("long binary value not trimmed")
	}

	// Idempotent: the cache may run the transform again on the same object.
	before := a.Data["dash.json"]
	_, _ = OwnTransform(a)
	if a.Data["dash.json"] != before {
		t.Error("second run changed an already trimmed value")
	}
}

func TestTrimValue_DoesNotSplitACharacter(t *testing.T) {
	v := strings.Repeat("é", 200<<10) // 2 bytes each
	got := trimValue("x" + v)         // shift so byte 2048 falls mid-character
	if !utf8.ValidString(got) {
		t.Error("trimmed value is not valid UTF-8")
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
