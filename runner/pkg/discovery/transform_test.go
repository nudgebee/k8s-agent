package discovery

import (
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

// stripVolatile drops time-based fields so two converter runs at slightly
// different wall-clock times compare equal.
func stripVolatile(m any) {
	if mm, ok := m.(map[string]any); ok {
		delete(mm, "update_time")
		delete(mm, "updated_at")
	}
}

func fatPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "api-xyz",
			Namespace:       "ns",
			ResourceVersion: "99",
			UID:             "pod-uid",
			Labels:          map[string]string{"app": "api"},
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": `{"huge":"manifest"}`,
				"app.kubernetes.io/managed-by":                     "Helm",
				"prometheus.io/scrape":                             "true",
			},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "api-rs", Controller: ptr.To(true)}},
			ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "kubelet"}, {Manager: "kube-controller"}},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-9",
			Volumes:  []corev1.Volume{{Name: "data"}}, // pod converter emits no volumes → strippable
			Containers: []corev1.Container{{
				Name:          "web",
				Image:         "nginx:1.27",
				Env:           []corev1.EnvVar{{Name: "BIG", Value: "xxxxxxxx"}},
				VolumeMounts:  []corev1.VolumeMount{{Name: "data", MountPath: "/d"}},
				LivenessProbe: &corev1.Probe{},
			}},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSBurstable,
			PodIP:    "10.0.0.9",
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "web", Ready: true, RestartCount: 3},
			},
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "init"}}, // strippable
		},
	}
}

// The core safety property: stripping fields in the transform must NOT change
// any converter's output. We convert a pristine copy and a transformed copy and
// assert the wire shapes are identical (modulo the volatile update_time).
func TestDiscoveryTransform_PreservesPodConverterOutput(t *testing.T) {
	pod := fatPod()

	want, ok := convertPod(pod.DeepCopy())
	if !ok {
		t.Fatal("baseline convertPod ok=false")
	}
	transformed, err := discoveryTransform(pod.DeepCopy())
	if err != nil {
		t.Fatalf("transform err: %v", err)
	}
	got, ok := convertPod(transformed)
	if !ok {
		t.Fatal("post-transform convertPod ok=false")
	}
	stripVolatile(want)
	stripVolatile(got)
	if !reflect.DeepEqual(want, got) {
		t.Errorf("transform changed pod converter output:\n want %+v\n got  %+v", want, got)
	}
}

func TestDiscoveryTransform_PreservesDeploymentConverterOutput(t *testing.T) {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "api",
			Namespace:     "ns",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "x"}},
			Annotations:   map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}", "keep": "me"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(2)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
				Spec: corev1.PodSpec{
					ServiceAccountName: "api-sa",
					Volumes:            []corev1.Volume{{Name: "data"}},
					Tolerations:        []corev1.Toleration{{Key: "k", Value: "v"}},
					Containers: []corev1.Container{{
						Name:      "web",
						Image:     "nginx",
						Env:       []corev1.EnvVar{{Name: "X", Value: "y"}}, // stripped
						Resources: corev1.ResourceRequirements{},
					}},
				},
			},
		},
	}

	want, ok := convertDeployment(dep.DeepCopy())
	if !ok {
		t.Fatal("baseline ok=false")
	}
	transformed, err := discoveryTransform(dep.DeepCopy())
	if err != nil {
		t.Fatalf("transform err: %v", err)
	}
	got, ok := convertDeployment(transformed)
	if !ok {
		t.Fatal("post-transform ok=false")
	}
	stripVolatile(want)
	stripVolatile(got)
	if !reflect.DeepEqual(want, got) {
		t.Errorf("transform changed deployment converter output:\n want %+v\n got  %+v", want, got)
	}
}

// Confirm the transform actually strips the heavy fields (otherwise the
// preserve-output test could pass trivially with a no-op transform).
func TestDiscoveryTransform_StripsHeavyFields(t *testing.T) {
	out, err := discoveryTransform(fatPod())
	if err != nil {
		t.Fatal(err)
	}
	pod := out.(*corev1.Pod)
	if pod.ManagedFields != nil {
		t.Error("ManagedFields not stripped")
	}
	// Annotations must be preserved intact — the workload/node/namespace
	// converters emit config.annotations, so trimming any key changes the wire.
	if _, present := pod.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; !present {
		t.Error("annotations must be preserved (converters emit them)")
	}
	if pod.Annotations["app.kubernetes.io/managed-by"] != "Helm" {
		t.Error("annotation wrongly removed (would break isHelmRelease)")
	}
	if pod.Spec.Containers[0].Env != nil || pod.Spec.Containers[0].LivenessProbe != nil {
		t.Error("container Env/LivenessProbe not stripped")
	}
	if pod.Spec.Volumes != nil || pod.Status.InitContainerStatuses != nil {
		t.Error("pod Volumes / InitContainerStatuses not stripped")
	}
}

// Unknown types and DeletedFinalStateUnknown tombstones must pass through
// untouched without error or panic.
func TestDiscoveryTransform_PassThrough(t *testing.T) {
	tomb := cache.DeletedFinalStateUnknown{Key: "ns/p", Obj: fatPod()}
	out, err := discoveryTransform(tomb)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := out.(cache.DeletedFinalStateUnknown); !ok {
		t.Errorf("tombstone not passed through: %T", out)
	}
	// A type the switch doesn't handle (ConfigMap) passes through as-is.
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "c", ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "x"}}}}
	out2, err := discoveryTransform(cm)
	if err != nil {
		t.Fatal(err)
	}
	if out2 != cm {
		t.Error("unknown type not passed through unchanged")
	}
}
