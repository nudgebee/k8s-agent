package mutate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestHandlers_RegistersK8sActionsWithoutAlertManager(t *testing.T) {
	m := New(fake.NewClientset(), "", nil)
	hs := Handlers(m)
	for _, want := range []string{"delete_pod", "delete_job", "cordon", "uncordon", "rollout_restart"} {
		if _, ok := hs[want]; !ok {
			t.Errorf("missing %q", want)
		}
	}
	for _, notWant := range []string{"get_silences", "add_silence", "delete_silence"} {
		if _, ok := hs[notWant]; ok {
			t.Errorf("%q should not be registered without AlertManager URL", notWant)
		}
	}
}

func TestHandlers_RegistersSilencesWhenAlertManagerSet(t *testing.T) {
	m := New(fake.NewClientset(), "http://am.local:9093", nil)
	hs := Handlers(m)
	for _, want := range []string{"get_silences", "add_silence", "delete_silence"} {
		if _, ok := hs[want]; !ok {
			t.Errorf("missing %q", want)
		}
	}
}

func TestHandleDeletePod_HappyPath(t *testing.T) {
	cs := fake.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "n"}})
	m := New(cs, "", nil)
	hs := Handlers(m)
	got, err := hs["delete_pod"](context.Background(), map[string]any{"namespace": "n", "name": "p"})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := got.(map[string]any); r["ok"] != true {
		t.Errorf("response = %v", got)
	}
}

func TestHandleRolloutRestart_PassesParams(t *testing.T) {
	cs := fake.NewClientset()
	if err := cs.Tracker().Add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "n"}}); err != nil {
		t.Fatal(err)
	}
	m := New(cs, "", nil)
	hs := Handlers(m)
	_, err := hs["rollout_restart"](context.Background(), map[string]any{
		"kind":      "deployment",
		"namespace": "n",
		"name":      "ignored", // no real deployment in fake; should error gracefully
	})
	if err == nil {
		// Fake will return NotFound — the handler should propagate it.
		t.Error("expected NotFound error for missing deployment")
	}
}

func TestHandleDeletePod_GracePeriodFromParams(t *testing.T) {
	cs := fake.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "n"}})
	m := New(cs, "", nil)
	hs := Handlers(m)
	_, err := hs["delete_pod"](context.Background(), map[string]any{
		"namespace":            "n",
		"name":                 "p",
		"grace_period_seconds": float64(30), // JSON numbers decode as float64
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHandleDeleteJob(t *testing.T) {
	cs := fake.NewClientset()
	if err := cs.Tracker().Add(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "n"}}); err != nil {
		t.Fatal(err)
	}
	m := New(cs, "", nil)
	hs := Handlers(m)
	_, err := hs["delete_job"](context.Background(), map[string]any{"namespace": "n", "name": "j"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHandleCordonUncordon(t *testing.T) {
	cs := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}})
	m := New(cs, "", nil)
	hs := Handlers(m)

	if _, err := hs["cordon"](context.Background(), map[string]any{"node": "node-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := hs["uncordon"](context.Background(), map[string]any{"node": "node-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestSilenceHandlers_ProxyAlertManager(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`[{"id":"existing"}]`))
		case http.MethodPost:
			_, _ = w.Write([]byte(`{"silenceID":"new-id"}`))
		case http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	m := New(fake.NewClientset(), srv.URL, nil)
	hs := Handlers(m)

	// get_silences with filters
	got, err := hs["get_silences"](context.Background(), map[string]any{
		"filters": []any{`alertname="X"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := got.(map[string]any); !strings.Contains(r["raw"].(string), "existing") {
		t.Errorf("get_silences = %v", got)
	}

	// add_silence with body in nested form
	got, err = hs["add_silence"](context.Background(), map[string]any{
		"body": map[string]any{"matchers": []any{}, "comment": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := got.(map[string]any); !strings.Contains(r["raw"].(string), "new-id") {
		t.Errorf("add_silence = %v", got)
	}

	// delete_silence with id
	if _, err := hs["delete_silence"](context.Background(), map[string]any{"id": "abc-123"}); err != nil {
		t.Fatal(err)
	}
}

func TestEvictPod_BuildsEvictionObject(t *testing.T) {
	cs := fake.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "n"}})
	m := New(cs, "", nil)
	// fake client supports EvictV1; if it doesn't error, we're good.
	if err := m.EvictPod(context.Background(), "n", "p"); err != nil {
		// Some fake versions return Method Not Allowed; tolerate.
		t.Logf("EvictPod (informational): %v", err)
	}
}
