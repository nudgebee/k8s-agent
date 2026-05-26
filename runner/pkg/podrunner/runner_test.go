package podrunner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newTestRunner builds a Runner against a fake clientset. The reactor pre-
// seeds a Succeeded pod whenever Create is called, so waitAndReadLogs
// returns immediately and the test exercises the full happy path without
// real timing.
func newTestRunner(now func() time.Time) (*Runner, *fake.Clientset) {
	cs := fake.NewSimpleClientset()
	// On pod create, retroactively mark it Succeeded so the wait loop
	// terminates on its next Get.
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		create, ok := a.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		pod, ok := create.GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		pod.Status.Phase = corev1.PodSucceeded
		return false, nil, nil // let default reactor persist with the mutation
	})
	r := New(cs, "nb-agent", "runner-sa", "acct-123")
	r.PollInterval = 10 * time.Millisecond
	if now != nil {
		r.Now = now
	}
	return r, cs
}

// extractResponseDict pulls the inner response_dict out of the Finding
// wrapper. Mirrors what api-server's relay.CommandExecutor does so the test
// asserts on the same path production hits.
func extractResponseDict(t *testing.T, out any) map[string]any {
	t.Helper()
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected map response, got %T", out)
	}
	findings, ok := m["findings"].([]any)
	if !ok || len(findings) == 0 {
		t.Fatalf("findings missing or empty: %v", m)
	}
	finding, _ := findings[0].(map[string]any)
	evidence, ok := finding["evidence"].([]any)
	if !ok || len(evidence) == 0 {
		t.Fatalf("evidence missing or empty: %v", finding)
	}
	ev, _ := evidence[0].(map[string]any)
	data, _ := ev["data"].(string)

	var blocks []map[string]any
	if err := json.Unmarshal([]byte(data), &blocks); err != nil {
		t.Fatalf("evidence.data is not a JSON array: %v (%q)", err, data)
	}
	if len(blocks) == 0 {
		t.Fatalf("no blocks in evidence.data")
	}
	innerJSON, _ := blocks[0]["data"].(string)
	var dict map[string]any
	if err := json.Unmarshal([]byte(innerJSON), &dict); err != nil {
		t.Fatalf("block.data is not a JSON object: %v (%q)", err, innerJSON)
	}
	return dict
}

