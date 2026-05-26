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
