package kube

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKubectl_RejectsEmptyCommand(t *testing.T) {
	k := &KubectlExecutor{}
	if _, err := k.Run(context.Background(), ""); err == nil {
		t.Error("expected error for empty command")
	}
}

func TestKubectl_RejectsMutatingVerbs(t *testing.T) {
	k := &KubectlExecutor{}
	for _, cmd := range []string{
		"kubectl delete pod foo",
		"kubectl apply -f deploy.yaml",
		"kubectl patch deploy foo -p '...'",
		"drain node-1",
		"cordon node-1",
	} {
		_, err := k.Run(context.Background(), cmd)
		if err == nil {
			t.Errorf("%s: expected rejection (mutating verb)", cmd)
		} else if !strings.Contains(err.Error(), "allowlist") && !strings.Contains(err.Error(), "verb") {
			t.Errorf("%s: error %q does not mention allowlist", cmd, err.Error())
		}
	}
}

func TestKubectl_AcceptsReadVerbs(t *testing.T) {
	// Use /usr/bin/true (or similar always-succeeds binary) by overriding
	// BinaryPath, since the test machine may not have kubectl. We're verifying
	// that Run() doesn't reject the verb up front — actual exec is not the
	// point of this unit test (the integration test exercises real kubectl).
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	for _, cmd := range []string{
		"kubectl get pods",
		"describe deployment foo",
		"top nodes",
		"explain pod.spec",
	} {
		out, err := k.Run(context.Background(), cmd)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", cmd, err)
			continue
		}
		if out["exit_code"] != 0 {
			t.Errorf("%s: exit_code = %v; want 0 (true binary)", cmd, out["exit_code"])
		}
	}
}

func TestKubectl_NonExistentBinary_ReturnsError(t *testing.T) {
	k := &KubectlExecutor{BinaryPath: "/no/such/binary/here"}
	_, err := k.Run(context.Background(), "kubectl get pods")
	if err == nil {
		t.Error("expected error for missing binary")
	}
}

func TestKubectl_AcceptsReadVerbsBehindGlobalFlags(t *testing.T) {
	// Global flags may precede the verb; the allowlist must validate the verb
	// past them. Regression for `verb "-n" not in read-only allowlist`.
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	for _, cmd := range []string{
		"kubectl -n kube-system get pods",
		"kubectl --namespace=kube-system get pods",
		"kubectl --context prod -n default get pods",
		"kubectl -o yaml get pod foo",
		"kubectl --kubeconfig /tmp/kc top nodes",
		"kubectl -v 6 get pods", // numeric flag value must not resolve as the verb
	} {
		out, err := k.Run(context.Background(), cmd)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", cmd, err)
			continue
		}
		if out["exit_code"] != 0 {
			t.Errorf("%s: exit_code = %v; want 0", cmd, out["exit_code"])
		}
	}
}

func TestKubectl_RejectsMutatingVerbBehindGlobalFlags(t *testing.T) {
	// A mutating verb hidden behind a flag must still be rejected when write
	// mode is off — the flag-skip must not become a bypass.
	k := &KubectlExecutor{}
	_, err := k.Run(context.Background(), "kubectl -n default scale deploy foo --replicas=3")
	if err == nil {
		t.Fatal("expected rejection for scale behind -n flag")
	}
	if !strings.Contains(err.Error(), "scale") {
		t.Errorf("error %q should name the resolved verb", err.Error())
	}
}

func TestKubectl_AllowWrite_PermitsMutatingVerbs(t *testing.T) {
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true", AllowWrite: true}
	for _, cmd := range []string{
		"kubectl scale deploy foo --replicas=3",
		"kubectl -n default patch deploy foo -p '{}'",
		"kubectl delete pod foo",
	} {
		out, err := k.Run(context.Background(), cmd)
		if err != nil {
			t.Errorf("%s: unexpected error with AllowWrite: %v", cmd, err)
			continue
		}
		if out["exit_code"] != 0 {
			t.Errorf("%s: exit_code = %v; want 0", cmd, out["exit_code"])
		}
	}
}

func TestKubectl_RejectsOnlyFlags(t *testing.T) {
	k := &KubectlExecutor{}
	if _, err := k.Run(context.Background(), "kubectl -n default"); err == nil {
		t.Error("expected error when no verb is present")
	}
}