func TestHandle_RejectsMissingImage(t *testing.T) {
	r, _ := newTestRunner(nil)
	out, err := r.Handle(context.Background(), map[string]any{
		"command": "echo hi",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, _ := out.(map[string]any)
	if success, _ := m["success"].(bool); success {
		t.Errorf("expected success=false for missing image, got %v", m)
	}
}

func TestHandle_RejectsMissingCommand(t *testing.T) {
	r, _ := newTestRunner(nil)
	out, err := r.Handle(context.Background(), map[string]any{
		"image": "busybox",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, _ := out.(map[string]any)
	if success, _ := m["success"].(bool); success {
		t.Errorf("expected success=false for missing command, got %v", m)
	}
}

func TestHandle_HappyPath_BuildsExpectedPodAndConfigMap(t *testing.T) {
	r, cs := newTestRunner(nil)

	out, err := r.Handle(context.Background(), map[string]any{
		"image":    "clickhouse/clickhouse-server:latest",
		"command":  `clickhouse client --query "SELECT 1"`,
		"pod_name": "ch-test-pod",
		"secret":   "ch-creds",
		"env_from_secret_keys": map[string]any{
			"CLICKHOUSE_USER":     "CLICKHOUSE_USER",
			"CLICKHOUSE_PASSWORD": "CLICKHOUSE_PASSWORD",
		},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	dict := extractResponseDict(t, out)
	if got, want := dict["type"], "pod_script_run_enricher"; got != want {
		t.Errorf("type = %v; want %v", got, want)
	}
	if dict["pod_name"] != "ch-test-pod" {
		t.Errorf("pod_name not echoed: got %v", dict["pod_name"])
	}
	if dict["secret"] != "ch-creds" {
		t.Errorf("secret not echoed: got %v", dict["secret"])
	}
	if _, ok := dict["response"]; !ok {
		t.Errorf("response field missing: %v", dict)
	}

	// Audit what we sent to the fake apiserver.
	var sawCMCreate, sawPodCreate, sawCMDelete, sawPodDelete bool
	var podCreated *corev1.Pod
	var cmCreated *corev1.ConfigMap
	for _, a := range cs.Actions() {
		switch v := a.(type) {
		case k8stesting.CreateAction:
			switch obj := v.GetObject().(type) {
			case *corev1.Pod:
				sawPodCreate = true
				podCreated = obj
			case *corev1.ConfigMap:
				sawCMCreate = true
				cmCreated = obj
			}
		case k8stesting.DeleteAction:
			switch v.GetResource().Resource {
			case "pods":
				sawPodDelete = true
			case "configmaps":
				sawCMDelete = true
			}
		}
	}
	if !sawCMCreate || !sawPodCreate || !sawPodDelete || !sawCMDelete {
		t.Fatalf("expected create+delete for both pod and configmap; got cm_create=%v pod_create=%v cm_delete=%v pod_delete=%v",
			sawCMCreate, sawPodCreate, sawCMDelete, sawPodDelete)
	}

	// Pod spec invariants.
	if podCreated.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("pod restartPolicy = %v; want Never", podCreated.Spec.RestartPolicy)
	}
	if podCreated.Spec.ServiceAccountName != "runner-sa" {
		t.Errorf("serviceAccountName = %q; want runner-sa", podCreated.Spec.ServiceAccountName)
	}
	c := podCreated.Spec.Containers[0]
	if c.Image != "clickhouse/clickhouse-server:latest" {
		t.Errorf("image = %q", c.Image)
	}
	if len(c.EnvFrom) != 1 || c.EnvFrom[0].SecretRef == nil || c.EnvFrom[0].SecretRef.Name != "ch-creds" {
		t.Errorf("envFrom secret not wired: %+v", c.EnvFrom)
	}
	// ConfigMap holds the base64-encoded script.
	script, ok := cmCreated.Data["script"]
	if !ok {
		t.Fatalf("configmap missing script key: %+v", cmCreated.Data)
	}
	// Plain command was base64-encoded.
	if !strings.Contains(c.Command[2], "base64 -d /mnt/script") {
		t.Errorf("container command does not decode script: %v", c.Command)
	}
	// Don't assert exact base64 — the command flows through the
	// fallback "raw command" branch. Just ensure it's non-empty.
	if script == "" {
		t.Error("script payload is empty")
	}
}

// TestHandle_TreatsValidBase64CommandAsRaw locks in the rule that `command`
// is always raw shell text — never auto-decoded. The string `echo` happens
// to be valid base64 (decodes to 3 bytes of binary garbage); if the
// heuristic ever creeps back, the configmap payload would be base64(garbage)
// instead of base64("echo") and this test would fail.
func TestHandle_TreatsValidBase64CommandAsRaw(t *testing.T) {
	r, cs := newTestRunner(nil)

	// `echo` (4 chars, all-base64-alphabet) is the canonical false-positive
	// the legacy heuristic mishandled.
	cmd := "echo"
	_, err := r.Handle(context.Background(), map[string]any{
		"image":   "busybox",
		"command": cmd,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	want := base64.StdEncoding.EncodeToString([]byte(cmd))
	for _, a := range cs.Actions() {
		if c, ok := a.(k8stesting.CreateAction); ok {
			if cm, ok := c.GetObject().(*corev1.ConfigMap); ok {
				if got := cm.Data["script"]; got != want {
					t.Errorf("configmap script = %q; want %q (raw command, base64-encoded for transport)",
						got, want)
				}
				return
			}
		}
	}
	t.Fatal("no configmap create observed")
}

func TestHandle_SplitsNamespaceFromSecret(t *testing.T) {
	r, cs := newTestRunner(nil)
	_, err := r.Handle(context.Background(), map[string]any{
		"image":   "busybox",
		"command": "echo hi",
		"secret":  "other-ns/db-creds",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range cs.Actions() {
		if c, ok := a.(k8stesting.CreateAction); ok {
			if pod, ok := c.GetObject().(*corev1.Pod); ok {
				if pod.Namespace != "other-ns" {
					t.Errorf("pod namespace = %q; want other-ns (from ns/secret split)", pod.Namespace)
				}
				if pod.Spec.Containers[0].EnvFrom[0].SecretRef.Name != "db-creds" {
					t.Errorf("secret name not split: %v", pod.Spec.Containers[0].EnvFrom)
				}
				return
			}
		}
	}
	t.Error("no pod create observed")
}

func TestHandle_UnrecoverableContainerError_SurfacedAsFailure(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		create, _ := a.(k8stesting.CreateAction)
		pod, _ := create.GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodPending
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{
			{
				Name: "runner-container",
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Reason:  "ImagePullBackOff",
						Message: "pull access denied",
					},
				},
			},
		}
		return false, nil, nil
	})
	r := New(cs, "nb-agent", "runner-sa", "acct")
	r.PollInterval = 5 * time.Millisecond

	out, err := r.Handle(context.Background(), map[string]any{
		"image":   "bogus/image:latest",
		"command": "echo hi",
	})
	if err != nil {
		t.Fatalf("Handle returned err (expected wrapped failure): %v", err)
	}
	dict := extractResponseDict(t, out)
	resp, _ := dict["response"].(string)
	if !strings.Contains(resp, "ImagePullBackOff") {
		t.Errorf("expected ImagePullBackOff in response, got %q", resp)
	}
}

func TestHandle_TimeoutWhenPodNeverCompletes(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		create, _ := a.(k8stesting.CreateAction)
		pod, _ := create.GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodRunning
		return false, nil, nil
	})
	r := New(cs, "nb-agent", "runner-sa", "acct")
	r.PollInterval = 5 * time.Millisecond
	// Advance synthetic clock past the 1-minute default on the second tick.
	calls := 0
	start := time.Now()
	r.Now = func() time.Time {
		calls++
		if calls > 2 {
			return start.Add(2 * time.Hour)
		}
		return start
	}

	out, err := r.Handle(context.Background(), map[string]any{
		"image":   "busybox",
		"command": "sleep 9999",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	dict := extractResponseDict(t, out)
	resp, _ := dict["response"].(string)
	if !strings.Contains(resp, "did not complete") {
		t.Errorf("expected timeout message, got %q", resp)
	}
}

// Guard: detectStringMap should never crash on weird input shapes the JSON
// decoder might hand us (slice, nil, int).
func TestDecodeStringMap_DefensiveShapes(t *testing.T) {
	cases := []any{nil, "scalar", 42, []any{"x"}}
	for _, c := range cases {
		m := decodeStringMap(c)
		if m == nil || len(m) != 0 {
			t.Errorf("decodeStringMap(%#v) = %v; want empty map", c, m)
		}
	}
}

// Sanity: the only registered action is pod_script_run_enricher.
func TestHandlers_RegistersOnlyExpectedAction(t *testing.T) {
	hs := Handlers(&Runner{})
	if _, ok := hs["pod_script_run_enricher"]; !ok {
		t.Error("pod_script_run_enricher must be registered")
	}
	if len(hs) != 1 {
		t.Errorf("expected 1 handler, got %d (%v)", len(hs), hs)
	}
}

// Static check: errors package isn't unused (used for one branch in runner.go
// — keep it referenced so a future test refactor doesn't churn imports).
var _ = errors.New
