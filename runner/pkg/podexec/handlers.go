package podexec

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Handlers wires Group D pod-exec primitives into the dispatch registry.
//
// Action contracts (LLM tool callers + legacy callers):
//
//	pod_bash_enricher:
//	  params = {namespace, pod, container?, command (string)}
//	  command is the shell line: agent runs ["bash", "-c", command]
//
//	pod_profiler:
//	  params = {name, namespace, seconds, profile_type, lang?,
//	           profile_tool?, output_type?}
//	  spawns a privileged debugger pod, streams its output, copies the
//	  profile result file out as a base64 FileBlock. Wired by callers
//	  that pass a non-nil ProfilerHandler (PodExecEnabled + restCfg
//	  available); otherwise omitted from the registry.
//
// Note: pod_script_run_enricher lives in pkg/podrunner — it's a
// dedicated-pod runner (spawn pod with image, env from k8s secret, run
// script, return logs), not an exec-into-existing-pod primitive. The
// wire shape and callers (api-server relay.CommandExecutor, runbook-server,
// llm-server) all assume that semantic; keeping it out of this package
// makes the split explicit.
func Handlers(e Executor) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"pod_bash_enricher": wrap(e, handleBash),
	}
}

// HandlersWithProfiler is the superset Handlers + the pod_profiler
// handler. Kept distinct so cmd/agent/main.go can decide whether to
// attach the profiler based on whether the rest config is available
// (the file-fetch path needs SPDY).
func HandlersWithProfiler(e Executor, p *ProfilerHandler) map[string]dispatch.Handler {
	hs := Handlers(e)
	if p != nil {
		hs["pod_profiler"] = func(ctx context.Context, params map[string]any) (any, error) {
			req, err := parseProfileRequest(params)
			if err != nil {
				return nil, err
			}
			return p.Profile(ctx, req)
		}
	}
	return hs
}

// parseProfileRequest unmarshals the dispatch params into the typed
// ProfileRequest. We go through json.Marshal/Unmarshal rather than
// hand-coding field-by-field readers so api-server's JSON shape and
// the agent's struct stay aligned without the boilerplate drifting.
func parseProfileRequest(params map[string]any) (ProfileRequest, error) {
	if params == nil {
		return ProfileRequest{}, errors.New("pod_profiler: params required")
	}
	b, err := json.Marshal(params)
	if err != nil {
		return ProfileRequest{}, err
	}
	var req ProfileRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return ProfileRequest{}, err
	}
	if req.Name == "" || req.Namespace == "" {
		return req, errors.New("pod_profiler: name and namespace required")
	}
	return req, nil
}

func wrap(e Executor, fn func(ctx context.Context, e Executor, p map[string]any) (any, error)) dispatch.Handler {
	return func(ctx context.Context, p map[string]any) (any, error) {
		if e == nil {
			return nil, errors.New("podexec: executor not configured")
		}
		return fn(ctx, e, p)
	}
}

func handleBash(ctx context.Context, e Executor, p map[string]any) (any, error) {
	cmd, _ := p["command"].(string)
	if cmd == "" {
		return nil, errors.New("pod_bash_enricher: command is required")
	}
	req := &Request{
		Namespace: str(p, "namespace"),
		Pod:       str(p, "pod"),
		Container: str(p, "container"),
		Command:   []string{"bash", "-c", cmd},
	}
	return e.Exec(ctx, req)
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}
