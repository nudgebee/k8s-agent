package podshell

import (
	"context"
	"strings"
	"testing"
	"time"
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
