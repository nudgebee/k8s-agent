package kube

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultKubeconfigDir is the parent of the per-command kubeconfig
// directories. mountedSecretPaths blocks it, so no kubectl argument (for
// example `kubectl cp /tmp/nb-kubeconfig/... pod:/x`) can read or copy one.
const DefaultKubeconfigDir = "/tmp/nb-kubeconfig"

// Identity names a kubectl segment runs as.
const (
	identityReader = "reader"
	identityWriter = "writer"
)

// Credentials gives every kubectl segment a short-lived kubeconfig for the
// identity matching it, instead of the process's own credential: reads run as
// the reader, writes as the writer, so the API server refuses a write the
// read-only parser let through. nil Credentials on the executor = kubectl uses
// the process's own credential (legacy).
type Credentials struct {
	Server string
	// CAFile or CAData: the cluster CA. CAFile wins when both are set.
	CAFile string
	CAData []byte
	// Namespace is the default namespace, matching what in-cluster kubectl
	// would use (the pod's own namespace).
	Namespace string
	Reader    func() (string, error)
	// Writer is nil when changes are off on this agent.
	Writer func() (string, error)
	// Dir overrides DefaultKubeconfigDir (tests).
	Dir string
}

// identityFlags are kubectl flags that choose a credential, a server or a
// file kubectl loads config from. With per-command kubeconfigs the executor
// alone decides these; a caller-supplied one is refused.
var identityFlags = []string{
	"--as", "--as-group", "--as-uid", "--as-user-extra",
	"--token", "--kubeconfig", "--kuberc",
	"--server", "--user", "--username", "--password",
	"--context", "--cluster",
	"--client-certificate", "--client-key", "--certificate-authority",
	"--insecure-skip-tls-verify", "--tls-server-name",
	"--cache-dir", "--log-file", "--log-dir",
}

// boolShorthands are kubectl's single-letter flags that take no value. In a
// cluster such as `-Aw`, pflag reads letters until the first one that takes a
// value; the rest of the token (or the next token) is that value.
var boolShorthands = map[byte]bool{'A': true, 'w': true, 'i': true, 't': true, 'R': true, 'h': true, 'q': true}

// shortValueFlag returns the first value-taking shorthand in a single-dash
// token (`-As URL` -> 's', `-Av6` -> 'v', `-ojson` -> 'o'), or 0. Unknown
// letters count as value-taking, which is how pflag would stop on them.
func shortValueFlag(a string) byte {
	if len(a) < 2 || a[0] != '-' || a[1] == '-' {
		return 0
	}
	for i := 1; i < len(a); i++ {
		if a[i] == '=' {
			return 0
		}
		if !boolShorthands[a[i]] {
			return a[i]
		}
	}
	return 0
}

// checkSplitModeArgs refuses what a per-command kubeconfig cannot allow:
// identity/endpoint flags (before `--`; later tokens belong to the command
// kubectl runs and are not parsed as kubectl flags), verbosity flags (high
// levels print request headers), and the `config` and `debug` verbs.
func checkSplitModeArgs(verb string, args []string) error {
	switch verb {
	case "config":
		return errors.New("kubectl: the config verb is not available on this agent (each command runs with its own short-lived credential)")
	case "debug":
		return errors.New("kubectl: debug needs ephemeral containers, which no agent identity is granted")
	}
	for _, a := range args {
		if a == "--" {
			break
		}
		for _, f := range identityFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return fmt.Errorf("kubectl: flag %s is not allowed; the agent chooses the credential and server for each command", f)
			}
		}
		if a == "--v" || strings.HasPrefix(a, "--v=") {
			return errors.New("kubectl: verbosity flags (-v) are not allowed on this agent")
		}
		switch shortValueFlag(a) {
		case 's':
			return errors.New("kubectl: flag -s is not allowed; the agent chooses the server for each command")
		case 'v':
			return errors.New("kubectl: verbosity flags (-v) are not allowed on this agent")
		}
	}
	return nil
}

// IdentityParam is the kubectl_command_executor param that overrides the
// executor's own read/write classification:
//   - "read": run every segment as the reader. Allowed for any request: it
//     can only remove rights, and a write run as the reader is refused by the
//     API server. Lets the server correct a read the parser takes for a write
//     without an agent upgrade.
//   - "write": run every segment as the writer. Needs the writer identity and
//     runner.enableWritePermissions, and is treated as a write by the request
//     validator (a valid relay signature where the agent verifies them), the
//     same bar as a kubectl write today. Lets the server correct a write the
//     parser takes for a read.
//   - "" (absent): the executor classifies each segment.
const IdentityParam = "identity"

