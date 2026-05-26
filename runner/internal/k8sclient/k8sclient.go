// Package k8sclient builds a kubernetes.Interface using in-cluster credentials
// when running as a pod, and falling back to ~/.kube/config (or KUBECONFIG)
// for local development. Same pattern as every controller-runtime app.
package k8sclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// New returns a typed Kubernetes clientset and the rest.Config used to build it.
// kubeconfigPath overrides the env / default lookup; pass "" to use defaults.
func New(kubeconfigPath string) (kubernetes.Interface, *rest.Config, error) {
	cfg, err := loadConfig(kubeconfigPath)
	if err != nil {
		return nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("k8sclient: build clientset: %w", err)
	}
	return cs, cfg, nil
}

func loadConfig(kubeconfigPath string) (*rest.Config, error) {
	// In-cluster first.
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	} else if !errors.Is(err, rest.ErrNotInCluster) {
		return nil, fmt.Errorf("k8sclient: in-cluster config: %w", err)
	}

	// Kubeconfig fallback.
	if kubeconfigPath == "" {
		kubeconfigPath = os.Getenv("KUBECONFIG")
	}
	if kubeconfigPath == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			kubeconfigPath = filepath.Join(home, ".kube", "config")
		}
	}
	if kubeconfigPath == "" {
		return nil, errors.New("k8sclient: no in-cluster config and no kubeconfig found")
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("k8sclient: load kubeconfig %s: %w", kubeconfigPath, err)
	}
	return cfg, nil
}
