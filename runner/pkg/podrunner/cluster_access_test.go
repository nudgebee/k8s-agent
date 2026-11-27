package podrunner

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// createdPod returns the pod Handle created; Handle deletes it on the way out,
// so it is read from the recorded create action rather than the store.
func createdPod(t *testing.T, cs *fake.Clientset) *corev1.Pod {
	t.Helper()
	for _, a := range cs.Actions() {
		if c, ok := a.(k8stesting.CreateAction); ok && a.GetResource().Resource == "pods" {
			return c.GetObject().(*corev1.Pod)
		}
	}
	t.Fatal("no pod was created")
	return nil
}

func podCreated(cs *fake.Clientset) bool {
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" && a.GetResource().Resource == "pods" {
			return true
		}
	}
	return false
}

func scriptParams(access any) map[string]any {
	p := map[string]any{"image": "busybox:1.36", "command": "echo hi"}
	if access != nil {
		p["cluster_access"] = access
	}
	return p
}

func TestHandle_ClusterAccess_PodIdentity(t *testing.T) {
	cases := []struct {
		name      string
		access    any
		wantSA    string
		wantToken bool
	}{
		{"absent → reader with token", nil, "runner-sa", true},
		{"empty → reader with token", "", "runner-sa", true},
		{"read → reader with token", "read", "runner-sa", true},
		{"none → reader without token", "none", "runner-sa", false},
		{"write → writer with token", "write", "writer-sa", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, cs := newTestRunner(nil)
			r.WriteServiceAccount = "writer-sa"
			if _, err := r.Handle(context.Background(), scriptParams(tc.access)); err != nil {
				t.Fatalf("Handle: %v", err)
			}
			pod := createdPod(t, cs)
			if got := pod.Spec.ServiceAccountName; got != tc.wantSA {
				t.Errorf("serviceAccountName = %q; want %q", got, tc.wantSA)
			}
			mount := pod.Spec.AutomountServiceAccountToken
			if mount == nil || *mount != tc.wantToken {
				t.Errorf("automountServiceAccountToken = %v; want %v", mount, tc.wantToken)
			}
		})
	}
}

func TestHandle_ClusterAccess_Refusals(t *testing.T) {
	cases := []struct {
		name     string
		access   any
		writerSA string
		wantCode int
	}{
		{"write without a writer account", "write", "", 403},
		{"unknown value", "admin", "writer-sa", 400},
		{"non-string value", true, "writer-sa", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, cs := newTestRunner(nil)
			r.WriteServiceAccount = tc.writerSA
			out, err := r.Handle(context.Background(), scriptParams(tc.access))
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			m, _ := out.(map[string]any)
			if m["success"] != false || m["error_code"] != tc.wantCode {
				t.Errorf("response = %v; want success=false error_code=%d", m, tc.wantCode)
			}
			if podCreated(cs) {
				t.Error("a pod was created for a refused request")
			}
		})
	}
}

func TestIsWriteRequest(t *testing.T) {
	cases := []struct {
		params map[string]any
		want   bool
	}{
		{map[string]any{}, false},
		{map[string]any{"cluster_access": "read"}, false},
		{map[string]any{"cluster_access": "none"}, false},
		{map[string]any{"cluster_access": "write"}, true},
		// Unparseable values count as writes so they cannot skip the signature check.
		{map[string]any{"cluster_access": "WRITE"}, true},
		{map[string]any{"cluster_access": 1}, true},
	}
	for _, tc := range cases {
		if got := IsWriteRequest(tc.params); got != tc.want {
			t.Errorf("IsWriteRequest(%v) = %v; want %v", tc.params, got, tc.want)
		}
	}
}