// IsWriteIdentityHint reports whether params ask for the writer, or carry a
// hint the executor will refuse; the validator treats both as writes.
func IsWriteIdentityHint(params map[string]any) bool {
	raw, ok := params[IdentityParam]
	if !ok || raw == nil {
		return false
	}
	v, isString := raw.(string)
	return !isString || (v != "" && v != "read")
}

func (k *KubectlExecutor) checkIdentityHint(hint string) error {
	switch hint {
	case "", "read":
		return nil
	case "write":
		if !k.AllowWrite {
			return errors.New("kubectl: identity=write needs runner.enableWritePermissions")
		}
		if k.Credentials != nil && k.Credentials.Writer == nil {
			return errors.New("kubectl: changes are off on this agent (runner.changes.enabled)")
		}
		return nil
	}
	return fmt.Errorf("kubectl: unknown %s %q (want read or write)", IdentityParam, hint)
}

// segmentIdentity picks the identity a validated segment runs as.
func (k *KubectlExecutor) segmentIdentity(verb, subcommand string, args []string, hint string) (string, error) {
	switch hint {
	case "read":
		return identityReader, nil
	case "write":
		return identityWriter, nil
	}
	// `auth can-i` and `whoami` answer for whoever runs them; callers ask
	// whether the agent can make a change, so they ask as the writer.
	if verb == "auth" && (subcommand == "can-i" || subcommand == "whoami") {
		if k.Credentials.Writer != nil {
			return identityWriter, nil
		}
		return identityReader, nil
	}
	if (&KubectlExecutor{}).validateSegment(args) == nil {
		return identityReader, nil
	}
	if k.Credentials.Writer == nil {
		return "", errors.New("kubectl: changes are off on this agent (runner.changes.enabled)")
	}
	return identityWriter, nil
}

// kubeconfig writes a kubeconfig for identity into a fresh private directory
// and returns its path and a cleanup func. Any failure is returned: a segment
// never runs without its own kubeconfig.
func (c *Credentials) kubeconfig(identity string) (string, func(), error) {
	tokenFn := c.Reader
	if identity == identityWriter {
		tokenFn = c.Writer
	}
	if tokenFn == nil {
		return "", nil, fmt.Errorf("kubectl: no %s credential on this agent", identity)
	}
	token, err := tokenFn()
	if err != nil {
		return "", nil, fmt.Errorf("kubectl: %s credential: %w", identity, err)
	}
	parent := c.Dir
	if parent == "" {
		parent = DefaultKubeconfigDir
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", nil, fmt.Errorf("kubectl: kubeconfig dir: %w", err)
	}
	dir, err := os.MkdirTemp(parent, "kc-")
	if err != nil {
		return "", nil, fmt.Errorf("kubectl: kubeconfig dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(c.render(token)), 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("kubectl: write kubeconfig: %w", err)
	}
	return path, cleanup, nil
}

func (c *Credentials) render(token string) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\nclusters:\n- name: cluster\n  cluster:\n")
	fmt.Fprintf(&b, "    server: %q\n", c.Server)
	switch {
	case c.CAFile != "":
		fmt.Fprintf(&b, "    certificate-authority: %q\n", c.CAFile)
	case len(c.CAData) > 0:
		fmt.Fprintf(&b, "    certificate-authority-data: %q\n", base64.StdEncoding.EncodeToString(c.CAData))
	}
	b.WriteString("users:\n- name: agent\n  user:\n")
	fmt.Fprintf(&b, "    token: %q\n", token)
	b.WriteString("contexts:\n- name: agent\n  context:\n    cluster: cluster\n    user: agent\n")
	if c.Namespace != "" {
		fmt.Fprintf(&b, "    namespace: %q\n", c.Namespace)
	}
	b.WriteString("current-context: agent\n")
	return b.String()
}

// envWithKubeconfig returns the process environment with KUBECONFIG set to
// path and any inherited KUBECONFIG removed.
func envWithKubeconfig(path string) []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, "KUBECONFIG=") {
			out = append(out, e)
		}
	}
	return append(out, "KUBECONFIG="+path)
}
