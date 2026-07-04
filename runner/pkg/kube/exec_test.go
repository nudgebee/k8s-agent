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