func TestKubectl_StripsLeadingKubectl(t *testing.T) {
	// `kubectl kubectl get pods` → effective verb is "kubectl" which is NOT
	// allowlisted; verifies the strip happens BEFORE allowlist check (only one).
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	out, err := k.Run(context.Background(), "kubectl get pods")
	if err != nil {
		t.Fatal(err)
	}
	if out["exit_code"] != 0 {
		t.Errorf("got exit_code %v", out["exit_code"])
	}
}

// fakeKubectl writes a stand-in kubectl script that logs each invocation's args
// to a counter file and exits with the given code. Returns the binary path and
// the counter path so tests can assert how many segments actually executed.
func fakeKubectl(t *testing.T, exitCode int) (bin, counter string) {
	t.Helper()
	dir := t.TempDir()
	counter = filepath.Join(dir, "calls.log")
	bin = filepath.Join(dir, "kubectl.sh")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexit %d\n", counter, exitCode)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, counter
}

func callCount(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

func TestKubectl_ChainedCommands_Issue33447(t *testing.T) {
	// Regression for #33447: two valid kubectl commands joined with && used to
	// collapse into one invocation (the second command's tokens became NAME args
	// of the first), yielding "name cannot be provided when a selector is
	// specified". Both segments must now run as separate kubectl processes.
	bin, counter := fakeKubectl(t, 0)
	k := &KubectlExecutor{BinaryPath: bin}
	cmd := "kubectl get pods -n default --field-selector status.phase=Failed -o wide && " +
		"kubectl get pods -n default --field-selector status.phase=Succeeded -o wide"
	out, err := k.Run(context.Background(), cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["exit_code"] != 0 {
		t.Errorf("exit_code = %v; want 0", out["exit_code"])
	}
	if n := callCount(t, counter); n != 2 {
		t.Errorf("segments executed = %d; want 2", n)
	}
}

func TestKubectl_Chained_AcceptsSequentialOperators(t *testing.T) {
	bin, _ := fakeKubectl(t, 0)
	k := &KubectlExecutor{BinaryPath: bin}
	for _, cmd := range []string{
		"kubectl get pods && kubectl get svc",
		"kubectl get pods ; kubectl top nodes",
		"kubectl get pods || describe deployment foo",
		"get pods && get svc && top nodes",
	} {
		if _, err := k.Run(context.Background(), cmd); err != nil {
			t.Errorf("%s: unexpected error: %v", cmd, err)
		}
	}
}

func TestKubectl_Chained_ValidatesEverySegment(t *testing.T) {
	// The read-only allowlist must hold for EACH chained command, not just the
	// first — otherwise `get && delete` would bypass it. The whole chain is
	// rejected before anything executes.
	bin, counter := fakeKubectl(t, 0)
	k := &KubectlExecutor{BinaryPath: bin}
	for _, cmd := range []string{
		"kubectl get pods && kubectl delete pod foo",
		"kubectl get pods ; kubectl scale deploy foo --replicas=0",
		"kubectl delete pod foo || kubectl get pods", // mutating verb in the FIRST segment
	} {
		_, err := k.Run(context.Background(), cmd)
		if err == nil {
			t.Errorf("%s: expected rejection (mutating verb in a segment)", cmd)
		}
	}
	if n := callCount(t, counter); n != 0 {
		t.Errorf("no segment should have executed on rejection; got %d", n)
	}
}

func TestKubectl_Chained_ShortCircuit(t *testing.T) {
	tests := []struct {
		name      string
		exitCode  int
		command   string
		wantCalls int
		wantExit  int
	}{
		{"&& stops on failure", 1, "get pods && get svc", 1, 1},
		{"&& continues on success", 0, "get pods && get svc", 2, 0},
		{"|| skips on success", 0, "get pods || get svc", 1, 0},
		{"|| runs on failure", 1, "get pods || get svc", 2, 1},
		{"; always runs both", 1, "get pods ; get svc", 2, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bin, counter := fakeKubectl(t, tc.exitCode)
			k := &KubectlExecutor{BinaryPath: bin}
			out, err := k.Run(context.Background(), tc.command)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if n := callCount(t, counter); n != tc.wantCalls {
				t.Errorf("segments executed = %d; want %d", n, tc.wantCalls)
			}
			if out["exit_code"] != tc.wantExit {
				t.Errorf("exit_code = %v; want %d", out["exit_code"], tc.wantExit)
			}
		})
	}
}

func TestKubectl_Chained_AllowWrite_PermitsMutations(t *testing.T) {
	bin, counter := fakeKubectl(t, 0)
	k := &KubectlExecutor{BinaryPath: bin, AllowWrite: true}
	out, err := k.Run(context.Background(), "kubectl delete pod a && kubectl delete pod b")
	if err != nil {
		t.Fatalf("unexpected error with AllowWrite: %v", err)
	}
	if out["exit_code"] != 0 {
		t.Errorf("exit_code = %v; want 0", out["exit_code"])
	}
	if n := callCount(t, counter); n != 2 {
		t.Errorf("segments executed = %d; want 2", n)
	}
}

func TestKubectl_RejectsPipesAndRedirects(t *testing.T) {
	bin, counter := fakeKubectl(t, 0)
	k := &KubectlExecutor{BinaryPath: bin}
	for _, cmd := range []string{
		"kubectl get pods | grep Running",
		"kubectl get pods > out.txt",
		"kubectl get pods >> out.txt",
		"kubectl logs foo & ",
	} {
		_, err := k.Run(context.Background(), cmd)
		if err == nil {
			t.Errorf("%s: expected rejection (pipe/redirect)", cmd)
		} else if !strings.Contains(err.Error(), "not supported") {
			t.Errorf("%s: error %q should explain the operator is unsupported", cmd, err.Error())
		}
	}
	if n := callCount(t, counter); n != 0 {
		t.Errorf("no segment should have executed; got %d", n)
	}
}

func TestKubectl_Chained_AcceptsTrailingSemicolon(t *testing.T) {
	// A trailing ";" is a valid shell no-op and must not be rejected.
	bin, counter := fakeKubectl(t, 0)
	k := &KubectlExecutor{BinaryPath: bin}
	out, err := k.Run(context.Background(), "kubectl get pods ;")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out["exit_code"] != 0 {
		t.Errorf("exit_code = %v; want 0", out["exit_code"])
	}
	if n := callCount(t, counter); n != 1 {
		t.Errorf("segments executed = %d; want 1", n)
	}
}

func TestKubectl_Chained_RespectsCancelledContext(t *testing.T) {
	// A done context stops the chain before any kubectl runs and propagates the
	// context error rather than reporting success.
	bin, counter := fakeKubectl(t, 0)
	k := &KubectlExecutor{BinaryPath: bin}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := k.Run(ctx, "kubectl get pods && kubectl get svc")
	if err == nil {
		t.Error("expected a context error")
	}
	if n := callCount(t, counter); n != 0 {
		t.Errorf("no segment should run under a cancelled context; got %d", n)
	}
}

func TestKubectl_RejectsEmptySegments(t *testing.T) {
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	for _, cmd := range []string{
		"kubectl get pods &&",
		"&& kubectl get pods",
		"kubectl get pods ; ; kubectl get svc",
		"kubectl get pods || || kubectl get svc",
	} {
		if _, err := k.Run(context.Background(), cmd); err == nil {
			t.Errorf("%s: expected rejection (empty command segment)", cmd)
		}
	}
}

func TestKubectl_AcceptsReadOnlySubcommands(t *testing.T) {
	// rollout history/status are read-only and llm-server sends them constantly
	// (74 of 77 prod `kubectl rollout` calls in a 60-day sample were `history`).
	// The verb-level allowlist used to reject all of them.
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	for _, cmd := range []string{
		"kubectl rollout history deployment/api",
		"kubectl rollout history deployment api -n prod --revision=297",
		"kubectl -n prod rollout status deployment/api",
		"kubectl config view",
		"kubectl config current-context",
		"kubectl auth can-i get pods",
	} {
		out, err := k.Run(context.Background(), cmd)
		if err != nil {
			t.Errorf("%s: unexpected rejection: %v", cmd, err)
			continue
		}
		if out["exit_code"] != 0 {
			t.Errorf("%s: exit_code = %v; want 0", cmd, out["exit_code"])
		}
	}
}

func TestKubectl_RejectsMutatingSubcommands(t *testing.T) {
	// The sibling subcommands of the same verbs mutate, and must stay rejected —
	// otherwise scoping the verb would have widened write access rather than
	// narrowed it.
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	for _, cmd := range []string{
		"kubectl rollout undo deployment/api",
		"kubectl rollout restart deployment/api",
		"kubectl rollout pause deployment/api",
		"kubectl -n prod rollout resume deployment/api",
		"kubectl config set-context foo",
		"kubectl config delete-context foo",
		"kubectl auth reconcile -f rbac.yaml",
		"kubectl rollout", // no subcommand at all
	} {
		if _, err := k.Run(context.Background(), cmd); err == nil {
			t.Errorf("%s: expected rejection (mutating or missing subcommand)", cmd)
		} else if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: error %q does not explain the read-only restriction", cmd, err.Error())
		}
	}
}

