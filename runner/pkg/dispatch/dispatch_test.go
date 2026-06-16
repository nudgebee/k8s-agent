package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/auth"
	"github.com/nudgebee/nudgebee-agent/pkg/relay"
)

// captureSend records every Response written so tests can inspect them.
type captureSend struct {
	mu    sync.Mutex
	calls []*relay.Response
}

func (c *captureSend) send(r *relay.Response) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, r)
	return nil
}

func (c *captureSend) only(t *testing.T) *relay.Response {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) != 1 {
		t.Fatalf("expected 1 response, got %d: %+v", len(c.calls), c.calls)
	}
	return c.calls[0]
}

func envelope(t *testing.T, action, requestID string, params map[string]any) []byte {
	t.Helper()
	body := map[string]any{"action_name": action, "timestamp": int64(1700000000)}
	if params != nil {
		body["action_params"] = params
	}
	out, err := json.Marshal(map[string]any{"body": body, "request_id": requestID})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDispatch_HappyPath(t *testing.T) {
	d := New(Config{}, nil, map[string]Handler{
		"ping": SimpleHandler(func(params map[string]any) (any, error) {
			return map[string]any{"pong": true}, nil
		}),
	})
	cap := &captureSend{}
	d.Handle(context.Background(), envelope(t, "ping", "req-1", nil), cap.send)

	r := cap.only(t)
	if r.StatusCode != 200 || r.RequestID != "req-1" || r.Action != "response" {
		t.Errorf("response = %+v", r)
	}
}

func TestDispatch_UnknownAction(t *testing.T) {
	d := New(Config{}, nil, map[string]Handler{})
	cap := &captureSend{}
	d.Handle(context.Background(), envelope(t, "no_such_action", "req-2", nil), cap.send)

	r := cap.only(t)
	if r.StatusCode != 404 {
		t.Errorf("expected 404, got %d", r.StatusCode)
	}
}

func TestDispatch_HandlerError(t *testing.T) {
	d := New(Config{}, nil, map[string]Handler{
		"boom": SimpleHandler(func(params map[string]any) (any, error) {
			return nil, errors.New("kaboom")
		}),
	})
	cap := &captureSend{}
	d.Handle(context.Background(), envelope(t, "boom", "req-3", nil), cap.send)

	r := cap.only(t)
	if r.StatusCode != 500 {
		t.Errorf("expected 500, got %d", r.StatusCode)
	}
}

func TestDispatch_TaskTimeout(t *testing.T) {
	d := New(Config{TaskTimeout: 50 * time.Millisecond}, nil, map[string]Handler{
		"slow": Handler(func(ctx context.Context, params map[string]any) (any, error) {
			select {
			case <-time.After(500 * time.Millisecond):
				return "done", nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}),
	})
	cap := &captureSend{}
	d.Handle(context.Background(), envelope(t, "slow", "req-4", nil), cap.send)

	r := cap.only(t)
	if r.StatusCode != 504 {
		t.Errorf("expected 504, got %d", r.StatusCode)
	}
}

func TestDispatch_NoRequestIDIsFireAndForget(t *testing.T) {
	d := New(Config{}, nil, map[string]Handler{
		"ping": SimpleHandler(func(params map[string]any) (any, error) {
			return "ok", nil
		}),
	})
	cap := &captureSend{}
	d.Handle(context.Background(), envelope(t, "ping", "" /* no request_id */, nil), cap.send)

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.calls) != 0 {
		t.Errorf("fire-and-forget should not respond; got %d responses", len(cap.calls))
	}
}

func TestDispatch_HighPriorityPoolSeparate(t *testing.T) {
	// Saturate the normal pool; verify that a high_priority action still runs.
	block := make(chan struct{})
	d := New(Config{NormalPoolSize: 1, HighPriorityPoolSize: 1}, nil, map[string]Handler{
		"slow": Handler(func(ctx context.Context, _ map[string]any) (any, error) {
			<-block
			return "done", nil
		}),
		"fast": SimpleHandler(func(_ map[string]any) (any, error) {
			return "ok", nil
		}),
	})

	// Saturate the normal pool with a slow request.
	go d.Handle(context.Background(), envelope(t, "slow", "req-slow", nil), func(*relay.Response) error { return nil })
	time.Sleep(20 * time.Millisecond)

	// Issue a high-priority fast request — should not be blocked.
	cap := &captureSend{}
	done := make(chan struct{})
	go func() {
		d.Handle(context.Background(), envelope(t, "fast", "req-fast", map[string]any{"high_priority": true}), cap.send)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("high-priority request was blocked behind saturated normal pool")
	}
	close(block) // let slow finish so it doesn't leak

	r := cap.only(t)
	if r.StatusCode != 200 {
		t.Errorf("expected 200, got %d", r.StatusCode)
	}
}

func TestDispatch_AuthRejected(t *testing.T) {
	v := &auth.Validator{LightActions: map[string]struct{}{"allowed": {}}}
	d := New(Config{}, v, map[string]Handler{
		"allowed":     SimpleHandler(func(params map[string]any) (any, error) { return "ok", nil }),
		"not_allowed": SimpleHandler(func(params map[string]any) (any, error) { return "ok", nil }),
	})
	cap := &captureSend{}
	d.Handle(context.Background(), envelope(t, "not_allowed", "req-5", nil), cap.send)

	r := cap.only(t)
	if r.StatusCode != 401 {
		t.Errorf("expected 401, got %d (%v)", r.StatusCode, r.Data)
	}
}

// Make sure fmt is used so the import isn't unused (helpful for debugging).
var _ = fmt.Sprintf

// stubTerminal records the requests it receives and returns a fixed response.
type stubTerminal struct {
	mu     sync.Mutex
	got    []*TerminalRequest
	status int
	resp   any
}

func (s *stubTerminal) Handle(_ context.Context, r *TerminalRequest) (any, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, r)
	if s.status == 0 {
		s.status = 200
	}
	if s.resp == nil {
		s.resp = map[string]any{"session_id": "sess-1", "data": "hello", "exit": false}
	}
	return s.resp, s.status
}

