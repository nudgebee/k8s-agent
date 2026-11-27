package kube

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// identityKubectl prints the token from the kubeconfig it was given, the
// kubeconfig's directory and file modes, and its own args.
func identityKubectl(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kubectl")
	script := `#!/bin/sh
if [ -n "$KUBECONFIG" ]; then
  echo "TOKEN=$(grep 'token:' "$KUBECONFIG" | sed 's/.*token: //')"
  echo "NS=$(grep 'namespace:' "$KUBECONFIG" | sed 's/.*namespace: //')"
  echo "FILEMODE=$(ls -l "$KUBECONFIG" | cut -c1-10)"
  echo "DIRMODE=$(ls -ld "$(dirname "$KUBECONFIG")" | cut -c1-10)"
  echo "KCPATH=$KUBECONFIG"
else
  echo "TOKEN=none"
fi
echo "ARGS=$*"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func splitExecutor(t *testing.T, allowWrite, withWriter bool) (*KubectlExecutor, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "kc")
	creds := &Credentials{
		Server: "https://192.0.2.10", CAFile: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
		Namespace: "nb",
		Reader:    func() (string, error) { return "reader-token", nil },
		Dir:       dir,
	}
	if withWriter {
		creds.Writer = func() (string, error) { return "writer-token", nil }
	}
	return &KubectlExecutor{BinaryPath: identityKubectl(t), AllowWrite: allowWrite, Credentials: creds}, dir
}

func stdoutOf(t *testing.T, res map[string]any) string {
	t.Helper()
	s, _ := res["stdout"].(string)
	return s
}

func TestSplit_SegmentIdentity(t *testing.T) {
	cases := []struct {
		name       string
		allowWrite bool
		withWriter bool
		cmd        string
		wantTokens []string
	}{
		{"read runs as reader", false, true, "kubectl get pods -A", []string{"reader-token"}},
		{"write runs as writer", true, true, "kubectl delete pod x -n a", []string{"writer-token"}},
		{"chain picks per segment", true, true, "kubectl get pods && kubectl rollout restart deploy/x", []string{"reader-token", "writer-token"}},
		{"auth can-i asks as writer", false, true, "kubectl auth can-i patch deployment/x", []string{"writer-token"}},
		{"auth can-i without writer asks as reader", false, false, "kubectl auth can-i patch deployment/x", []string{"reader-token"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, _ := splitExecutor(t, tc.allowWrite, tc.withWriter)
			res, err := k.Run(context.Background(), tc.cmd)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			out := stdoutOf(t, res)
			var got []string
			for _, line := range strings.Split(out, "\n") {
				if v, ok := strings.CutPrefix(line, "TOKEN="); ok {
					got = append(got, strings.Trim(v, `"`))
				}
			}
			if strings.Join(got, ",") != strings.Join(tc.wantTokens, ",") {
				t.Fatalf("tokens = %v, want %v\n%s", got, tc.wantTokens, out)
			}
		})
	}
}

func TestSplit_WriteWithoutWriterRefused(t *testing.T) {
	k, _ := splitExecutor(t, true, false)
	_, err := k.Run(context.Background(), "kubectl delete pod x")
	if err == nil || !strings.Contains(err.Error(), "changes are off") {
		t.Fatalf("err = %v, want changes are off", err)
	}
}

func TestSplit_RefusedArgs(t *testing.T) {
	for _, cmd := range []string{
		"kubectl --as=system:serviceaccount:nb:runner-writer get pods",
		"kubectl get pods --as system:admin",
		"kubectl get pods --token abc",
		"kubectl get pods --token=abc",
		"kubectl -s https://203.0.113.5 get pods",
		"kubectl -shttps://203.0.113.5 get pods",
		"kubectl get pods --server=https://203.0.113.5",
		"kubectl get pods --kubeconfig /tmp/x",
		"kubectl get pods --context other",
		"kubectl get pods --insecure-skip-tls-verify",
		"kubectl get pods -v=9",
		"kubectl get pods -v9",
		"kubectl get pods --v 9",
		"kubectl config view --raw",
		"kubectl config view --flatten",
		"kubectl debug node/n1 -it --image=busybox",
		// short-flag clusters: pflag reads the first value-taking letter
		"kubectl get pods -As https://203.0.113.5",
		"kubectl get pods -Asfoo",
		"kubectl get pods -Av6",
		"kubectl get pods -wAs http://203.0.113.5",
		"kubectl get pods -v=9",
	} {
		k, _ := splitExecutor(t, true, true)
		if _, err := k.Run(context.Background(), cmd); err == nil {
			t.Errorf("%q: accepted, want refused", cmd)
		}
	}
}

