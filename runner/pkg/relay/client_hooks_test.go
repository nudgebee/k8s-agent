package relay

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The relay_connected gauge is driven entirely by these two hooks. If
// OnDisconnect is ever missed the gauge latches at 1 and reports a healthy
// relay on an agent that has lost its session — worse than the zero it used
// to report, so both edges are pinned here.
func TestClient_ConnectAndDisconnectHooksBracketTheSession(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		var g Greeting
		_ = conn.ReadJSON(&g)
		// Drop the session immediately so the disconnect edge fires.
		_ = conn.Close()
	}))
	defer srv.Close()

	var (
		mu     sync.Mutex
		events []string
	)
	record := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, s)
	}

	client := NewClient(Config{
		URL:            "ws" + strings.TrimPrefix(srv.URL, "http"),
		AuthSecretKey:  "test-secret",
		ReconnectDelay: 50 * time.Millisecond,
		Logger:         slog.Default(),
		OnConnect:      func() { record("connect") },
		OnDisconnect:   func() { record("disconnect") },
		OnReconnect:    func() { record("reconnect") },
	}, func(context.Context, []byte, SendFunc) {})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = client.Run(ctx); close(done) }()

	// Wait for at least one full connect -> disconnect -> reconnect cycle.
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()

	if len(got) < 3 {
		t.Fatalf("hook events = %v; want at least connect, disconnect, reconnect", got)
	}
	if got[0] != "connect" || got[1] != "disconnect" {
		t.Errorf("first two hook events = %v; want [connect disconnect]", got[:2])
	}
	if got[2] != "reconnect" {
		t.Errorf("third hook event = %q; want reconnect", got[2])
	}

	// Every connect must be matched by a disconnect: the gauge must not be
	// left at 1 when the loop is between sessions.
	var open int
	for _, e := range got {
		switch e {
		case "connect":
			open++
		case "disconnect":
			open--
		}
		if open < 0 || open > 1 {
			t.Fatalf("connect/disconnect became unbalanced (%d) in %v", open, got)
		}
	}
}

// A handshake that never establishes a session must not emit a connect edge,
// or the gauge reads 1 for an agent that never reached the relay.
func TestClient_NoConnectHookWhenHandshakeFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	var mu sync.Mutex
	connects := 0

	client := NewClient(Config{
		URL:            "ws" + strings.TrimPrefix(srv.URL, "http"),
		AuthSecretKey:  "test-secret",
		ReconnectDelay: 30 * time.Millisecond,
		Logger:         slog.Default(),
		OnConnect: func() {
			mu.Lock()
			connects++
			mu.Unlock()
		},
	}, func(context.Context, []byte, SendFunc) {})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_ = client.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if connects != 0 {
		t.Errorf("OnConnect fired %d times on a failing handshake; want 0", connects)
	}
}
