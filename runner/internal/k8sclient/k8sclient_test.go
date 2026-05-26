package k8sclient

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNew_WithExplicitKubeconfig_FailsCleanlyOnMissingFile(t *testing.T) {
	// Bypass in-cluster (no env vars set). loadConfig falls through to the
	// kubeconfig path; with a non-existent file the typed error from clientcmd
	// must propagate.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("KUBECONFIG", "")

	bogus := filepath.Join(t.TempDir(), "no-such-config")
	_, _, err := New(bogus)
	if err == nil {
		t.Fatalf("New(%q) = nil error; want error", bogus)
	}
}

func TestNew_NoConfigFound_ReturnsError(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	t.Setenv("KUBECONFIG", "")
	t.Setenv("HOME", t.TempDir()) // ensure no ~/.kube/config

	_, _, err := New("")
	if err == nil {
		t.Fatal("expected error when no in-cluster config and no kubeconfig present")
	}
}

func TestLoadConfig_HonorsKubeconfigEnv(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	// Write a minimal valid kubeconfig pointing at a fake cluster.
	dir := t.TempDir()
	kc := filepath.Join(dir, "config")
	const yaml = `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: kind
contexts:
- context:
    cluster: kind
    user: kind
  name: kind
current-context: kind
users:
- name: kind
`
	if err := os.WriteFile(kc, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kc)

	cfg, err := loadConfig("")
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Errorf("Host = %q; want https://127.0.0.1:6443", cfg.Host)
	}
}

// Sanity: file-not-found must surface with a recognisable hint, not a panic.
func TestLoadConfig_NonExistentPath(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	_, err := loadConfig(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected error")
	}
	// Don't pin to a specific error type — clientcmd wraps; just ensure non-nil.
	if errors.Is(err, nil) {
		t.Fatal("nil error wrapped")
	}
}
