package kube

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// Secret-access policy for kubectl_command_executor and get_resource.
//
// The agent is the one boundary every relay caller crosses, so Secret values
// are refused here regardless of what any caller filtered upstream, and
// regardless of runner.enableWritePermissions. The rules mirror the kubectl
// denylist the backend applies before relaying, so a command refused there is
// refused here too, and a command that slips past it still stops here.

// secretBearingKinds are resource kinds whose objects hold credentials, in
// every spelling kubectl accepts: singular, plural and the short names the
// External Secrets Operator and Secrets Store CSI CRDs declare. `ss` is
// SecretStore — statefulsets are `sts`.
var secretBearingKinds = map[string]struct{}{
	"secret": {}, "secrets": {},
	"sealedsecret": {}, "sealedsecrets": {},
	"externalsecret": {}, "externalsecrets": {}, "es": {},
	"secretproviderclass": {}, "secretproviderclasses": {}, "spc": {}, "spcs": {},
	"clustersecretstore": {}, "clustersecretstores": {}, "css": {},
	"secretstore": {}, "secretstores": {}, "ss": {},
}

// secretBearingAPIResources are the same kinds as they appear in an API path
// (`--raw /api/v1/namespaces/x/secrets`): always the plural resource name.
var secretBearingAPIResources = map[string]struct{}{
	"secrets":               {},
	"sealedsecrets":         {},
	"externalsecrets":       {},
	"secretproviderclasses": {},
	"clustersecretstores":   {},
	"secretstores":          {},
}

// resourceTakingVerbs are the subcommands whose positional arguments name
// resource kinds. Read and write verbs both: write mode must not create, edit
// or delete Secrets through this path either.
var resourceTakingVerbs = map[string]struct{}{
	"get": {}, "describe": {}, "top": {}, "explain": {}, "wait": {},
	"create": {}, "delete": {}, "edit": {}, "apply": {}, "replace": {},
	"scale": {}, "rollout": {}, "set": {},
	"label": {}, "annotate": {}, "patch": {},
}

// mountedSecretPaths are where kubelet mounts ServiceAccount tokens and Secret
// volumes. A kubectl argument naming one reads a local credential file on the
// agent pod (`-f`, `--token-file`, a template file) or, via exec, a target pod.
var mountedSecretPaths = []string{
	"/var/run/secrets",
	"/run/secrets",
	"/var/lib/kubelet/pods",
	"kubernetes.io/serviceaccount",
}

// checkSecretAccess refuses a kubectl segment (verb + args, "kubectl" already
// stripped) that would read or write a secret-bearing resource or a local
// credential file. It runs before the read-only allowlist and in write mode.
func checkSecretAccess(verb string, args []string) error {
	if err := checkRawPaths(args); err != nil {
		return err
	}
	if err := checkOutputFormats(args); err != nil {
		return err
	}
	if p := mountedSecretPathIn(args); p != "" {
		return fmt.Errorf("kubectl: reading mounted secret paths (%s) is blocked", p)
	}
	if verb == "create" && containsToken(args, "token") {
		return fmt.Errorf("kubectl: minting ServiceAccount tokens (create token) is blocked")
	}
	if _, ok := resourceTakingVerbs[verb]; !ok {
		return nil
	}
	if kind := secretBearingKindIn(args); kind != "" {
		return fmt.Errorf("kubectl: access to %q is blocked. Secret-bearing kinds (secrets, sealedsecrets, externalsecrets, secretstores, secretproviderclasses) are not readable through the agent", kind)
	}
	return nil
}

// secretBearingKindIn returns the first secret-bearing kind named by a
// non-flag token, or "". Each token is split on `,` first (`pods,secrets`),
// then stripped of `/name` and `.group` (`secret/x`, `secrets.v1.`).
//
// Flag values are scanned too, since we do not track which flags take one: a
// namespace literally named `secrets` (or `es`, `ss`) is refused. Accepted:
// a false refusal is a clear error, a miss is a leak.
func secretBearingKindIn(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		for _, part := range strings.Split(a, ",") {
			part, _, _ = strings.Cut(part, "/")
			part, _, _ = strings.Cut(part, ".")
			part = strings.ToLower(part)
			if _, ok := secretBearingKinds[part]; ok {
				return part
			}
		}
	}
	return ""
}