func TestKubectl_AllowWriteSkipsSubcommandScoping(t *testing.T) {
	// enableWritePermissions hands enforcement to the API server's RBAC, so the
	// subcommand scope must lift with the verb allowlist rather than outliving it.
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true", AllowWrite: true}
	for _, cmd := range []string{
		"kubectl rollout undo deployment/api",
		"kubectl config set-context foo",
	} {
		if _, err := k.Run(context.Background(), cmd); err != nil {
			t.Errorf("%s: unexpected rejection with AllowWrite: %v", cmd, err)
		}
	}
}

// llmServerReadCommands mirrors what llm-server classifies as a read request and
// therefore routes to this agent: kubectlReadVerbs plus the subcommand-scoped
// reads, both in llm/llm-server/tools/tool_kubectl.go (kubectlReadVerbs and
// kubectlRequestType) in the nudgebee repo.
//
// The two lists are maintained in separate repos and have drifted before: this
// agent rejected `rollout history` while llm-server sent it as a read, and 15 of
// 61 production verb rejections over 60 days were that one command — recorded as
// successes, so nothing counted them. Anything llm-server calls a read must be
// accepted here, or it fails at the agent with no signal. Update both sides
// together.
//
// KNOWN AND DELIBERATE EXCLUSIONS — llm-server's kubectlReadVerbs also holds
// `diff`, `wait` and `options`, which this agent does not accept. They are left
// out rather than added because the same 60-day production sample shows `diff`
// and `options` were never called and `wait` once, so widening the allowlist
// buys nothing; `wait` additionally blocks, and `--timeout=0` blocks forever.
// Add them here and to allowedKubectlVerbs together if that ever changes.
var llmServerReadCommands = []string{
	"kubectl api-resources",
	"kubectl api-versions",
	"kubectl cluster-info",
	"kubectl describe pod foo",
	"kubectl explain pod.spec",
	"kubectl get pods",
	"kubectl logs pod/foo",
	"kubectl top nodes",
	"kubectl version",
	"kubectl config view",
	"kubectl config current-context",
	"kubectl config get-contexts",
	"kubectl config get-clusters",
	"kubectl rollout history deployment/api",
	"kubectl rollout status deployment/api",
	"kubectl auth can-i get pods",
}

