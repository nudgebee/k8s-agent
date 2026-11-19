package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

// templateLabelsContractFile is the workload `config` the Knowledge Graph
// (nudgebee-enterprise, sources/k8s/testdata/agent_workload_config.json) builds
// its Service-matching tests from. Regenerate with UPDATE_CONTRACT=1 and copy the
// file there; a key rename then fails one side or the other.
const templateLabelsContractFile = "testdata/workload_config_contract.json"

// templateLabelledStatefulSet carries a label only on its pod template — the
// shape of a chart's primary database, whose Service selects `role: primary`.
func templateLabelledStatefulSet() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-postgresql", Namespace: "db", ResourceVersion: "7",
			Labels: map[string]string{"app.kubernetes.io/name": "postgresql"},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"app.kubernetes.io/name": "postgresql", "role": "primary",
				}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "postgresql", Image: "postgres:16"}}},
			},
		},
	}
}

// labelledRollout is an inline-template Rollout whose object carries no labels.
func labelledRollout() *unstructured.Unstructured {
	u := rolloutFixture("checkout", "shop")
	unstructured.RemoveNestedField(u.Object, "metadata", "labels")
	_ = unstructured.SetNestedStringMap(u.Object, map[string]string{"app": "checkout"},
		"spec", "template", "metadata", "labels")
	return u
}

func configOf(t *testing.T, item any, ok bool) map[string]any {
	t.Helper()
	if !ok {
		t.Fatal("converter returned ok=false")
	}
	return item.(map[string]any)["config"].(map[string]any)
}

func TestServiceDict_TemplateLabelsAreThePodTemplates(t *testing.T) {
	item, ok := convertStatefulSet(templateLabelledStatefulSet())
	cfg := configOf(t, item, ok)

	wantTemplate := map[string]string{"app.kubernetes.io/name": "postgresql", "role": "primary"}
	if got := cfg["template_labels"]; !reflect.DeepEqual(got, wantTemplate) {
		t.Errorf("template_labels = %v; want %v", got, wantTemplate)
	}
	// The object's own labels are unchanged: k8s_workloads.labels and the UI read them.
	if got := cfg["labels"]; !reflect.DeepEqual(got, map[string]string{"app.kubernetes.io/name": "postgresql"}) {
		t.Errorf("labels = %v; want the object's own labels", got)
	}
}

func TestServiceDict_TemplateLabelsNeverNull(t *testing.T) {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "ns"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
	}
	item, ok := convertDeployment(d)
	cfg := configOf(t, item, ok)
	raw, err := json.Marshal(cfg["template_labels"])
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{}" {
		t.Errorf("template_labels marshals to %s; want {} (never null)", raw)
	}
}

func TestRolloutConverter_TemplateLabels(t *testing.T) {
	item, ok := newRolloutConverter(nil)(labelledRollout())
	cfg := configOf(t, item, ok)
	if got := cfg["template_labels"]; !reflect.DeepEqual(got, map[string]string{"app": "checkout"}) {
		t.Errorf("template_labels = %v; want the inline template's labels", got)
	}
	if got := cfg["labels"]; !reflect.DeepEqual(got, map[string]string{}) {
		t.Errorf("labels = %v; want {} for a Rollout with no object labels", got)
	}
}

func TestRolloutConverter_WorkloadRefHasEmptyTemplateLabels(t *testing.T) {
	u := rolloutFixture("checkout", "shop")
	unstructured.RemoveNestedField(u.Object, "spec", "template")
	_ = unstructured.SetNestedMap(u.Object, map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment", "name": "checkout",
	}, "spec", "workloadRef")

	item, ok := newRolloutConverter(nil)(u)
	cfg := configOf(t, item, ok)
	if got := cfg["template_labels"]; !reflect.DeepEqual(got, map[string]string{}) {
		t.Errorf("template_labels = %v; want {} — the referenced Deployment's template is not resolved", got)
	}
}

// TestTemplateLabelsContract pins the wire shape the Knowledge Graph's tests are
// built from. On a deliberate change, regenerate (UPDATE_CONTRACT=1) and refresh
// the copy in nudgebee-enterprise.
func TestTemplateLabelsContract(t *testing.T) {
	sts, ok := convertStatefulSet(templateLabelledStatefulSet())
	stsCfg := configOf(t, sts, ok)
	rollout, ok := newRolloutConverter(nil)(labelledRollout())
	rolloutCfg := configOf(t, rollout, ok)

	got, err := json.MarshalIndent(map[string]any{
		"StatefulSet": stsCfg,
		"Rollout":     rolloutCfg,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	if os.Getenv("UPDATE_CONTRACT") == "1" {
		if err := os.MkdirAll(filepath.Dir(templateLabelsContractFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(templateLabelsContractFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(templateLabelsContractFile)
	if err != nil {
		t.Fatalf("read %s (regenerate with UPDATE_CONTRACT=1): %v", templateLabelsContractFile, err)
	}
	if string(got) != string(want) {
		t.Errorf("workload config drifted from %s — the Knowledge Graph builds its tests from it.\ngot:\n%s\nwant:\n%s",
			templateLabelsContractFile, got, want)
	}
}
