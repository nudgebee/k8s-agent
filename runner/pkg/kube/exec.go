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

// rejectedShellTokens are shell metacharacters we refuse rather than silently
// mis-execute. kubectl is exec'd directly (no shell), so a pipe or redirect
// would not do what the author expects — and worse, a pipe hands output to a
// non-kubectl binary, escaping the per-command verb allowlist. Sequential
// chaining (&&, ||, ;) IS supported (see runSegments); these are not.
var rejectedShellTokens = map[string]struct{}{
	"|": {}, "|&": {},
	">": {}, ">>": {}, "<": {}, "<<": {}, "<<<": {},
	"&": {}, "&>": {}, "&>>": {},
}

// segmentSeparators are the sequential list operators that may chain kubectl
// commands. They map to shell short-circuit semantics in shouldRunSegment.
var segmentSeparators = map[string]struct{}{
	"&&": {}, "||": {}, ";": {},
}

// cmdSegment is one kubectl invocation within a chained command, together with
// the operator that precedes it ("" for the first segment).
type cmdSegment struct {
	op   string   // "", "&&", "||", ";"
	args []string // verb + args, leading "kubectl" already stripped
}

// Run parses the command string, validates the verb of every chained segment
// against the allowlist, and executes kubectl. A command may chain multiple
// invocations with &&, || and ; (matching shell short-circuit semantics);
// pipes and redirects are rejected. Stdout, stderr, and exit code are returned
// together — callers consume all three.
func (k *KubectlExecutor) Run(ctx context.Context, command string) (map[string]any, error) {
	if command == "" {
		return nil, errors.New("kubectl: command is required")
	}
	tokens, err := shlex.Split(command)
	if err != nil {
		return nil, fmt.Errorf("kubectl: parse command: %w", err)
	}

	// Reject pipes/redirects up front. shlex yields these as standalone tokens,
	// so an operator quoted inside an argument (e.g. a label value) is untouched.
	for _, t := range tokens {
		if _, bad := rejectedShellTokens[t]; bad {
			return nil, fmt.Errorf("kubectl: shell operator %q is not supported; chain commands with && , || or ; (no pipes or redirects)", t)
		}
	}

	segments, err := splitSegments(tokens)
	if err != nil {
		return nil, err
	}

	// Validate every segment BEFORE running any, so a chain such as
	// `get pods && delete pod foo` is rejected atomically — the read-only verb
	// allowlist must hold for each command, not merely the first one.
	for _, seg := range segments {
		if err := k.validateSegment(seg.args); err != nil {
			return nil, err
		}
	}

	bin := k.BinaryPath
	if bin == "" {
		bin = "kubectl"
	}
	return k.runSegments(ctx, bin, segments)
}

// splitSegments partitions shlex tokens into command segments on the sequential
// operators && || ; . A leading "kubectl" is stripped from each segment. Empty
// segments (a leading/trailing/doubled operator) are an error.
func splitSegments(tokens []string) ([]cmdSegment, error) {
	var segments []cmdSegment
	op := "" // operator preceding the segment currently being accumulated
	var cur []string

	flush := func(nextOp string) error {
		args := cur
		if len(args) > 0 && args[0] == "kubectl" {
			args = args[1:]
		}
		if len(args) == 0 {
			return errors.New("kubectl: empty command segment; && , || and ; each need a command on both sides")
		}
		segments = append(segments, cmdSegment{op: op, args: args})
		op = nextOp
		cur = nil
		return nil
	}

	for _, t := range tokens {
		if _, isSep := segmentSeparators[t]; isSep {
			if err := flush(t); err != nil {
				return nil, err
			}
			continue
		}
		cur = append(cur, t)
	}
	if err := flush(""); err != nil {
		return nil, err
	}
	return segments, nil
}

// validateSegment resolves the verb past leading global flags and enforces the
// read-only allowlist when write mode is off.
func (k *KubectlExecutor) validateSegment(args []string) error {
	verb := firstVerb(args)
	if verb == "" {
		return errors.New("kubectl: no verb found (only flags supplied)")
	}
	if !k.AllowWrite {
		if _, ok := allowedKubectlVerbs[verb]; !ok {
			return fmt.Errorf("kubectl: verb %q not in read-only allowlist; enable runner.enableWritePermissions for writes, or route mutating actions through pkg/mutate", verb)
		}
	}
	return nil
}

// shouldRunSegment applies shell short-circuit semantics: a segment after && runs
// only when the previous command succeeded, after || only when it failed, and
// after ; always. &&/|| are equal-precedence and left-associative, so evaluating
// left-to-right against the carried exit code reproduces bash for mixed chains.
func shouldRunSegment(op string, prevExit int) bool {
	switch op {
	case "&&":
		return prevExit == 0
	case "||":
		return prevExit != 0
	default: // ";" — unconditional
		return true
	}
}

// runSegments executes segments in order, honoring &&/||/; short-circuiting, and
// aggregates their output. exit_code is the status of the last command that
// actually ran (skipped segments carry the prior status forward, as in a shell).
func (k *KubectlExecutor) runSegments(ctx context.Context, bin string, segments []cmdSegment) (map[string]any, error) {
	var stdout, stderr bytes.Buffer
	exitCode := 0 // status carried between segments for &&/|| short-circuit

	for i, seg := range segments {
		if i > 0 && !shouldRunSegment(seg.op, exitCode) {
			continue
		}

		// Verb allowlist enforced per segment in validateSegment above.
		cmd := exec.CommandContext(ctx, bin, seg.args...) //nolint:gosec // verb-allowlist enforced per segment
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		runErr := cmd.Run()

		// ProcessState is nil if the process never started (e.g. kubectl not on
		// PATH); the real-exec-failure branch below turns that into an error.
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		} else {
			exitCode = -1
		}

		if runErr != nil {
			// kubectl returns non-zero for cases the caller may want to inspect
			// (e.g. "not found"); surface those as data via exit_code. A real
			// exec failure (binary missing) is a hard error and stops the chain.
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) {
				return map[string]any{
					"stdout":    stdout.String(),
					"stderr":    stderr.String(),
					"exit_code": exitCode,
				}, fmt.Errorf("kubectl exec: %w", runErr)
			}
		}
	}

	return map[string]any{
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
		"exit_code": exitCode,
	}, nil
}