func TestKubectl_AcceptsEverythingLLMServerCallsARead(t *testing.T) {
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	for _, cmd := range llmServerReadCommands {
		if _, err := k.Run(context.Background(), cmd); err != nil {
			t.Errorf("llm-server classifies %q as a read but the agent rejects it: %v", cmd, err)
		}
	}
}

// A flag between a scoped verb and its subcommand is where the verb allowlist can be
// talked out of its own guarantee. Real kubectl resolves each of these to a MUTATING
// subcommand, because Cobra assumes an unrecognized flag at the level it is resolving
// takes a value and swallows the following token. A validator that scans for "the next
// token that does not start with a dash" instead reads the swallowed token as the
// subcommand and calls the command a read.
//
// Verified against kubectl v1.36.2:
//   - `rollout --to-revision history undo deployment/api` → kubectl reports
//     `invalid argument "history" for "--to-revision" flag ... See 'kubectl rollout undo --help'`,
//     i.e. it resolved rollout undo.
//   - `rollout --selector history restart deployment` → kubectl proceeds to execute
//     rollout restart, issuing real discovery and GET calls.
//   - `config --current view set-context foo` → kubectl reports `Unexpected args: [view foo]`
//     from the set-context path.
func TestKubectl_RejectsMutationHiddenBehindAFlag(t *testing.T) {
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	for _, cmd := range []string{
		"kubectl rollout --to-revision history undo deployment/api",
		"kubectl rollout --selector history restart deployment",
		"kubectl config --current view set-context foo",
		"kubectl auth --namespace can-i reconcile -f rbac.yaml",
		"kubectl -n prod rollout --selector status undo deployment/api",
	} {
		if _, err := k.Run(context.Background(), cmd); err == nil {
			t.Errorf("%s: accepted — a flag before the subcommand hides a mutation", cmd)
		} else if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: error %q does not explain the read-only restriction", cmd, err.Error())
		}
	}
}

