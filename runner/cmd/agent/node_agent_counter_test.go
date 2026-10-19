package main

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestNodeAgentCounter(t *testing.T) {
	cutoff := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	pod := func(name string, labels map[string]string, phase corev1.PodPhase, started time.Time) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agent", Labels: labels},
			Status:     corev1.PodStatus{Phase: phase, StartTime: &metav1.Time{Time: started}},
		}
	}
	na := map[string]string{"app": "nudgebee-node-agent"}
	cs := fake.NewSimpleClientset(
		pod("na-1", na, corev1.PodRunning, cutoff.Add(-time.Hour)),
		pod("na-2", na, corev1.PodRunning, cutoff.Add(-time.Minute)),
		pod("na-new", na, corev1.PodRunning, cutoff.Add(time.Minute)), // started after: not expected yet
		pod("other", map[string]string{"app": "web"}, corev1.PodRunning, cutoff.Add(-time.Hour)),
	)
	count := nodeAgentCounter(cs)
	got, err := count(context.Background(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Errorf("count = %d; want 2", got)
	}

	// Within the TTL the pod list is reused, even for a later cutoff.
	if got, _ := count(context.Background(), cutoff.Add(time.Hour)); got != 3 {
		t.Errorf("count after cutoff+1h = %d; want 3", got)
	}
	if lists := len(cs.Actions()); lists != 1 {
		t.Errorf("pod LIST calls = %d; want 1", lists)
	}
}
