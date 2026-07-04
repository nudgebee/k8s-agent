package scanners

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fixtures: a workload pod in "app" with a pod-level pull secret + a SA-level
// pull secret, plus a non-registry secret that must be ignored.
func pullSecretFixtures() *fake.Clientset {
	return fake.NewClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app"},
			Spec: corev1.PodSpec{
				ServiceAccountName: "web-sa",
				ImagePullSecrets:   []corev1.LocalObjectReference{{Name: "reg-pod"}, {Name: "not-a-registry"}},
			},
		},
		&corev1.ServiceAccount{
			ObjectMeta:       metav1.ObjectMeta{Name: "web-sa", Namespace: "app"},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "reg-sa"}},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "reg-pod", Namespace: "app"},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{".dockerconfigjson": []byte("{}")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "reg-sa", Namespace: "app"},
			Type:       corev1.SecretTypeDockercfg,
			Data:       map[string][]byte{".dockercfg": []byte("{}")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "not-a-registry", Namespace: "app"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"token": []byte("x")},
		},
	)
}

func scheduleWithPullFrom(t *testing.T, r *Runner) {
	t.Helper()
	_, err := r.handleScheduleJob(context.Background(), map[string]any{
		"spec": JobSpec{
			NamePrefix:           "trivy-image-scan",
			Image:                "registry.private.example.com/web:v1",
			ImagePullSecretsFrom: &PodRef{Namespace: "app", Name: "web"},
		},
	})
	if err != nil {
		t.Fatalf("handleScheduleJob: %v", err)
	}
}

func TestAutoCopyPullSecrets_CopiesRegistrySecretsAndAttaches(t *testing.T) {
	cs := pullSecretFixtures()
	r := NewRunner(cs, "scan-ns", "scan-sa")
	r.AutoCopyPullSecrets = true

	scheduleWithPullFrom(t, r)

	// Both registry secrets (pod-level + SA-level) copied into the scanner ns;
	// the opaque one ignored.
	copied, _ := cs.CoreV1().Secrets("scan-ns").List(context.Background(), metav1.ListOptions{})
	if len(copied.Items) != 2 {
		t.Fatalf("copied secrets = %d; want 2 (reg-pod + reg-sa, opaque skipped)", len(copied.Items))
	}
	for _, s := range copied.Items {
		if s.Type != corev1.SecretTypeDockerConfigJson && s.Type != corev1.SecretTypeDockercfg {
			t.Errorf("copied a non-registry secret: %s (%s)", s.Name, s.Type)
		}
		if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Kind != "Job" {
			t.Errorf("copied secret %s missing Job ownerReference for GC: %+v", s.Name, s.OwnerReferences)
		}
	}

	// The Job's pod references the copies.
	jobs, _ := cs.BatchV1().Jobs("scan-ns").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatalf("jobs = %d; want 1", len(jobs.Items))
	}
	if got := len(jobs.Items[0].Spec.Template.Spec.ImagePullSecrets); got != 2 {
		t.Errorf("job imagePullSecrets = %d; want 2", got)
	}
	// The Job is created suspended (so its pod can't pull before the copied
	// credentials exist) and must be resumed once they do — it must not be left
	// suspended, or the scan never runs.
	if s := jobs.Items[0].Spec.Suspend; s != nil && *s {
		t.Errorf("job left suspended after schedule; resume did not run")
	}
}

func TestAutoCopyPullSecrets_DisabledIgnoresField(t *testing.T) {
	cs := pullSecretFixtures()
	r := NewRunner(cs, "scan-ns", "scan-sa") // AutoCopyPullSecrets defaults false

	scheduleWithPullFrom(t, r)

	copied, _ := cs.CoreV1().Secrets("scan-ns").List(context.Background(), metav1.ListOptions{})
	if len(copied.Items) != 0 {
		t.Errorf("auto-copy off must read/copy no secrets; got %d", len(copied.Items))
	}
	jobs, _ := cs.BatchV1().Jobs("scan-ns").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 || len(jobs.Items[0].Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Errorf("auto-copy off must leave the Job's imagePullSecrets empty")
	}
}