// checkRawPaths refuses `--raw PATH` / `--raw=PATH` when PATH addresses a
// secret-bearing resource or ServiceAccount token minting. `--raw` skips
// kubectl's resource parsing, so the kind check above never sees the path.
// The path is percent-decoded first: the API server decodes it before routing,
// so `%73ecrets` reaches the secrets handler.
func checkRawPaths(args []string) error {
	for i, a := range args {
		var raw string
		switch {
		case a == "--raw" && i+1 < len(args):
			raw = args[i+1]
		case strings.HasPrefix(a, "--raw="):
			raw = strings.TrimPrefix(a, "--raw=")
		default:
			continue
		}
		p, _, _ := strings.Cut(raw, "?")
		decoded, err := url.PathUnescape(p)
		if err != nil {
			return fmt.Errorf("kubectl: --raw path %q is not a valid URL path", raw)
		}
		segs := strings.Split(strings.ToLower(decoded), "/")
		for j, s := range segs {
			if _, ok := secretBearingAPIResources[s]; ok {
				return fmt.Errorf("kubectl: --raw access to %q is blocked. Secret-bearing resources are not readable through the agent", s)
			}
			if s == "token" && j >= 2 && segs[j-2] == "serviceaccounts" {
				return fmt.Errorf("kubectl: --raw access to ServiceAccount tokens is blocked")
			}
		}
	}
	return nil
}

// checkOutputFormats refuses file-backed output formats (`-o jsonpath-file=`,
// `go-template-file`, `custom-columns-file`, `templatefile`). They make kubectl
// read a file from the agent pod's filesystem as a template, and a parse error
// echoes its content — the runner's own ServiceAccount token included.
func checkOutputFormats(args []string) error {
	for i, a := range args {
		var v string
		switch {
		case (a == "-o" || a == "--output") && i+1 < len(args):
			v = args[i+1]
		case strings.HasPrefix(a, "--output="):
			v = strings.TrimPrefix(a, "--output=")
		case strings.HasPrefix(a, "-o") && !strings.HasPrefix(a, "--") && len(a) > 2:
			v = strings.TrimPrefix(a[2:], "=")
		default:
			continue
		}
		format, _, _ := strings.Cut(v, "=")
		format = strings.ToLower(format)
		if strings.HasSuffix(format, "-file") || format == "templatefile" {
			return fmt.Errorf("kubectl: output format %q reads a local file and is blocked; pass the template inline (e.g. -o jsonpath='{...}')", format)
		}
	}
	return nil
}

// mountedSecretPathIn returns the mounted-secret path an argument names, or "".
// Arguments are cleaned first so `/var/run/../run/secrets` still matches.
func mountedSecretPathIn(args []string) string {
	for _, a := range args {
		candidates := []string{a}
		if strings.Contains(a, "/") {
			// A flag carries its path after `=` (`--token-file=/var/...`); clean
			// that part, since path.Clean on the whole token would not resolve it.
			_, v, hasEq := strings.Cut(a, "=")
			if !hasEq {
				v = a
			}
			candidates = append(candidates, path.Clean(v))
		}
		for _, c := range candidates {
			for _, p := range mountedSecretPaths {
				if strings.Contains(c, p) {
					return p
				}
			}
		}
	}
	return ""
}

func containsToken(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// redactSecretData blanks the values of a core/v1 Secret in place, keeping its
// keys so callers can still count and name them.
// The last-applied-configuration annotation is dropped: `kubectl apply` stores
// the full manifest there, values included.
func redactSecretData(obj map[string]any) {
	for _, field := range []string{"data", "stringData"} {
		m, ok := obj[field].(map[string]any)
		if !ok {
			continue
		}
		for k := range m {
			m[k] = ""
		}
	}
	meta, _ := obj["metadata"].(map[string]any)
	if ann, ok := meta["annotations"].(map[string]any); ok {
		delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
	}
}
