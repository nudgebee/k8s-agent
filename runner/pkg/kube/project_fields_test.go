package kube

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func barePod() *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-0", Namespace: "app", UID: "uid-1",
			Labels:          map[string]string{"app": "web"},
			ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-abc"}},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-web-0"}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			Containers: []corev1.Container{{
				Name:    "web",
				Image:   "nginx:1.27",
				Command: []string{"nginx", "-g", "daemon off;"},
				Env: []corev1.EnvVar{{Name: "DB_PASSWORD", ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "password"},
				}}},
				EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "web-config"}}}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "192.0.2.10"},
	}
}

func getOne(t *testing.T, c *Client, params map[string]any) map[string]any {
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

func TestGetResource_FieldsKeepOnlyWhatIsAskedPlusIdentity(t *testing.T) {
	c := newFakeDynamic(t, barePod())

	pod := getOne(t, c, map[string]any{
		"version": "v1", "resource_type": "pods", "all_namespaces": true,
		"fields": []any{"metadata.ownerReferences", "spec.volumes.persistentVolumeClaim", "spec.containers.env", "spec.containers.envFrom"},
	})

	want := map[string]any{
		"api_version": "v1",
		"kind":        "Pod",
		"metadata": map[string]any{
			"name": "web-0", "namespace": "app", "uid": "uid-1",
			"owner_references": []any{map[string]any{"api_version": "", "kind": "ReplicaSet", "name": "web-abc", "uid": ""}},
		},
		"spec": map[string]any{
			// Each volume keeps what the rest of the path asks for: the claim on
			// the first, nothing on the emptyDir.
			"volumes": []any{
				map[string]any{"persistent_volume_claim": map[string]any{"claim_name": "data-web-0"}},
				map[string]any{},
			},
			"containers": []any{map[string]any{
				"env": []any{map[string]any{"name": "DB_PASSWORD", "value_from": map[string]any{
					"secret_key_ref": map[string]any{"name": "db", "key": "password"},
				}}},
				"env_from": []any{map[string]any{"config_map_ref": map[string]any{"name": "web-config"}}},
			}},
		},
	}
	if !reflect.DeepEqual(pod, want) {
		gotJSON, _ := json.MarshalIndent(pod, "", "  ")
		t.Errorf("projected pod:\n%s", gotJSON)
	}
}

func TestGetResource_NoFieldsSendsTheWholeObject(t *testing.T) {
	c := newFakeDynamic(t, barePod())

	pod := getOne(t, c, map[string]any{"version": "v1", "resource_type": "pods", "all_namespaces": true})

	for _, field := range []string{"status", "spec"} {
		if _, ok := pod[field]; !ok {
			t.Errorf("%s missing without a projection", field)
		}
	}
	if _, ok := pod["metadata"].(map[string]any)["managed_fields"]; !ok {
		t.Error("managed_fields dropped without a projection")
	}
}

// A projection removes fields; it never brings back a value redaction blanked.
func TestGetResource_FieldsDoNotUndoSecretRedaction(t *testing.T) {
	c := newFakeDynamic(t, &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "app"},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	})

	secret := getOne(t, c, map[string]any{
		"version": "v1", "resource_type": "secrets", "all_namespaces": true, "fields": []any{"data"},
	})

	if got := secret["data"].(map[string]any)["password"]; got != "" {
		t.Errorf("password = %v; want blank", got)
	}
}

// The size cap measures the reply that is sent, so a projection lets a list
// through that would be over the cap whole.
func TestGetResource_SizeCapMeasuresTheProjectedReply(t *testing.T) {
	c := newFakeDynamic(t, barePod())
	whole, err := c.GetResourceJSON(context.Background(), ParseGetParams(map[string]any{
		"version": "v1", "resource_type": "pods", "all_namespaces": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	c.MaxResultBytes = len(whole) - 1

	if _, err := c.GetResourceJSON(context.Background(), ParseGetParams(map[string]any{
		"version": "v1", "resource_type": "pods", "all_namespaces": true,
	})); !errors.Is(err, ErrResultTooLarge) {
		t.Fatalf("whole list under a cap one byte too small: err = %v, want ErrResultTooLarge", err)
	}
	if _, err := c.GetResourceJSON(context.Background(), ParseGetParams(map[string]any{
		"version": "v1", "resource_type": "pods", "all_namespaces": true, "fields": []any{"spec.nodeName"},
	})); err != nil {
		t.Fatalf("projected list: %v", err)
	}
}

func TestProjectFields(t *testing.T) {
	obj := map[string]any{
		"kind": "Thing",
		"metadata": map[string]any{
			"name": "a", "labels": map[string]any{"x": "1"},
		},
		"spec": map[string]any{
			"ports":  []any{80.0, 443.0},
			"matrix": []any{[]any{map[string]any{"v": 1.0, "w": 2.0}}},
			"items":  []any{map[string]any{"name": "i", "size": 3.0}, "plain"},
			"scalar": "s",
		},
	}
	cases := []struct {
		name   string
		fields []string
		want   map[string]any
	}{
		{
			name:   "an array of scalars is kept as it is",
			fields: []string{"spec.ports"},
			want:   map[string]any{"kind": "Thing", "metadata": map[string]any{"name": "a"}, "spec": map[string]any{"ports": []any{80.0, 443.0}}},
		},
		{
			name:   "the rest of the path applies to each element; non-map elements are kept",
			fields: []string{"spec.items.name"},
			want:   map[string]any{"kind": "Thing", "metadata": map[string]any{"name": "a"}, "spec": map[string]any{"items": []any{map[string]any{"name": "i"}, "plain"}}},
		},
		{
			name:   "an array of arrays keeps the inner array whole",
			fields: []string{"spec.matrix.v"},
			want:   map[string]any{"kind": "Thing", "metadata": map[string]any{"name": "a"}, "spec": map[string]any{"matrix": []any{[]any{map[string]any{"v": 1.0, "w": 2.0}}}}},
		},
		{
			name:   "a missing path adds nothing, and a path through a scalar stops there",
			fields: []string{"spec.absent", "spec.scalar.deeper", "status"},
			want:   map[string]any{"kind": "Thing", "metadata": map[string]any{"name": "a"}, "spec": map[string]any{}},
		},
		{
			name:   "a wider path wins over a narrower one, in either order",
			fields: []string{"metadata.labels.x", "metadata", "spec.items.size", "spec.items"},
			want: map[string]any{"kind": "Thing", "metadata": obj["metadata"], "spec": map[string]any{
				"items": []any{map[string]any{"name": "i", "size": 3.0}, "plain"},
			}},
		},
		{
			name:   "a malformed path keeps nothing extra",
			fields: []string{"spec.", "spec..ports", ""},
			want:   map[string]any{"kind": "Thing", "metadata": map[string]any{"name": "a"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := projectFields(obj, newFieldTree(tc.fields))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v\nwant %#v", got, tc.want)
			}
		})
	}
	if newFieldTree(nil) != nil {
		t.Error("no fields must mean no projection")
	}
}
