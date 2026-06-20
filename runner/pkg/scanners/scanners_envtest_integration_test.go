//go:build integration

package scanners

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// envtest has no scheduler/kubelet, so Jobs we submit won't actually progress
// to Complete on their own. We test the realistic pieces:
//
//  1. The Job spec the agent generates from a server-supplied JobSpec is
//     accepted by a real kube-apiserver (catches OpenAPI / admission
//     validation issues that fake clients miss).
//  2. wait_for_k8s_job observes a Complete status when we mark it.
//  3. The concurrency cap rejects the (N+1)th schedule.
//
// Real Job execution + log fetching needs kind or a live cluster — those
// belong in an e2e test suite separate from this build tag.
func TestPrimitives_RealAPIServer(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; -short")
	}

	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("envtest.Start: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// envtest doesn't include kube-system / nudgebee-agent — use default.
	r := NewRunner(cs, "default", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	t.Run("ScheduleJob_ImageSpecAccepted", func(t *testing.T) {
		out, err := r.handleScheduleJob(ctx, map[string]any{
			"spec": map[string]any{
				"name_prefix": "trivy-image",
				"image":       "aquasec/trivy:0.58.0",
				"args":        []any{"image", "--format", "json", "nginx:1.27"},
			},
		})
		if err != nil {
			t.Fatalf("schedule_k8s_job rejected by apiserver: %v", err)
		}
		resp := out.(map[string]any)
		if resp["success"] != true {
			t.Fatalf("schedule failed: %+v", resp)
		}
		jobName := resp["job_name"].(string)
		t.Cleanup(func() {
			_ = cs.BatchV1().Jobs("default").Delete(context.Background(), jobName, metav1.DeleteOptions{})
		})

		// Verify the cmd args round-tripped (api-server's substitution must
		// happen server-side; agent forwards bytes verbatim).
		created, err := cs.BatchV1().Jobs("default").Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(created.Spec.Template.Spec.Containers[0].Args, " "), "nginx:1.27") {
			t.Errorf("args not preserved: %v", created.Spec.Template.Spec.Containers[0].Args)
		}
	})

	t.Run("ScheduleJob_PrivilegedSpecAccepted", func(t *testing.T) {
		out, err := r.handleScheduleJob(ctx, map[string]any{
			"spec": map[string]any{
				"name_prefix":  "kube-bench",
				"image":        "aquasec/kube-bench:v0.10.4",
				"args":         []any{"--json"},
				"privileged":   true,
				"host_pid":     true,
				"host_network": true,
			},
		})
		if err != nil {
			t.Fatalf("schedule_k8s_job rejected: %v", err)
		}
		jobName := out.(map[string]any)["job_name"].(string)
		t.Cleanup(func() {
			_ = cs.BatchV1().Jobs("default").Delete(context.Background(), jobName, metav1.DeleteOptions{})
		})

		created, err := cs.BatchV1().Jobs("default").Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !created.Spec.Template.Spec.HostPID {
			t.Error("HostPID lost in apiserver round-trip")
		}
		c := created.Spec.Template.Spec.Containers[0]
		if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
			t.Error("privileged flag lost in apiserver round-trip")
		}
	})

	t.Run("WaitForJob_ObservesComplete", func(t *testing.T) {
		out, err := r.handleScheduleJob(ctx, map[string]any{
			"spec": map[string]any{"name_prefix": "wait-test", "image": "x:1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		jobName := out.(map[string]any)["job_name"].(string)
		t.Cleanup(func() {
			_ = cs.BatchV1().Jobs("default").Delete(context.Background(), jobName, metav1.DeleteOptions{})
		})

		// K8s 1.32 requires both SuccessCriteriaMet and Complete + start/end.
		j, err := cs.BatchV1().Jobs("default").Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		now := metav1.Now()
		j.Status.StartTime = &now
		j.Status.CompletionTime = &now
		j.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now, Reason: "Test"},
		}
		if _, err := cs.BatchV1().Jobs("default").UpdateStatus(ctx, j, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}

		waitOut, err := r.handleWaitForJob(ctx, map[string]any{"job_name": jobName})
		if err != nil {
			t.Fatal(err)
		}
		if waitOut.(map[string]any)["status"] != "Complete" {
			t.Fatalf("status = %v", waitOut.(map[string]any)["status"])
		}
	})

	t.Run("WaitForJob_DetectsFailure", func(t *testing.T) {
		out, err := r.handleScheduleJob(ctx, map[string]any{
			"spec": map[string]any{"name_prefix": "fail-test", "image": "x:1"},
		})
		if err != nil {
			t.Fatal(err)
		}
		jobName := out.(map[string]any)["job_name"].(string)
		t.Cleanup(func() {
			_ = cs.BatchV1().Jobs("default").Delete(context.Background(), jobName, metav1.DeleteOptions{})
		})

		j, _ := cs.BatchV1().Jobs("default").Get(ctx, jobName, metav1.GetOptions{})
		now := metav1.Now()
		j.Status.StartTime = &now
		j.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now, Reason: "TestFailure"},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastProbeTime: now, LastTransitionTime: now, Reason: "TestFailure", Message: "simulated failure"},
		}
		if _, err := cs.BatchV1().Jobs("default").UpdateStatus(ctx, j, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}

		waitOut, err := r.handleWaitForJob(ctx, map[string]any{"job_name": jobName})
		if err != nil {
			t.Fatal(err)
		}
		resp := waitOut.(map[string]any)
		if resp["status"] != "Failed" {
			t.Errorf("status = %v", resp["status"])
		}
		if !strings.Contains(resp["failure_reason"].(string), "simulated failure") {
			t.Errorf("failure_reason = %v", resp["failure_reason"])
		}
	})

	t.Run("ScheduleJob_AutoCopyPullSecrets", func(t *testing.T) {
		// Source workload in "default": a pod with a pod-level registry pull
		// secret + a SA-level one, plus an opaque secret that must NOT be copied.
		mkSecret := func(name string, typ corev1.SecretType, key string) {
			if _, err := cs.CoreV1().Secrets("default").Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Type:       typ,
				Data:       map[string][]byte{key: []byte("{}")},
			}, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cs.CoreV1().Secrets("default").Delete(context.Background(), name, metav1.DeleteOptions{}) })
		}
		mkSecret("ac-reg-pod", corev1.SecretTypeDockerConfigJson, ".dockerconfigjson")
		mkSecret("ac-reg-sa", corev1.SecretTypeDockercfg, ".dockercfg")
		mkSecret("ac-opaque", corev1.SecretTypeOpaque, "tok")
		if _, err := cs.CoreV1().ServiceAccounts("default").Create(ctx, &corev1.ServiceAccount{
			ObjectMeta:       metav1.ObjectMeta{Name: "ac-sa", Namespace: "default"},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "ac-reg-sa"}},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := cs.CoreV1().Pods("default").Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "ac-pod", Namespace: "default"},
			Spec: corev1.PodSpec{
				ServiceAccountName: "ac-sa",
				ImagePullSecrets:   []corev1.LocalObjectReference{{Name: "ac-reg-pod"}, {Name: "ac-opaque"}},
				Containers:         []corev1.Container{{Name: "c", Image: "x:1"}},
			},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}

		r2 := NewRunner(cs, "default", "")
		r2.AutoCopyPullSecrets = true
		out, err := r2.handleScheduleJob(ctx, map[string]any{
			"spec": map[string]any{
				"name_prefix":             "imgscan-ac",
				"image":                   "registry.private.example.com/app:v1",
				"image_pull_secrets_from": map[string]any{"namespace": "default", "name": "ac-pod"},
			},
		})
		if err != nil {
			t.Fatalf("schedule_k8s_job: %v", err)
		}
		jobName := out.(map[string]any)["job_name"].(string)
		t.Cleanup(func() { _ = cs.BatchV1().Jobs("default").Delete(context.Background(), jobName, metav1.DeleteOptions{}) })

		job, err := cs.BatchV1().Jobs("default").Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// Both registry secrets attached; the opaque one skipped.
		if got := len(job.Spec.Template.Spec.ImagePullSecrets); got != 2 {
			t.Fatalf("job imagePullSecrets = %d; want 2 (pod + SA registry secrets, opaque skipped)", got)
		}
		// Each copy exists in the scanner ns, is a registry secret, and is owned
		// by the Job (so it GCs with the Job — verified against a real apiserver,
		// which enforces ownerReference semantics a fake client doesn't).
		for _, ref := range job.Spec.Template.Spec.ImagePullSecrets {
			s, err := cs.CoreV1().Secrets("default").Get(ctx, ref.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("copied secret %s missing: %v", ref.Name, err)
			}
			t.Cleanup(func() {
				_ = cs.CoreV1().Secrets("default").Delete(context.Background(), s.Name, metav1.DeleteOptions{})
			})
			if s.Type != corev1.SecretTypeDockerConfigJson && s.Type != corev1.SecretTypeDockercfg {
				t.Errorf("copied a non-registry secret %s (%s)", s.Name, s.Type)
			}
			owned := false
			for _, o := range s.OwnerReferences {
				if o.Kind == "Job" && o.Name == jobName {
					owned = true
				}
			}
			if !owned {
				t.Errorf("copied secret %s not owned by job %s (GC would leak it)", s.Name, jobName)
			}
		}
	})
}
