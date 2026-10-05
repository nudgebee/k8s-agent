package podshell

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestManager_HandleStartRequiresK8s — start without a K8s client returns
// 503 with a configuration-hint message, not a panic.
func TestManager_HandleStartRequiresK8s(t *testing.T) {
	m := &Manager{sessions: map[string]*session{}}
	resp, status := m.Handle(context.Background(), &Request{Action: "start", Name: "p", Namespace: "ns"})
	if status != 503 {
		t.Errorf("status = %d; want 503 (no K8s client)", status)
	}
	if !strings.Contains(resp.Error, "K8s") {
		t.Errorf("error = %q; want it to mention K8s", resp.Error)
	}
}

// TestManager_HandleStartValidatesInput — start without name/namespace gets
// a 400 with a clear error string.
func TestManager_HandleStartValidatesInput(t *testing.T) {
	m := &Manager{sessions: map[string]*session{}}
	resp, status := m.Handle(context.Background(), &Request{Action: "start"})
	if status != 400 || !strings.Contains(resp.Error, "name and namespace") {
		t.Errorf("got status=%d resp=%+v; want 400 + 'name and namespace required'", status, resp)
	}
}

// TestManager_HandleUnknownSession — exec/read/close against a missing
// session_id returns Exit=true so the UI reconnects.
func TestManager_HandleUnknownSession(t *testing.T) {
	m := &Manager{sessions: map[string]*session{}}
	for _, action := range []string{"exec", "read", "close"} {
		resp, status := m.Handle(context.Background(), &Request{Action: action, SessionID: "missing"})
		if status != 200 {
			t.Errorf("%s status = %d; want 200", action, status)
		}
		if !resp.Exit {
			t.Errorf("%s exit = false; want true (session gone)", action)
		}
	}
}

// TestManager_InvalidAction — unknown action gets 400 + "Invalid action".
func TestManager_InvalidAction(t *testing.T) {
	m := &Manager{sessions: map[string]*session{}}
	resp, status := m.Handle(context.Background(), &Request{Action: "yolo", SessionID: "s"})
	if status != 400 || resp.Error != "Invalid action" {
		t.Errorf("got status=%d resp=%+v; want 400 'Invalid action'", status, resp)
	}
}

// TestSession_DrainPushesAndResetsBuffer — drain returns whatever the
// reader goroutine wrote and clears the buffer for the next call. UI
// polls read() periodically and expects each call to return only new
// bytes.
func TestSession_DrainPushesAndResetsBuffer(t *testing.T) {
	s := &session{}
	w := &outWriter{s: s}

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("world\n")); err != nil {
		t.Fatal(err)
	}
	got, closed := s.drain()
	if got != "hello\nworld\n" {
		t.Errorf("drain = %q; want both writes", got)
	}
	if closed {
		t.Error("closed = true on a fresh session")
	}
	got, _ = s.drain()
	if got != "" {
		t.Errorf("second drain = %q; want empty (buffer cleared)", got)
	}
}

// TestManager_ReapTimesOutIdleSessions — sessions older than IdleTimeout
// are closed by the cleanup loop.
func TestManager_ReapTimesOutIdleSessions(t *testing.T) {
	m := &Manager{
		sessions:    map[string]*session{},
		idleTimeout: 50 * time.Millisecond,
	}
	old := &session{id: "old", lastUsed: time.Now().Add(-1 * time.Second)}
	fresh := &session{id: "fresh", lastUsed: time.Now()}
	m.sessions[old.id] = old
	m.sessions[fresh.id] = fresh

	m.reap()

	if _, ok := m.sessions["old"]; ok {
		t.Error("old session was not reaped")
	}
	if _, ok := m.sessions["fresh"]; !ok {
		t.Error("fresh session was reaped (shouldn't be)")
	}
	if !old.closed {
		t.Error("expired session not flagged closed")
	}
}

// podWith builds a Running pod with the given containers and annotations.
func podWith(annotations map[string]string, containers ...string) *corev1.Pod {
	cs := make([]corev1.Container, 0, len(containers))
	for _, name := range containers {
		cs = append(cs, corev1.Container{Name: name})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "dev", Annotations: annotations},
		Spec:       corev1.PodSpec{Containers: cs},
	}
}

// TestResolveContainer — the exec subresource refuses to upgrade a
// multi-container pod unless a container is named, so every start must resolve
// one. Mirrors kubectl: explicit choice, then the default-container
// annotation, then first in spec order.
func TestResolveContainer(t *testing.T) {
	tests := []struct {
		name        string
		pod         *corev1.Pod
		want        string
		expect      string
		expectError string
	}{
		{
			name:   "single container needs no hint",
			pod:    podWith(nil, "app"),
			expect: "app",
		},
		{
			name:   "multi-container defaults to the first in spec order",
			pod:    podWith(nil, "nextjs-static-init", "app", "nginx"),
			expect: "nextjs-static-init",
		},
		{
			name:   "default-container annotation wins over spec order",
			pod:    podWith(map[string]string{defaultContainerAnnotation: "app"}, "nextjs-static-init", "app", "nginx"),
			expect: "app",
		},
		{
			name:   "stale annotation falls back rather than failing",
			pod:    podWith(map[string]string{defaultContainerAnnotation: "removed"}, "app", "nginx"),
			expect: "app",
		},
		{
			name:   "explicit request wins over both",
			pod:    podWith(map[string]string{defaultContainerAnnotation: "app"}, "app", "nginx"),
			want:   "nginx",
			expect: "nginx",
		},
		{
			name:        "unknown request is rejected with the available names",
			pod:         podWith(nil, "app", "nginx"),
			want:        "sidecar",
			expectError: "choose one of: app, nginx",
		},
		{
			name:        "pod with no containers is an error, not a panic",
			pod:         podWith(nil),
			expectError: "no containers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveContainer(tt.pod, tt.want)
			if tt.expectError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.expectError) {
					t.Fatalf("err = %v; want it to contain %q", err, tt.expectError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.expect {
				t.Errorf("container = %q; want %q", got, tt.expect)
			}
		})
	}
}

// TestContainerNames — the start response advertises what else the user could
// have attached to, so a client can offer a switcher without another API call.
func TestContainerNames(t *testing.T) {
	got := containerNames(podWith(nil, "nextjs-static-init", "app", "nginx"))
	want := []string{"nextjs-static-init", "app", "nginx"}
	if len(got) != len(want) {
		t.Fatalf("containerNames = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("containerNames = %v; want %v (spec order)", got, want)
		}
	}
}

// TestResolveContainerIgnoresInitContainers — init containers have terminated by
// the time the pod is Running, so exec'ing into one only fails confusingly. They
// must never be picked as the default, nor be selectable by name.
func TestResolveContainerIgnoresInitContainers(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "dev"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "nextjs-static-init"}},
			Containers:     []corev1.Container{{Name: "app"}, {Name: "nginx"}},
		},
	}

	got, err := resolveContainer(pod, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "app" {
		t.Errorf("default container = %q; want %q (first non-init container)", got, "app")
	}

	if _, err := resolveContainer(pod, "nextjs-static-init"); err == nil {
		t.Error("resolveContainer accepted an init container; want it rejected")
	}

	if names := containerNames(pod); len(names) != 2 {
		t.Errorf("containerNames = %v; want only the two non-init containers", names)
	}
}
