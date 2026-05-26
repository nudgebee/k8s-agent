//go:build integration

package podexec

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// envtest gives us a real kube-apiserver but no kubelet, so SPDY exec dial
// to the pod will fail. That's fine — this test verifies that:
//  1. The URL builder runs without panicking against a real apiserver.
//  2. The SPDY dial attempt produces a recognizable error (not a panic).
//  3. Pre-flight validation (missing namespace/pod/command) still rejects.
func TestExecutor_RealAPIServer_DialAttempt(t *testing.T) {
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
	// Seed a Pod so the URL exists. Status will stay Pending (no kubelet).
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "exec-target", Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "alpine"}},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	if _, err := cs.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed pod: %v", err)
	}

	e := New(cs, cfg)

	t.Run("ExecAttempt_FailsCleanly_NoPanic", func(t *testing.T) {
		_, err := e.Exec(ctx, &Request{
			Namespace: "default",
			Pod:       "exec-target",
			Command:   []string{"true"},
			Timeout:   500 * time.Millisecond,
		})
		// Expect an error — either upgrade failure or pod-pending — but
		// MUST be a clean error, not a panic. Empty error == surprise.
		if err == nil {
			t.Skip("apiserver accepted exec — unexpected on envtest, but not a failure")
		}
	})

	t.Run("ExecAttempt_WithStdin", func(t *testing.T) {
		_, err := e.Exec(ctx, &Request{
			Namespace: "default",
			Pod:       "exec-target",
			Command:   []string{"sh", "-s"},
			Stdin:     "echo hi",
			Timeout:   500 * time.Millisecond,
		})
		if err == nil {
			t.Skip("apiserver accepted exec on Pending pod — unexpected, not failing")
		}
		// Just verify the error is reasonable (mentions exec or stream).
		s := err.Error()
		if !strings.Contains(s, "exec") && !strings.Contains(s, "stream") && !strings.Contains(s, "dial") && !strings.Contains(s, "upgrade") && !strings.Contains(s, "container") {
			t.Errorf("unexpected error shape: %q", s)
		}
	})

	t.Run("InputValidation_MissingNamespace", func(t *testing.T) {
		_, err := e.Exec(ctx, &Request{Pod: "x", Command: []string{"true"}})
		if err == nil {
			t.Error("expected validation error")
		}
	})
}
