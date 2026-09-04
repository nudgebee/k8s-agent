package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Spin up a test WS server, run the client, verify:
//
//  1. Authorization header is the base64 of AuthSecretKey.
//  2. Client sends the greeting on connect.
//  3. Server sends a message; client invokes handler; handler sends response;
//     response arrives at the server.
func TestClient_RoundTrip(t *testing.T) {
	upgrader := websocket.Upgrader{}
	gotGreeting := make(chan Greeting, 1)
	gotResponse := make(chan Response, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "dGVzdC1zZWNyZXQ=" { // base64("test-secret")
			t.Errorf("Authorization header = %q; want base64('test-secret')", r.Header.Get("Authorization"))
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		// 1. Read the greeting.
		var g Greeting
		if err := conn.ReadJSON(&g); err != nil {
			t.Errorf("read greeting: %v", err)
			return
		}
		gotGreeting <- g

		// 2. Send a fake action request.
		req := ExternalActionRequest{
			RequestID: "req-1",
			Body:      ActionRequestBody{ActionName: "ping", Timestamp: 1700000000},
		}
		if err := conn.WriteJSON(req); err != nil {
			t.Errorf("write action: %v", err)
			return
		}

		// 3. Read back the response.
		var resp Response
		if err := conn.ReadJSON(&resp); err != nil {
			t.Errorf("read response: %v", err)
			return
		}
		gotResponse <- resp
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	client := NewClient(Config{
		URL:           wsURL,
		AuthSecretKey: "test-secret",
		Greeting:      Greeting{Version: "test", AgentVersion: "0.0.0"},
	}, func(ctx context.Context, msg []byte, send SendFunc) {
		var req ExternalActionRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			t.Errorf("unmarshal: %v", err)
			return
		}
		_ = send(&Response{
			Action:     "response",
			RequestID:  req.RequestID,
			StatusCode: 200,
			Data:       map[string]any{"pong": true},
		})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	select {
	case g := <-gotGreeting:
		if g.Action != "auth" || g.Version != "test" || g.AgentVersion != "0.0.0" {
			t.Errorf("greeting = %+v; want action=auth version=test agent_version=0.0.0", g)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for greeting")
	}

	select {
	case r := <-gotResponse:
		if r.Action != "response" || r.RequestID != "req-1" || r.StatusCode != 200 {
			t.Errorf("response = %+v; want action=response request_id=req-1 status=200", r)
		}
		data, _ := r.Data.(map[string]any)
		if data["pong"] != true {
			t.Errorf("response data = %v; want {pong:true}", r.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for response")
	}
}

// A relay that stops answering must end the session and trigger a reconnect.
//
// Regression: the client had no read deadline and no pinger, so a half-open
// connection (LB idle timeout, NAT eviction, node partition) parked
// ReadMessage forever. runOnce never returned, so the reconnect loop in Run
// never ran and the agent stayed silently disconnected until a pod restart —
// while the relay, whose own read deadline had expired, reported
// NOT_CONNECTED. Before the fix this test hangs until the test binary's
// deadline.
func TestClient_ReconnectsWhenRelayGoesSilent(t *testing.T) {
	upgrader := websocket.Upgrader{}
	dials := make(chan struct{}, 4)
	// Closed when the test ends, so the silent handlers below return instead
	// of holding srv.Close() open for a fixed sleep.
	testOver := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		dials <- struct{}{}
		var g Greeting
		if err := conn.ReadJSON(&g); err != nil {
			return
		}
		// Go silent: never read again, so the client's pings are never
		// answered, and never write. The connection stays open at the TCP
		// level — exactly the half-open case.
		<-testOver
	}))
	// LIFO: testOver closes first, releasing the handlers, then Close returns.
	defer srv.Close()
	defer close(testOver)

	client := NewClient(Config{
		URL:            "ws" + strings.TrimPrefix(srv.URL, "http"),
		AuthSecretKey:  "test-secret",
		PingInterval:   50 * time.Millisecond,
		PongWait:       300 * time.Millisecond,
		ReconnectDelay: 50 * time.Millisecond,
	}, func(ctx context.Context, msg []byte, send SendFunc) {})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	// Two dials means the first session was detected as dead and redialed.
	for i := 0; i < 2; i++ {
		select {
		case <-dials:
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for dial %d; a silent relay did not end the session", i+1)
		}
	}
}

// Pings from the relay alone must hold the session open: the relay pings
// every 30s and an otherwise idle agent has nothing else to refresh its read
// deadline with.
func TestClient_RelayPingsKeepSessionAlive(t *testing.T) {
	upgrader := websocket.Upgrader{}
	dials := make(chan struct{}, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		dials <- struct{}{}
		var g Greeting
		if err := conn.ReadJSON(&g); err != nil {
			return
		}
		// Drain reads so the client's own pings get gorilla's automatic pong.
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()
		for i := 0; i < 20; i++ {
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second)); err != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}))
	defer srv.Close()

	client := NewClient(Config{
		URL:            "ws" + strings.TrimPrefix(srv.URL, "http"),
		AuthSecretKey:  "test-secret",
		PingInterval:   time.Hour, // never fires; the relay's pings must carry it
		PongWait:       300 * time.Millisecond,
		ReconnectDelay: 10 * time.Millisecond,
	}, func(ctx context.Context, msg []byte, send SendFunc) {})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	select {
	case <-dials:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for the first dial")
	}
	select {
	case <-dials:
		t.Fatal("session was redialed; relay pings did not refresh the read deadline")
	case <-time.After(900 * time.Millisecond):
	}
}
