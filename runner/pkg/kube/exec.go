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

	// AllowWrite lifts the read-only verb allowlist when true. Gated by the
	// chart's runner.enableWritePermissions (KUBECTL_ALLOW_WRITE) — the same
	// switch that grants the service account its write RBAC, so the runner
	// allowlist and the cluster RBAC stay in lockstep. When false (default),
	// only the read verbs in allowedKubectlVerbs are permitted; mutations must
	// go through pkg/mutate. When true, any verb is allowed and the API
	// server's RBAC becomes the enforcement boundary.
	AllowWrite bool
}

// firstVerb returns the kubectl subcommand from args, skipping any leading
// global flags (`-n ns`, `--namespace=ns`, `--context c`, `-o yaml`, ...).
// kubectl accepts global flags before the verb, so `kubectl -n foo get pods`
// has verb "get", not "-n". Returns "" if no non-flag token is found.
//
// Flags that take a separate-token value (`-n foo`, `--context bar`) would
// otherwise leave the value looking like a verb; verbFlagsWithValue lists the
// global flags whose value is a following token so we can skip it. Flags using
// `=` (`--namespace=foo`) carry their value inline and need no lookahead.
func firstVerb(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a
		}
		// A `--flag=value` / `-o=value` token is self-contained.
		if strings.Contains(a, "=") {
			continue
		}
		// A bare global flag taking a separate value consumes the next token.
		if _, takesValue := verbFlagsWithValue[a]; takesValue {
			i++
		}
	}
	return ""
}

// verbFlagsWithValue are the kubectl global flags that may legitimately precede
// the verb and consume a following token as their value. Boolean global flags
// (e.g. --insecure-skip-tls-verify) are absent because they take no value.
var verbFlagsWithValue = map[string]struct{}{
	"-n": {}, "--namespace": {},
	"--context": {},
	"--cluster": {},
	"--user":    {},
	"-o":        {}, "--output": {},
	"-s": {}, "--server": {},
	"-v": {}, "--v": {}, // log verbosity, e.g. `-v 6`
	"--kubeconfig":            {},
	"--token":                 {},
	"--as":                    {},
	"--as-group":              {},
	"--as-uid":                {},
	"--username":              {},
	"--password":              {},
	"--vmodule":               {},
	"--request-timeout":       {},
	"--cache-dir":             {},
	"--certificate-authority": {},
	"--client-certificate":    {},
	"--client-key":            {},
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

	// Resolve the verb past any leading global flags so `kubectl -n ns get
	// pods` validates as "get", not "-n".
	verb := firstVerb(args)
	if verb == "" {
		return nil, errors.New("kubectl: no verb found (only flags supplied)")
	}
	if !k.AllowWrite {
		if _, ok := allowedKubectlVerbs[verb]; !ok {
			return nil, fmt.Errorf("kubectl: verb %q not in read-only allowlist; enable runner.enableWritePermissions for writes, or route mutating actions through pkg/mutate", verb)
		}
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
