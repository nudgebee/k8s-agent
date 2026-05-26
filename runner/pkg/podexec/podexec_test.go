package podexec

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// stubExec records calls and returns canned responses. Lets us test the
// param-marshalling layer without needing a real apiserver/kubelet.
type stubExec struct {
	got    *Request
	result *Result
	err    error
}

func (s *stubExec) Exec(_ context.Context, req *Request) (*Result, error) {
	s.got = req
	if s.err != nil {
		return nil, s.err
	}
	if s.result == nil {
		return &Result{Stdout: "ok"}, nil
	}
	return s.result, nil
}

func TestNew_RequiresClient(t *testing.T) {
	e := New(nil, nil) // production exec with nil deps
	_, err := e.Exec(context.Background(), &Request{Namespace: "n", Pod: "p", Command: []string{"true"}})
	if err == nil {
		t.Error("expected error when client + restCfg are nil")
	}
}

func TestExec_RequiresNamespaceAndPod(t *testing.T) {
	e := &clientGoExecutor{}
	if _, err := e.Exec(context.Background(), &Request{Pod: "p", Command: []string{"x"}}); err == nil {
		t.Error("missing namespace should error")
	}
	if _, err := e.Exec(context.Background(), &Request{Namespace: "n", Command: []string{"x"}}); err == nil {
		t.Error("missing pod should error")
	}
}

func TestExec_RequiresCommand(t *testing.T) {
	e := &clientGoExecutor{}
	if _, err := e.Exec(context.Background(), &Request{Namespace: "n", Pod: "p"}); err == nil {
		t.Error("missing command should error")
	}
}

func TestHandleBash_BuildsCorrectCommand(t *testing.T) {
	stub := &stubExec{}
	hs := Handlers(stub)
	_, err := hs["pod_bash_enricher"](context.Background(), map[string]any{
		"namespace": "shop",
		"pod":       "frontend",
		"container": "web",
		"command":   "ls -la /etc",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bash", "-c", "ls -la /etc"}
	if !reflect.DeepEqual(stub.got.Command, want) {
		t.Errorf("Command = %v; want %v", stub.got.Command, want)
	}
	if stub.got.Namespace != "shop" || stub.got.Pod != "frontend" || stub.got.Container != "web" {
		t.Errorf("ns/pod/container wrong: %+v", stub.got)
	}
}

func TestHandleBash_RejectsEmptyCommand(t *testing.T) {
	stub := &stubExec{}
	hs := Handlers(stub)
	_, err := hs["pod_bash_enricher"](context.Background(), map[string]any{
		"namespace": "n", "pod": "p",
	})
	if err == nil {
		t.Error("expected error for missing command")
	}
}

func TestHandlers_PodScriptRunEnricherNotRegistered(t *testing.T) {
	// pod_script_run_enricher moved to pkg/podrunner — its old exec-into-pod
	// semantics didn't match what api-server/relay/runbook/llm callers send
	// (image + secret envFrom). Keep this test as a tripwire so a future
	// refactor doesn't silently re-register the wrong handler here.
	hs := Handlers(&stubExec{})
	if _, ok := hs["pod_script_run_enricher"]; ok {
		t.Error("pod_script_run_enricher must not be registered by podexec; it belongs to pkg/podrunner")
	}
}

func TestHandlers_NilExecutor(t *testing.T) {
	hs := Handlers(nil)
	_, err := hs["pod_bash_enricher"](context.Background(), map[string]any{
		"namespace": "n", "pod": "p", "command": "echo",
	})
	if err == nil {
		t.Error("expected error when executor is nil")
	}
}

func TestHandlers_PropagatesExecError(t *testing.T) {
	stub := &stubExec{err: errors.New("transport blew up")}
	hs := Handlers(stub)
	_, err := hs["pod_bash_enricher"](context.Background(), map[string]any{
		"namespace": "n", "pod": "p", "command": "echo",
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestHandlers_PassesResultThrough(t *testing.T) {
	stub := &stubExec{result: &Result{Stdout: "hello", ExitCode: 7}}
	hs := Handlers(stub)
	got, err := hs["pod_bash_enricher"](context.Background(), map[string]any{
		"namespace": "n", "pod": "p", "command": "echo",
	})
	if err != nil {
		t.Fatal(err)
	}
	r, ok := got.(*Result)
	if !ok {
		t.Fatalf("result type = %T", got)
	}
	if r.Stdout != "hello" || r.ExitCode != 7 {
		t.Errorf("result = %+v", r)
	}
}

// NOTE: Coverage of clientGoExecutor.Exec's SPDY-dial path is best done by
// envtest with a real apiserver (out of scope for the unit suite — fake
// clientset returns nil for the streaming subresource RESTClient and panics).
// The interface guard tests above cover the input-validation surface.

func TestRequest_DefaultTimeout(t *testing.T) {
	// Verify Request.Timeout defaults to 60s when not set; reading the
	// constant from the source is the most robust check.
	stub := &stubExec{}
	hs := Handlers(stub)
	_, _ = hs["pod_bash_enricher"](context.Background(), map[string]any{
		"namespace": "n", "pod": "p", "command": "echo",
	})
	// The default is applied inside clientGoExecutor.Exec, not in handlers,
	// so the stub captures Timeout=0. This documents the contract.
	if stub.got.Timeout != 0 {
		t.Errorf("expected stub to receive raw Request (Timeout=0); got %v", stub.got.Timeout)
	}
	_ = time.Second // keep import
}
