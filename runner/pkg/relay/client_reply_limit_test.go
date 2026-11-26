package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func bigData(n int) map[string]any { return map[string]any{"out": strings.Repeat("x", n)} }

func decodeReply(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A reply over the limit goes out as a 413 naming the action and the size, for
// the same request, and the hook sees the action.
func TestEncode_ReplyOverLimitBecomes413(t *testing.T) {
	var hooked []string
	c := NewClient(Config{MaxReplyBytes: 1 << 20, OnReplyTooLarge: func(a string) { hooked = append(hooked, a) }}, nil)
	b, err := c.encode(&Response{Action: "response", RequestID: "r1", StatusCode: 200, Data: bigData(3 << 20),
		OutputType: "actions", ActionName: "kubectl_command_executor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 1<<20 {
		t.Fatalf("413 reply is %d bytes", len(b))
	}
	m := decodeReply(t, b)
	if m["status_code"] != float64(413) || m["request_id"] != "r1" || m["action"] != "response" || m["output_type"] != "actions" {
		t.Fatalf("reply = %v", m)
	}
	msg, _ := m["data"].(map[string]any)["error"].(string)
	if !strings.HasPrefix(msg, "kubectl_command_executor reply was 3.0 MiB, over the agent's 1.0 MiB reply limit") {
		t.Errorf("error = %q", msg)
	}
	if _, sent := m["action_name"]; sent {
		t.Error("action name went on the wire")
	}
	if len(hooked) != 1 || hooked[0] != "kubectl_command_executor" {
		t.Errorf("hook calls = %v", hooked)
	}
}

// Grafana and Terminal replies keep their stringified data on the 413 too.
func TestEncode_ProxyReplyOverLimitKeepsStringData(t *testing.T) {
	c := NewClient(Config{MaxReplyBytes: 1000}, nil)
	for _, out := range []string{"Grafana", "Terminal"} {
		b, err := c.encode(&Response{Action: "response", RequestID: "g1", StatusCode: 200, Data: strings.Repeat("y", 5000),
			OutputType: out, ActionName: "grafana_proxy"})
		if err != nil {
			t.Fatal(err)
		}
		m := decodeReply(t, b)
		s, ok := m["data"].(string)
		if !ok || m["status_code"] != float64(413) || m["output_type"] != out {
			t.Fatalf("%s reply = %v", out, m)
		}
		var inner map[string]any
		if err := json.Unmarshal([]byte(s), &inner); err != nil || !strings.Contains(fmt.Sprint(inner["error"]), "grafana_proxy reply was") {
			t.Errorf("%s data = %q (%v)", out, s, err)
		}
	}
}

// Under the limit, with the limit off, and for messages that are not replies,
// the bytes are exactly json.Marshal's.
func TestEncode_UnchangedWhenNotOverLimit(t *testing.T) {
	small := &Response{Action: "response", RequestID: "r2", StatusCode: 200, Data: bigData(10), ActionName: "get_resource"}
	big := &Response{Action: "response", RequestID: "r3", StatusCode: 200, Data: bigData(5000), ActionName: "get_resource"}
	greeting := &Greeting{Action: "auth", Version: strings.Repeat("v", 5000)}
	cases := []struct {
		name  string
		limit int
		v     any
	}{
		{"under limit", 1000, small},
		{"limit off", 0, big},
		{"not a reply", 1000, greeting},
	}
	for _, tc := range cases {
		c := NewClient(Config{MaxReplyBytes: tc.limit}, nil)
		got, err := c.encode(tc.v)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(tc.v)
		if string(got) != string(want) {
			t.Errorf("%s: encoded bytes changed", tc.name)
		}
	}
}

// End to end over a WebSocket: concurrent replies, some over the limit, all
// arrive — the over-limit ones as 413s — and the session stays up.
func TestClient_ConcurrentRepliesWithLimit(t *testing.T) {
	const n = 20
	upgrader := websocket.Upgrader{}
	got := make(chan Response, n)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var g Greeting
		if err := conn.ReadJSON(&g); err != nil {
			return
		}
		for i := 0; i < n; i++ {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"request_id":"r%d"}`, i)))
		}
		for i := 0; i < n; i++ {
			var resp Response
			if err := conn.ReadJSON(&resp); err != nil {
				return
			}
			got <- resp
		}
	}))
	defer srv.Close()

	c := NewClient(Config{URL: "ws" + strings.TrimPrefix(srv.URL, "http"), MaxReplyBytes: 64 << 10},
		func(ctx context.Context, msg []byte, send SendFunc) {
			var probe struct {
				RequestID string `json:"request_id"`
			}
			_ = json.Unmarshal(msg, &probe)
			size := 1 << 10
			if probe.RequestID[len(probe.RequestID)-1]%2 == 0 { // r0, r2, ... go over
				size = 256 << 10
			}
			_ = send(&Response{Action: "response", RequestID: probe.RequestID, StatusCode: 200, Data: bigData(size), ActionName: "a"})
		})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Run(ctx) }()

	var mu sync.Mutex
	status := map[string]int{}
	for i := 0; i < n; i++ {
		select {
		case r := <-got:
			mu.Lock()
			status[r.RequestID] = r.StatusCode
			mu.Unlock()
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d of %d replies", i, n)
		}
	}
	for id, s := range status {
		want := 200
		if id[len(id)-1]%2 == 0 {
			want = 413
		}
		if s != want {
			t.Errorf("%s status = %d; want %d", id, s, want)
		}
	}
}