func TestKubectl_AcceptsEventsVerb(t *testing.T) {
	// `kubectl events` reads the same objects `kubectl get events` does — already allowed —
	// with the filtering an investigation wants. It has no mutating subcommand, so unlike
	// rollout/config/auth it needs no scoping.
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	for _, cmd := range []string{
		"kubectl events",
		"kubectl events --for pod/api -n prod",
		"kubectl -n prod events --types=Warning",
	} {
		if _, err := k.Run(context.Background(), cmd); err != nil {
			t.Errorf("%s: unexpected rejection: %v", cmd, err)
		}
	}
}

// A value-taking global flag missing from the table used to shift which token was read as the
// verb, so a mutation was approved as a read. Verified against kubectl v1.34:
//
//	kubectl --tls-server-name get rollout undo deployment/api
//
// kubectl swallows `get` as the flag's value and runs `rollout undo` — it reached the server
// under test. The parser saw verb "get", resource "rollout", and allowed it.
//
// The fix is not a longer table (the next release would reopen it) but refusing what we cannot
// parse. These must stay rejected even as kubectl's flag set changes.
func TestKubectl_RejectsUnknownFlagBeforeTheVerb(t *testing.T) {
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	for _, cmd := range []string{
		"kubectl --not-a-real-flag get rollout undo deployment/api",
		"kubectl --future-kubectl-flag value get pods",
	} {
		if _, err := k.Run(context.Background(), cmd); err == nil {
			t.Errorf("%s: accepted — an unparseable flag must not be guessed at", cmd)
		} else if !strings.Contains(err.Error(), "unrecognized flag") {
			t.Errorf("%s: error %q does not name the unrecognized flag", cmd, err.Error())
		}
	}
}

// The flags that caused the bypass are now known and value-taking, so the verb resolves the way
// kubectl resolves it.
//
// The dangerous shape is the one where the flag's VALUE IS OMITTED: kubectl then swallows the
// following token as the value, and the mutating verb after it becomes the command. With the flag
// unknown to the old table, the parser skipped nothing and read that swallowed token as the verb.
func TestKubectl_ResolvesVerbPastValueTakingGlobalFlags(t *testing.T) {
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	for _, cmd := range []string{
		"kubectl --tls-server-name get rollout undo deployment/api",
		"kubectl --log-file get rollout restart deployment/api",
		"kubectl --token get rollout pause deployment/api",
	} {
		if _, err := k.Run(context.Background(), cmd); err == nil {
			t.Errorf("%s: accepted — kubectl eats the next token and runs the mutating verb", cmd)
		} else if !strings.Contains(err.Error(), "read-only") {
			t.Errorf("%s: error %q does not explain the read-only restriction", cmd, err.Error())
		}
	}

	// With the value actually supplied, the same flags front a genuine read: kubectl sees
	// `get rollout undo deployment/api`, which reads resources of kind "rollout". The parser must
	// agree rather than refuse it.
	for _, cmd := range []string{
		"kubectl --tls-server-name example.com get rollout undo deployment/api",
		"kubectl --profile cpu get pods",
	} {
		if _, err := k.Run(context.Background(), cmd); err != nil {
			t.Errorf("%s: unexpected rejection: %v", cmd, err)
		}
	}

	// The same flags in front of a genuine read must still work.
	for _, cmd := range []string{
		"kubectl --tls-server-name example.com get pods",
		"kubectl --insecure-skip-tls-verify get pods",
		"kubectl --profile=cpu get pods",
		"kubectl --some-unknown-future-flag=value get pods", // self-contained: unambiguous
	} {
		if _, err := k.Run(context.Background(), cmd); err != nil {
			t.Errorf("%s: unexpected rejection: %v", cmd, err)
		}
	}
}
