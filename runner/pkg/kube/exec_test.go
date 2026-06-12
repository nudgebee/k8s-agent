package kube

import (
	"context"
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