// Flags after `--` belong to the command kubectl runs, not to kubectl.
func TestSplit_ArgsAfterDoubleDashAllowed(t *testing.T) {
	k, _ := splitExecutor(t, true, true)
	res, err := k.Run(context.Background(), `kubectl exec pod-a -n a -- sh -c "curl --token x"`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(stdoutOf(t, res), "TOKEN=\"writer-token\"") {
		t.Fatalf("exec should run as writer:\n%s", stdoutOf(t, res))
	}
}

func TestSplit_KubeconfigPrivateAndRemoved(t *testing.T) {
	k, dir := splitExecutor(t, false, true)
	res, err := k.Run(context.Background(), "kubectl get pods")
	if err != nil {
		t.Fatal(err)
	}
	out := stdoutOf(t, res)
	if !strings.Contains(out, "FILEMODE=-rw-------") || !strings.Contains(out, "DIRMODE=drwx------") {
		t.Fatalf("kubeconfig not private:\n%s", out)
	}
	if !strings.Contains(out, `NS="nb"`) {
		t.Fatalf("default namespace missing:\n%s", out)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("kubeconfig dirs left behind: %v", entries)
	}
	if strings.Contains(out, "ARGS=") && strings.Contains(strings.SplitN(out, "ARGS=", 2)[1], "token") {
		t.Fatalf("token appears in kubectl args:\n%s", out)
	}
}

func TestSplit_TokenFailureNeverRuns(t *testing.T) {
	k, _ := splitExecutor(t, false, true)
	k.Credentials.Reader = func() (string, error) { return "", errors.New("no token") }
	res, err := k.Run(context.Background(), "kubectl get pods")
	if err == nil || !strings.Contains(err.Error(), "reader credential") {
		t.Fatalf("err = %v, want reader credential error", err)
	}
	if strings.Contains(stdoutOf(t, res), "ARGS=") {
		t.Fatal("kubectl ran without its kubeconfig")
	}
}

func TestSplit_KubeconfigDirBlockedAsArgument(t *testing.T) {
	k, _ := splitExecutor(t, true, true)
	if _, err := k.Run(context.Background(), "kubectl cp /tmp/nb-kubeconfig/kc-1/config a/pod:/x"); err == nil {
		t.Fatal("copying a kubeconfig out must be refused")
	}
}

// Without Credentials the executor keeps the process credential (legacy).
func TestLegacy_NoKubeconfig(t *testing.T) {
	k := &KubectlExecutor{BinaryPath: identityKubectl(t)}
	t.Setenv("KUBECONFIG", "")
	res, err := k.Run(context.Background(), "kubectl get pods --context x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdoutOf(t, res), "TOKEN=none") {
		t.Fatalf("legacy mode wrote a kubeconfig:\n%s", stdoutOf(t, res))
	}
}

// Clusters of bool shorthands, and value shorthands other than -s/-v, stay
// allowed.
func TestSplit_ShortFlagClustersAllowed(t *testing.T) {
	for _, cmd := range []string{
		"kubectl get pods -A",
		"kubectl get pods -Aw",
		"kubectl get pods -A -ojson",
		"kubectl get pods -ojsonpath={.items[*].spec.serviceAccountName}",
		"kubectl get pods -n sys -l app=svc",
	} {
		k, _ := splitExecutor(t, false, true)
		if _, err := k.Run(context.Background(), cmd); err != nil {
			t.Errorf("%q: refused: %v", cmd, err)
		}
	}
}

// The identity hint overrides the parser: "read" may be used by anyone (it
// only removes rights), "write" needs the writer and write permissions.
func TestSplit_IdentityHint(t *testing.T) {
	cases := []struct {
		name       string
		allowWrite bool
		withWriter bool
		cmd, hint  string
		wantToken  string
		wantErr    string
	}{
		{"read hint downgrades a write", true, true, "kubectl delete pod x", "read", "reader-token", ""},
		{"read hint on can-i asks as reader", false, true, "kubectl auth can-i delete pods", "read", "reader-token", ""},
		{"write hint upgrades a read", true, true, "kubectl get pods", "write", "writer-token", ""},
		{"write hint needs write permissions", false, true, "kubectl get pods", "write", "", "enableWritePermissions"},
		{"write hint needs the writer", true, false, "kubectl get pods", "write", "", "changes are off"},
		{"unknown hint refused", true, true, "kubectl get pods", "admin", "", "unknown identity"},
		{"read hint does not bypass the read-only allowlist", false, true, "kubectl delete pod x", "read", "", "read-only allowlist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k, _ := splitExecutor(t, tc.allowWrite, tc.withWriter)
			res, err := k.RunAs(context.Background(), tc.cmd, "", tc.hint)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RunAs: %v", err)
			}
			if !strings.Contains(stdoutOf(t, res), `TOKEN="`+tc.wantToken+`"`) {
				t.Fatalf("want %s:\n%s", tc.wantToken, stdoutOf(t, res))
			}
		})
	}
}

func TestIsWriteIdentityHint(t *testing.T) {
	for params, want := range map[string]bool{"": false, "read": false, "write": true, "reader": true, "WRITE": true} {
		p := map[string]any{}
		if params != "" {
			p[IdentityParam] = params
		}
		if got := IsWriteIdentityHint(p); got != want {
			t.Errorf("IsWriteIdentityHint(%q) = %v, want %v", params, got, want)
		}
	}
	if !IsWriteIdentityHint(map[string]any{IdentityParam: 1}) {
		t.Error("a non-string hint must count as a write")
	}
}
