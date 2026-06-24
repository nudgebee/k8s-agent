package scanners

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func managedJob(name string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels: map[string]string{
				managedByLabel:    managedByValue,
				orchestratorLabel: orchestratorValue,
			},
		},
	}
}

func completedAt(j *batchv1.Job, t time.Time) *batchv1.Job {
	ct := metav1.NewTime(t)
	j.Status.CompletionTime = &ct
	j.Status.Conditions = []batchv1.JobCondition{{
		Type:               batchv1.JobComplete,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: ct,
	}}
	return j
}

func failedAt(j *batchv1.Job, t time.Time) *batchv1.Job {
	// Failed Jobs carry no CompletionTime — only the terminal condition's time.
	j.Status.Conditions = []batchv1.JobCondition{{
		Type:               batchv1.JobFailed,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(t),
	}}
	return j
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReap_DeletesFinishedJobsPastGrace(t *testing.T) {
	now := time.Now()
	grace := time.Duration(reapGraceSeconds) * time.Second

	staleComplete := completedAt(managedJob("trivy-image-scan-old"), now.Add(-grace-time.Minute))
	staleFailed := failedAt(managedJob("trivy-image-scan-failed"), now.Add(-grace-time.Minute))
	freshComplete := completedAt(managedJob("trivy-image-scan-fresh"), now.Add(-time.Second))
	running := managedJob("trivy-image-scan-running") // no terminal condition

	r := NewRunner(fake.NewClientset(staleComplete, staleFailed, freshComplete, running), "ns", "sa")

	if n := r.reapFinishedJobs(context.Background(), discardLogger(), now); n != 2 {
		t.Fatalf("reaped = %d; want 2 (both stale jobs)", n)
	}

	remaining, err := r.Client.BatchV1().Jobs("ns").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, j := range remaining.Items {
		got[j.Name] = true
	}
	if got["trivy-image-scan-old"] || got["trivy-image-scan-failed"] {
		t.Errorf("stale jobs not deleted: %v", got)
	}
	if !got["trivy-image-scan-fresh"] || !got["trivy-image-scan-running"] {
		t.Errorf("fresh/running jobs must survive: %v", got)
	}
}

func TestReap_IgnoresUnmanagedJobs(t *testing.T) {
	now := time.Now()
	old := metav1.NewTime(now.Add(-24 * time.Hour))

	// A finished Job that is NOT ours (e.g. a customer CronJob) — must be left alone.
	customer := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "customer-backup", Namespace: "ns"},
		Status: batchv1.JobStatus{
			CompletionTime: &old,
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: old,
			}},
		},
	}
	r := NewRunner(fake.NewClientset(customer), "ns", "sa")

	if n := r.reapFinishedJobs(context.Background(), discardLogger(), now); n != 0 {
		t.Fatalf("reaped = %d; want 0 (unmanaged job must be untouched)", n)
	}
	if _, err := r.Client.BatchV1().Jobs("ns").Get(context.Background(), "customer-backup", metav1.GetOptions{}); err != nil {
		t.Errorf("customer job was deleted: %v", err)
	}
}

func TestDeleteJobCascade_BackgroundPropagation(t *testing.T) {
	job := completedAt(managedJob("trivy-image-scan-x"), time.Now())
	r := NewRunner(fake.NewClientset(job), "ns", "sa")

	if err := r.deleteJobCascade(context.Background(), "trivy-image-scan-x"); err != nil {
		t.Fatalf("deleteJobCascade: %v", err)
	}
	if _, err := r.Client.BatchV1().Jobs("ns").Get(context.Background(), "trivy-image-scan-x", metav1.GetOptions{}); err == nil {
		t.Error("job still present after cascade delete")
	}
}
