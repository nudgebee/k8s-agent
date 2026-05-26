package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/google/shlex"
)

// allowedKubectlVerbs is the closed list of subcommands the agent will run
// when handling a `kubectl_command_executor` action. Limits blast radius:
// even if a misauthenticated request slips through, it can only read.
//
// Mutating verbs (apply, delete, patch, edit, drain, cordon, scale, replace,
// rollout) belong in pkg/mutate with explicit named actions and RSA partial-
// keys auth — NOT here.
var allowedKubectlVerbs = map[string]struct{}{
	"get":           {},
	"describe":      {},
	"logs":          {},
	"top":           {},
	"explain":       {},
	"api-resources": {},
	"api-versions":  {},
	"version":       {},
	"cluster-info":  {},
	"config":        {}, // only read subcommands, but kubectl config can also write — caller must scope further
	"auth":          {}, // can-i, whoami; both read-only
}

// KubectlExecutor runs kubectl as an external process. It expects the kubectl
// binary to be on PATH inside the agent image (the Dockerfile installs it).
type KubectlExecutor struct {
	BinaryPath string // default "kubectl"
}

// Run parses the command string, validates the verb against the allowlist,
// and executes kubectl. Stdout, stderr, and exit code are returned together
// — callers consume all three.
func (k *KubectlExecutor) Run(ctx context.Context, command string) (map[string]any, error) {
	if command == "" {
		return nil, errors.New("kubectl: command is required")
	}
	args, err := shlex.Split(command)
	if err != nil {
		return nil, fmt.Errorf("kubectl: parse command: %w", err)
	}

	// Strip a leading "kubectl" if the caller included it.
	if len(args) > 0 && args[0] == "kubectl" {
		args = args[1:]
	}
	if len(args) == 0 {
		return nil, errors.New("kubectl: empty command after stripping prefix")
	}

	verb := args[0]
	if _, ok := allowedKubectlVerbs[verb]; !ok {
		return nil, fmt.Errorf("kubectl: verb %q not in read-only allowlist; mutating actions go through pkg/mutate", verb)
	}

	bin := k.BinaryPath
	if bin == "" {
		bin = "kubectl"
	}

	var stdout, stderr bytes.Buffer
	// args are validated upstream against a read-verb allowlist
	// (pkg/kube/dynamic.go) before reaching this shell-out.
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // verb-allowlist enforced
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	// ProcessState is nil if the process never started (e.g. kubectl not
	// on PATH); the real-exec-failure branch below turns that into an error.
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	out := map[string]any{
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
		"exit_code": exitCode,
	}
	// kubectl returns non-zero for cases the caller may want to inspect
	// (e.g. "not found"); surface as data, not Go error.
	if runErr != nil {
		// Real exec failure (e.g. binary not found) is a hard error.
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return out, nil
		}
		return out, fmt.Errorf("kubectl exec: %w", runErr)
	}
	return out, nil
}

// command is exposed for tests; gofmt forbids unused funcs.
var _ = strings.HasPrefix