// stubGrafana records Grafana / APIProxy / Prometheus invocations.
type stubGrafana struct {
	mu     sync.Mutex
	gotG   []*GrafanaRequest
	gotAPI []struct {
		base string
		req  *GrafanaRequest
	}
	gotProm []*GrafanaRequest
	resp    any
}

func (s *stubGrafana) HandleGrafana(_ context.Context, r *GrafanaRequest) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gotG = append(s.gotG, r)
	if s.resp == nil {
		s.resp = map[string]any{"status_code": 200, "body": "ZHVtbXk=", "header": map[string][]string{"Content-Type": {"text/plain"}}}
	}
	return s.resp
}

func (s *stubGrafana) HandleAPI(_ context.Context, base string, r *GrafanaRequest) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gotAPI = append(s.gotAPI, struct {
		base string
		req  *GrafanaRequest
	}{base, r})
	if s.resp == nil {
		s.resp = map[string]any{"status_code": 200, "body": "YXBp", "header": map[string][]string{}}
	}
	return s.resp
}

func (s *stubGrafana) HandlePrometheus(_ context.Context, r *GrafanaRequest) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gotProm = append(s.gotProm, r)
	if s.resp == nil {
		s.resp = map[string]any{"status_code": 200, "body": "cHJvbQ==", "header": map[string][]string{}}
	}
	return s.resp
}

// TestDispatch_TerminalShape covers the bug that took pod_shell down: the
// agent was silently dropping TerminalRequests, then the relay 500'd on
// `cannot unmarshal object into Go struct field AgentResponse.data of type
// string` because the agent's later 404-style replies had data as an object.
//
// After the fix, TerminalRequests must:
//   - hit the registered TerminalHandler
//   - reply with output_type=Terminal and data as a JSON-stringified payload
func TestDispatch_TerminalShape(t *testing.T) {
	stub := &stubTerminal{}
	d := New(Config{}, nil, map[string]Handler{})
	d.SetTerminal(stub)

	msg, _ := json.Marshal(map[string]any{
		"action":     "start",
		"name":       "rabbitmq",
		"namespace":  "nudgebee",
		"request_id": "req-term-1",
	})
	cap := &captureSend{}
	d.Handle(context.Background(), msg, cap.send)

	if len(stub.got) != 1 || stub.got[0].Action != "start" || stub.got[0].Name != "rabbitmq" {
		t.Fatalf("terminal handler not called or wrong payload: %+v", stub.got)
	}
	r := cap.only(t)
	if r.OutputType != "Terminal" {
		t.Errorf("output_type = %q; want Terminal", r.OutputType)
	}
	if r.RequestID != "req-term-1" {
		t.Errorf("request_id = %q; want req-term-1", r.RequestID)
	}
	// Data MUST be a JSON-stringified payload, not a raw object — the relay
	// unmarshals AgentResponse.Data as `string`.
	dataStr, ok := r.Data.(string)
	if !ok {
		t.Fatalf("Response.Data type = %T; want string", r.Data)
	}
	var inner map[string]any
	if err := json.Unmarshal([]byte(dataStr), &inner); err != nil {
		t.Fatalf("Data is not valid JSON: %v\n%s", err, dataStr)
	}
	if inner["session_id"] != "sess-1" {
		t.Errorf("inner session_id = %v; want sess-1", inner["session_id"])
	}
}

