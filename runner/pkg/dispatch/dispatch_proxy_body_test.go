package dispatch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// X-Nb-Request-Type: Prometheus routes to HandlePrometheus.
// A proxy request carries `body` — the base64-encoded HTTP request body, a JSON
// string — alongside method+url. It must still route to the Grafana handler.
// Before the shape probe required a body *object*, any POST-with-body landed in
// the action parser, which rejected the string body and returned without
// replying, so the relay waited out its full timeout with no error.
func TestDispatch_ProxyRequestWithBodyRoutesToGrafana(t *testing.T) {
	form := "query=up&start=1&end=2&step=60"

	for _, tt := range []struct {
		name string
		body string
		want bool // expect it to reach the Grafana handler
	}{
		{"POST with form body", base64.StdEncoding.EncodeToString([]byte(form)), true},
		{"POST with empty body", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubGrafana{}
			d := New(Config{}, nil, map[string]Handler{})
			d.SetGrafana(stub)

			m := map[string]any{
				"method": "POST",
				"url":    "/api/v1/query_range",
				"header": map[string][]string{
					"X-Nb-Request-Id":   {"proxy-post-1"},
					"X-Nb-Request-Type": {"Prometheus"},
				},
			}
			if tt.body != "" {
				m["body"] = tt.body
				m["content_length"] = len(form)
			}
			msg, _ := json.Marshal(m)

			cap := &captureSend{}
			d.Handle(context.Background(), msg, cap.send)

			if got := len(stub.gotProm) == 1; got != tt.want {
				t.Fatalf("reached Prometheus proxy handler = %v, want %v (calls=%+v)", got, tt.want, stub.gotProm)
			}
			if stub.gotProm[0].URL != "/api/v1/query_range" {
				t.Errorf("url = %q; want /api/v1/query_range", stub.gotProm[0].URL)
			}
			if stub.gotProm[0].Body != tt.body {
				t.Errorf("body not forwarded intact: got %q want %q", stub.gotProm[0].Body, tt.body)
			}
			// The caller must get an answer, not silence.
			if r := cap.only(t); r.RequestID != "proxy-post-1" {
				t.Errorf("request_id = %q; want proxy-post-1", r.RequestID)
			}
		})
	}
}

// A genuine action envelope must keep going to the action path.
func TestDispatch_ActionEnvelopeStillRoutesToAction(t *testing.T) {
	called := false
	d := New(Config{}, nil, map[string]Handler{
		"ping": func(ctx context.Context, p map[string]any) (any, error) { called = true; return "pong", nil },
	})
	msg, _ := json.Marshal(map[string]any{
		"request_id": "act-1",
		"body":       map[string]any{"action_name": "ping", "action_params": map[string]any{}},
	})
	cap := &captureSend{}
	d.Handle(context.Background(), msg, cap.send)
	if !called {
		t.Fatal("action handler was not invoked; the body-object probe must still select the action path")
	}
	_ = cap
}

// A malformed envelope must be answered, not silently dropped — otherwise the
// caller blocks until the relay's timeout with no diagnostic.
func TestDispatch_MalformedEnvelopeRepliesInsteadOfHanging(t *testing.T) {
	d := New(Config{}, nil, map[string]Handler{})
	// `body` is an object (so it takes the action path) but `signature` is the
	// wrong type, which fails the envelope unmarshal.
	msg := []byte(`{"request_id":"bad-1","body":{"action_name":"x"},"signature":{"not":"a string"}}`)

	cap := &captureSend{}
	d.Handle(context.Background(), msg, cap.send)

	r := cap.only(t)
	if r.StatusCode != 400 {
		t.Errorf("status_code = %d; want 400", r.StatusCode)
	}
	if r.RequestID != "bad-1" {
		t.Errorf("request_id = %q; want bad-1", r.RequestID)
	}
}
