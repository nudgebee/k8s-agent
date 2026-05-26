// Package podexec implements the pod-exec primitives the agent exposes
// (Group D in the deprecation plan). The agent runs `kubectl exec`-style
// commands inside customer-cluster pods on behalf of LLM tools and runbooks.
//
// Action surface:
//   - pod_bash_enricher        : run a bash one-liner inside a container
//   - pod_script_run_enricher  : pipe a script body into bash via stdin
//
// Implementation: client-go's remotecommand package over SPDY (the same
// channel kubectl exec uses). No kubectl binary required in the image.
//
// SECURITY: these handlers execute arbitrary shell in customer pods. They
// MUST be served behind RSA partial-keys auth in production, and logged
// per-call (request_id + account + pod + command bytes).
package podexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	clientexec "k8s.io/client-go/util/exec"
)

// Request describes one pod-exec invocation.
type Request struct {
	Namespace string
	Pod       string
	Container string   // optional; defaults to pod's first container
	Command   []string // already-tokenized command (e.g. ["bash","-c","ls"])
	Stdin     string   // optional; piped to the container's stdin
	Timeout   time.Duration
}

// Result captures the exec outcome. ExitCode is 0 on success, non-zero
// when the remote process exited non-zero (still a successful invocation).
// Error is set only when the SPDY transport itself failed.
type Result struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

// Executor abstracts the SPDY exec call so unit tests can substitute a stub.
type Executor interface {
	Exec(ctx context.Context, req *Request) (*Result, error)
}

// New returns a production Executor backed by client-go remotecommand.
func New(cs kubernetes.Interface, restCfg *rest.Config) Executor {
	return &clientGoExecutor{cs: cs, restCfg: restCfg}
}

type clientGoExecutor struct {
	cs      kubernetes.Interface
	restCfg *rest.Config
}

func (e *clientGoExecutor) Exec(ctx context.Context, req *Request) (*Result, error) {
	if e.cs == nil || e.restCfg == nil {
		return nil, errors.New("podexec: client not configured")
	}
	if req.Pod == "" || req.Namespace == "" {
		return nil, errors.New("podexec: namespace and pod are required")
	}
	if len(req.Command) == 0 {
		return nil, errors.New("podexec: command is required")
	}
	if req.Timeout == 0 {
		req.Timeout = 60 * time.Second
	}

	restReq := e.cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(req.Pod).
		Namespace(req.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command:   req.Command,
			Stdin:     req.Stdin != "",
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
			Container: req.Container,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.restCfg, "POST", restReq.URL())
	if err != nil {
		return nil, fmt.Errorf("podexec: build SPDY executor: %w", err)
	}

	streamCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	streamOpts := remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}
	if req.Stdin != "" {
		streamOpts.Stdin = strings.NewReader(req.Stdin)
	}

	streamErr := exec.StreamWithContext(streamCtx, streamOpts)
	res := &Result{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if streamErr != nil {
		// remotecommand returns a CodeExitError when the remote process
		// exited non-zero — that's data, not an error.
		var codeErr clientexec.CodeExitError
		if errors.As(streamErr, &codeErr) {
			res.ExitCode = codeErr.Code
			return res, nil
		}
		res.Error = streamErr.Error()
		return res, fmt.Errorf("podexec: stream: %w", streamErr)
	}
	res.ExitCode = 0
	return res, nil
}