// TestDispatch_TerminalNoHandler covers the case where pod_shell is unwired
// (e.g. agent has no K8s client). We must still reply with the stringified
// shape and OutputType=Terminal — otherwise the relay 500s on the same
// unmarshal that broke shells in the first place.
func TestDispatch_TerminalNoHandler(t *testing.T) {
	d := New(Config{}, nil, map[string]Handler{})
	msg, _ := json.Marshal(map[string]any{
		"action":     "start",
		"request_id": "req-term-2",
	})
	cap := &captureSend{}
	d.Handle(context.Background(), msg, cap.send)

	r := cap.only(t)
	if r.StatusCode != 501 {
		t.Errorf("status = %d; want 501", r.StatusCode)
	}
	if r.OutputType != "Terminal" {
		t.Errorf("output_type = %q; want Terminal", r.OutputType)
	}
	if _, ok := r.Data.(string); !ok {
		t.Errorf("Response.Data type = %T; want string", r.Data)
	}
}

// TestDispatch_GrafanaShape — same wire-shape contract as Terminal, but
// dispatched on `method` + `url` and routed via X-NB-Request-Type.
func TestDispatch_GrafanaShape(t *testing.T) {
	stub := &stubGrafana{}
	d := New(Config{}, nil, map[string]Handler{})
	d.SetGrafana(stub)

	t.Run("default Grafana path", func(t *testing.T) {
		msg, _ := json.Marshal(map[string]any{
			"method": "GET",
			"url":    "/api/datasources",
			"header": map[string][]string{
				"X-Nb-Request-Id": {"grafana-req-1"},
			},
		})
		cap := &captureSend{}
		d.Handle(context.Background(), msg, cap.send)

		if len(stub.gotG) != 1 || stub.gotG[0].URL != "/api/datasources" {
			t.Fatalf("HandleGrafana not called or wrong: %+v", stub.gotG)
		}
		r := cap.only(t)
		if r.OutputType != "Grafana" {
			t.Errorf("output_type = %q; want Grafana", r.OutputType)
		}
		if r.RequestID != "grafana-req-1" {
			t.Errorf("request_id = %q; want grafana-req-1 (extracted from X-Nb-Request-Id header)", r.RequestID)
		}
		if _, ok := r.Data.(string); !ok {
			t.Fatalf("Response.Data type = %T; want string", r.Data)
		}
	})

	t.Run("APIProxy path forwards X-API-Base-URL", func(t *testing.T) {
		stub.gotG = nil
		stub.gotAPI = nil
		msg, _ := json.Marshal(map[string]any{
			"method": "POST",
			"url":    "/v1/data",
			"header": map[string][]string{
				"X-Nb-Request-Id":   {"api-req-1"},
				"X-NB-Request-Type": {"APIProxy"},
				"X-API-Base-URL":    {"https://api.example.com"},
			},
		})
		cap := &captureSend{}
		d.Handle(context.Background(), msg, cap.send)

		if len(stub.gotAPI) != 1 || stub.gotAPI[0].base != "https://api.example.com" {
			t.Fatalf("HandleAPI not called with correct base URL: %+v", stub.gotAPI)
		}
		r := cap.only(t)
		if r.OutputType != "Grafana" {
			t.Errorf("output_type = %q; want Grafana", r.OutputType)
		}
	})

	t.Run("Prometheus path routes to HandlePrometheus", func(t *testing.T) {
		// Production hot-path: collector's `/prometheus-v2/*` route
		// forwards every Prometheus HTTP call this way. Before this
		// dispatch routing, the agent 501'd the request and api-server's
		// FetchMetricsLabels failed with no upstream call ever made.
		stub.gotG = nil
		stub.gotAPI = nil
		stub.gotProm = nil
		msg, _ := json.Marshal(map[string]any{
			"method": "GET",
			"url":    "/api/v1/labels?&match[]=%7B__name__%3D%22x%22%7D",
			"header": map[string][]string{
				"X-Nb-Request-Id":   {"prom-req-1"},
				"X-NB-Request-Type": {"Prometheus"},
			},
		})
		cap := &captureSend{}
		d.Handle(context.Background(), msg, cap.send)

		if len(stub.gotProm) != 1 {
			t.Fatalf("HandlePrometheus not called: %+v", stub.gotProm)
		}
		if stub.gotProm[0].URL != "/api/v1/labels?&match[]=%7B__name__%3D%22x%22%7D" {
			t.Errorf("URL preserved verbatim through dispatch; got %q", stub.gotProm[0].URL)
		}
		if len(stub.gotG) != 0 || len(stub.gotAPI) != 0 {
			t.Errorf("Prometheus request leaked to Grafana/API path: gotG=%d gotAPI=%d", len(stub.gotG), len(stub.gotAPI))
		}
		r := cap.only(t)
		if r.OutputType != "Grafana" {
			t.Errorf("output_type = %q; want Grafana (same envelope as G/APIProxy)", r.OutputType)
		}
		if r.StatusCode != 200 {
			t.Errorf("status = %d; want 200 (stub default)", r.StatusCode)
		}
	})
}

