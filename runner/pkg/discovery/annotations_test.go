package discovery

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// What `kubectl apply` and Rancher leave on an object: a full copy of it,
// inline env values included.
func appliedAnnotations() map[string]string {
	return map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"DB_PASSWORD","value":"applied-pass"}]}]}}}}`,
		"objectset.rio.cattle.io/applied":                  "H4sIAAAAAAAA-rancher-copy",
		"deployment.kubernetes.io/revision":                "4",
		"argocd.argoproj.io/tracking-id":                   "shop:apps/Deployment:prod/api",
	}
}

var keptAnnotations = map[string]string{
	"deployment.kubernetes.io/revision": "4",
	"argocd.argoproj.io/tracking-id":    "shop:apps/Deployment:prod/api",
}

func assertNoAppliedCopy(t *testing.T, what string, wire map[string]any) {
	t.Helper()
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"last-applied-configuration", "objectset.rio.cattle.io/applied", "applied-pass", "rancher-copy"} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("%s: wire payload carries %q: %s", what, leaked, raw)
		}
	}
}

func TestDiscovery_NeverSendsAppliedCopyAnnotations(t *testing.T) {
	ann := appliedAnnotations()
	before := maps.Clone(ann)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod", Annotations: ann},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Image: "api:1"}}}},
		},
	}
	got, ok := convertDeployment(dep)
	if !ok {
		t.Fatal("convertDeployment rejected the Deployment")
	}
	wire := got.(map[string]any)
	assertNoAppliedCopy(t, "Deployment", wire)
	if gotAnn := wire["config"].(map[string]any)["annotations"].(map[string]string); !maps.Equal(gotAnn, keptAnnotations) {
		t.Errorf("Deployment annotations = %v; want %v", gotAnn, keptAnnotations)
	}

	ns, _ := convertNamespace(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "prod", Annotations: ann}})
	assertNoAppliedCopy(t, "Namespace", ns.(map[string]any))

	node, _ := convertNode(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Annotations: ann}})
	assertNoAppliedCopy(t, "Node", node.(map[string]any))

	// ann is the informer cache's own map; the converters must not touch it.
	if !maps.Equal(ann, before) {
		t.Errorf("converter modified the cached object's annotations: %v", ann)
	}
}

func TestWireAnnotations(t *testing.T) {
	if got := wireAnnotations(nil); got == nil || len(got) != 0 {
		t.Errorf("nil annotations must become an empty map; got %#v", got)
	}
	plain := map[string]string{"a": "b"}
	if got := wireAnnotations(plain); !maps.Equal(got, plain) {
		t.Errorf("annotations without an applied copy must pass through; got %v", got)
	}
	only := map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"}
	if got := wireAnnotations(only); got == nil || len(got) != 0 {
		t.Errorf("only an applied copy must leave an empty map; got %#v", got)
	}
}
