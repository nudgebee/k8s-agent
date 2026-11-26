package dispatch

import (
	"context"
	"testing"
)

// Every reply carries the action it answers, success and failure alike, so the
// relay client's size limit can name it.
func TestDispatch_RepliesCarryActionName(t *testing.T) {
	d := New(Config{}, nil, map[string]Handler{
		"ping": SimpleHandler(func(params map[string]any) (any, error) { return map[string]any{"pong": true}, nil }),
	})
	for _, action := range []string{"ping", "no_such_action"} {
		cap := &captureSend{}
		d.Handle(context.Background(), envelope(t, action, "req-"+action, nil), cap.send)
		if r := cap.only(t); r.ActionName != action {
			t.Errorf("%s reply ActionName = %q", action, r.ActionName)
		}
	}
}

// Proxy and shell replies are named by their path.
func TestDispatch_ProxyRepliesCarryPathName(t *testing.T) {
	d := New(Config{}, nil, map[string]Handler{})
	cases := map[string]string{
		`{"method":"GET","url":"/api/v1/query","header":{"X-Nb-Request-Id":["g1"]}}`: "grafana_proxy",
		`{"request_id":"t1","action":"start","name":"web","namespace":"shop"}`:       "pod_shell",
	}
	for msg, want := range cases {
		cap := &captureSend{}
		d.Handle(context.Background(), []byte(msg), cap.send)
		if r := cap.only(t); r.ActionName != want {
			t.Errorf("%s reply ActionName = %q; want %q", msg, r.ActionName, want)
		}
	}
}