// TestDispatch_RegularActionUnchanged — sanity check that the new shape
// detection doesn't disturb the regular action path.
func TestDispatch_RegularActionUnchanged(t *testing.T) {
	d := New(Config{}, nil, map[string]Handler{
		"ping": SimpleHandler(func(_ map[string]any) (any, error) {
			return map[string]any{"pong": true}, nil
		}),
	})
	cap := &captureSend{}
	d.Handle(context.Background(), envelope(t, "ping", "req-reg-1", nil), cap.send)

	r := cap.only(t)
	if r.StatusCode != 200 || r.OutputType != "actions" {
		t.Errorf("regular action shape changed: status=%d output_type=%q", r.StatusCode, r.OutputType)
	}
	// Regular actions still return data as the raw object (not stringified) —
	// the relay's regular-action path forwards raw bytes verbatim.
	if _, ok := r.Data.(map[string]any); !ok {
		t.Errorf("regular action Data type = %T; want map[string]any", r.Data)
	}
}

// silence unused-imports warning for auth (kept for future tests)
var _ = auth.Validator{}

// TestHandleTrusted_LongActionTimeout verifies the per-action timeout: a long
// action runs under LongTaskTimeout while everything else uses the (short)
// TaskTimeout and times out.
func TestHandleTrusted_LongActionTimeout(t *testing.T) {
	d := New(Config{
		TaskTimeout:     20 * time.Millisecond,
		LongTaskTimeout: 2 * time.Second,
		LongActions:     map[string]struct{}{"migrate": {}},
	}, nil, map[string]Handler{
		"migrate": Handler(func(ctx context.Context, _ map[string]any) (any, error) {
			select {
			case <-time.After(80 * time.Millisecond):
				return "ok", nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}),
		"quick": Handler(func(ctx context.Context, _ map[string]any) (any, error) {
			select {
			case <-time.After(80 * time.Millisecond):
				return "ok", nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}),
	})

	// long action: survives past the short TaskTimeout.
	data, ok, err := d.HandleTrusted(context.Background(), "migrate", nil, false)
	if !ok || err != nil || data != "ok" {
		t.Errorf("long action: data=%v ok=%v err=%v; want ok", data, ok, err)
	}
	// non-long action: reaped at TaskTimeout.
	_, ok, err = d.HandleTrusted(context.Background(), "quick", nil, false)
	if !ok || err == nil {
		t.Errorf("short action should hit deadline; ok=%v err=%v", ok, err)
	}
}
