package mutate

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

func newPVC(ns, name, sc, size string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: strPtr(sc),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
}

func expandableSC(name string) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: name},
		AllowVolumeExpansion: boolPtr(true),
	}
}

func TestExpandPVC_HappyPath(t *testing.T) {
	cs := fake.NewClientset(newPVC("data", "db", "fast", "10Gi"), expandableSC("fast"))
	m := New(cs, "", nil)

	// "20.000000Gi" is the %f wire form the api-server sends.
	got, err := m.RightsizePVC(context.Background(), "data", "db", "20.000000Gi")
	if err != nil {
		t.Fatal(err)
	}
	if got.(map[string]any)["success"] != true {
		t.Fatalf("success = %v", got)
	}
	pvc, _ := cs.CoreV1().PersistentVolumeClaims("data").Get(context.Background(), "db", metav1.GetOptions{})
	if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != "20Gi" {
		t.Errorf("requested storage = %s; want 20Gi", q.String())
	}
}

func TestExpandPVC_DownsizeRejected(t *testing.T) {
	cs := fake.NewClientset(newPVC("data", "db", "fast", "20Gi"), expandableSC("fast"))
	m := New(cs, "", nil)
	if _, err := m.RightsizePVC(context.Background(), "data", "db", "10Gi"); err == nil {
		t.Fatal("expected downsize to be rejected")
	}
}

func TestExpandPVC_AlreadyAtSize(t *testing.T) {
	cs := fake.NewClientset(newPVC("data", "db", "fast", "20Gi"), expandableSC("fast"))
	m := New(cs, "", nil)
	got, err := m.RightsizePVC(context.Background(), "data", "db", "20Gi")
	if err != nil {
		t.Fatal(err)
	}
	if got.(map[string]any)["success"] != true {
		t.Errorf("expected no-op success, got %v", got)
	}
}

func TestExpandPVC_ExpansionNotAllowed(t *testing.T) {
	sc := &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: "slow"},
		AllowVolumeExpansion: boolPtr(false),
	}
	cs := fake.NewClientset(newPVC("data", "db", "slow", "10Gi"), sc)
	m := New(cs, "", nil)
	if _, err := m.RightsizePVC(context.Background(), "data", "db", "20Gi"); err == nil {
		t.Fatal("expected error when StorageClass forbids expansion")
	}
}

func TestDeleteVolume_WithClaimRef(t *testing.T) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-1"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Namespace: "data", Name: "db"},
		},
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "data"}}
	cs := fake.NewClientset(pv, pvc)
	m := New(cs, "", nil)

	if _, err := m.DeleteVolume(context.Background(), "pv-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().PersistentVolumes().Get(context.Background(), "pv-1", metav1.GetOptions{}); err == nil {
		t.Error("pv should be deleted")
	}
	if _, err := cs.CoreV1().PersistentVolumeClaims("data").Get(context.Background(), "db", metav1.GetOptions{}); err == nil {
		t.Error("bound pvc should be deleted")
	}
}

func TestDeleteVolume_NoClaimRef(t *testing.T) {
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-2"}}
	cs := fake.NewClientset(pv)
	m := New(cs, "", nil)
	if _, err := m.DeleteVolume(context.Background(), "pv-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().PersistentVolumes().Get(context.Background(), "pv-2", metav1.GetOptions{}); err == nil {
		t.Error("pv should be deleted")
	}
}

func TestDeleteVolume_NotFoundIsSuccess(t *testing.T) {
	cs := fake.NewClientset()
	m := New(cs, "", nil)
	got, err := m.DeleteVolume(context.Background(), "ghost")
	if err != nil {
		t.Fatal(err)
	}
	if got.(map[string]any)["success"] != true {
		t.Errorf("missing pv should be success, got %v", got)
	}
}

func TestHandlers_RegistersPVCRemediation(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	hs := Handlers(m)
	for _, want := range []string{"rightsize_pvc", "volume_delete"} {
		if _, ok := hs[want]; !ok {
			t.Errorf("missing %q (should register with just a typed client)", want)
		}
	}
}
