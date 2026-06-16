package tasks

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDispatch records what gets dispatched and lets tests inject the
// response. ok=false simulates an unregistered action_name.
type fakeDispatch struct {
	calls atomic.Int32
	last  struct {
		actionName string
		params     map[string]any
	}
	resp map[string]any
	ok   bool
	err  error
}

func (f *fakeDispatch) HandleTrusted(_ context.Context, actionName string, params map[string]any, _ bool) (any, bool, error) {
	f.calls.Add(1)
	f.last.actionName = actionName
	f.last.params = params
	return f.resp, f.ok, f.err
}

// TestService_DrainsAllAndRespondsPerTask covers the happy path: GET returns
// 2 tasks, agent dispatches each, posts the result back, then GETs again
// (because remaining_task_count was non-zero) and exits when the queue is
// empty.
func TestService_DrainsAllAndRespondsPerTask(t *testing.T) {
	var pulls atomic.Int32
	posts := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/k8s/tasks":
			pulls.Add(1)
			// First call returns 2 tasks + remaining=1 to force a re-pull.
			// Second call returns empty.
			n := pulls.Load()
			if n == 1 {
				_, _ = w.Write([]byte(`{"data":[
{"task_id":"t1","payload":{"action_name":"krr_scan","action_params":{}},"source":"recommendation"},
{"task_id":"t2","payload":"{\"action_name\":\"image_scanner\",\"action_params\":{\"image_name\":\"nginx\"}}","source":"recommendation"}
],"remaining_task_count":1}`))
			} else {
				_, _ = w.Write([]byte(`{"data":[],"remaining_task_count":0}`))
			}
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/k8s/tasks/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/k8s/tasks/")
			body, _ := io.ReadAll(r.Body)
			var v map[string]any
			_ = json.Unmarshal(body, &v)
			posts[id] = v
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	d := &fakeDispatch{ok: true, resp: map[string]any{"success": true, "data": "result"}}
	s := &Service{
		Endpoint:   srv.URL,
		AuthSecret: "tok",
		Period:     time.Hour,
		Logger:     slog.Default(),
		Dispatch:   d,
	}
	if err := s.drain(context.Background()); err != nil {
		t.Fatal(err)
	}

	if d.calls.Load() != 2 {
		t.Errorf("dispatch called %d times; want 2", d.calls.Load())
	}
	if len(posts) != 2 {
		t.Fatalf("posts back = %d; want 2 (got %+v)", len(posts), posts)
	}
	if posts["t1"]["success"] != true {
		t.Errorf("t1 response = %+v", posts["t1"])
	}
	// The second task's payload was a JSON string; make sure unpacking worked
	// and the inner action_params reached the dispatcher.
	if d.last.actionName != "image_scanner" || d.last.params["image_name"] != "nginx" {
		t.Errorf("string-payload unpack wrong: action=%q params=%+v", d.last.actionName, d.last.params)
	}
}

// TestService_UnregisteredActionPostsFailure asserts that a task naming an
// action the agent doesn't have still gets a POST back so the api-server
// task row moves out of TODO state.
func TestService_UnregisteredActionPostsFailure(t *testing.T) {
	var seen map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"task_id":"x","payload":{"action_name":"nope"}}],"remaining_task_count":0}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &seen)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	s := &Service{
		Endpoint: srv.URL,
		Logger:   slog.Default(),
		Dispatch: &fakeDispatch{ok: false},
	}
	if err := s.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seen["success"] != false {
		t.Errorf("expected success=false; got %+v", seen)
	}
	if !strings.Contains(seen["msg"].(string), "not registered") {
		t.Errorf("msg = %q", seen["msg"])
	}
}

// TestService_AuthHeaderShape locks the bare-base64 auth shape to match the
// discovery sink (collector decodes the whole header verbatim).
func TestService_AuthHeaderShape(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[],"remaining_task_count":0}`))
	}))
	defer srv.Close()
	s := &Service{Endpoint: srv.URL, AuthSecret: "secret", Logger: slog.Default(), Dispatch: &fakeDispatch{ok: true}}
	if err := s.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth == "" || strings.HasPrefix(auth, "Basic ") {
		t.Errorf("Authorization = %q (want bare base64, no Basic prefix)", auth)
	}
}

// TestParseTaskWindow covers env-default behavior so the chart's existing
// TASK_RUNNER_WINDOW value works unchanged.
func TestParseTaskWindow(t *testing.T) {
	cases := map[string]time.Duration{
		"":     120 * time.Second,
		"30":   30 * time.Second,
		"abc":  120 * time.Second,
		"-1":   120 * time.Second,
		"3600": 3600 * time.Second,
	}
	for in, want := range cases {
		if got := ParseTaskWindow(in); got != want {
			t.Errorf("ParseTaskWindow(%q) = %v; want %v", in, got, want)
		}
	}
}

// gateDispatch blocks in HandleTrusted until released, so the test can observe
// that a long action does not block the drain loop.
type gateDispatch struct {
	started  chan struct{}
	release  chan struct{}
	finished atomic.Bool
}

func (g *gateDispatch) HandleTrusted(_ context.Context, _ string, _ map[string]any, _ bool) (any, bool, error) {
	close(g.started)
	<-g.release
	g.finished.Store(true)
	return map[string]any{"success": true}, true, nil
}

// TestService_LongActionRunsAsync asserts a LongActions task is dispatched on a
// detached goroutine: drain() returns before the handler finishes, and the
// result is still POSTed once the handler completes.
func TestService_LongActionRunsAsync(t *testing.T) {
	posted := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[{"task_id":"m1","payload":{"action_name":"rightsize_pvc","action_params":{}},"source":"recommendation"}],"remaining_task_count":0}`))
		case r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var v map[string]any
			_ = json.Unmarshal(body, &v)
			posted <- v
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()

	g := &gateDispatch{started: make(chan struct{}), release: make(chan struct{})}
	s := &Service{
		Endpoint:    srv.URL,
		Period:      time.Hour,
		Logger:      slog.Default(),
		Dispatch:    g,
		LongActions: map[string]struct{}{"rightsize_pvc": {}},
	}

	if err := s.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-g.started // handler is in flight
	if g.finished.Load() {
		t.Fatal("drain should have returned before the long handler finished")
	}
	select {
	case <-posted:
		t.Fatal("result posted before handler released")
	case <-time.After(20 * time.Millisecond):
	}

	close(g.release) // let the handler complete
	select {
	case v := <-posted:
		if v["success"] != true {
			t.Errorf("posted result = %+v", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long action result never posted")
	}
}
