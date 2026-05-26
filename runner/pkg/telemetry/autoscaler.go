package telemetry

import (
	"context"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// AutoScalerInfo is the subset of the autoscaler-discovery output that
// the collector + UI consume (autoScalerType / autoScalerVersion /
// autoScalerNamespace + a boolean Enabled).
type AutoScalerInfo struct {
	Enabled   bool
	Type      string // "karpenter" | "cluster-autoscaler" | "gke" | ""
	Version   string
	Namespace string
}

// DetectAutoScaler looks for a Karpenter or cluster-autoscaler deployment
// cluster-wide and falls back to GKE-as-autoscaler when the provider hint
// is GKE (matching legacy KarpenterDiscovery / AutoScalerDiscovery /
// AutoScalerForGKEDiscovery semantics).
//
// One List() per call — cheap enough to run per heartbeat tick. Errors are
// swallowed (returns zero-value) so an RBAC gap on a single resource type
// can't break the rest of telemetry.
func DetectAutoScaler(ctx context.Context, cs kubernetes.Interface, provider string, logger *slog.Logger) AutoScalerInfo {
	if logger == nil {
		logger = slog.Default()
	}
	if cs == nil {
		return AutoScalerInfo{}
	}
	deps, err := cs.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		logger.Debug("autoscaler discovery: deployment list failed", "err", err)
		// Even on RBAC failure we can still surface GKE-as-autoscaler.
		if provider == providerGKE {
			return AutoScalerInfo{Enabled: true, Type: "gke"}
		}
		return AutoScalerInfo{}
	}
	for _, d := range deps.Items {
		name := strings.ToLower(d.Name)
		switch {
		case strings.Contains(name, "karpenter"):
			return AutoScalerInfo{
				Enabled:   true,
				Type:      "karpenter",
				Version:   imageTag(d.Spec.Template.Spec.Containers),
				Namespace: d.Namespace,
			}
		case strings.Contains(name, "cluster-autoscaler"):
			return AutoScalerInfo{
				Enabled:   true,
				Type:      "cluster-autoscaler",
				Version:   imageTag(d.Spec.Template.Spec.Containers),
				Namespace: d.Namespace,
			}
		}
	}
	// GKE clusters get the managed autoscaler for free even without a
	// deployment in the cluster — surface it so the UI doesn't claim
	// "no autoscaler" on a managed cluster.
	if provider == providerGKE {
		return AutoScalerInfo{Enabled: true, Type: "gke"}
	}
	return AutoScalerInfo{}
}

// imageTag returns the tag of the first container's image. "ghcr.io/foo:v1.2.3"
// → "v1.2.3"; missing tag → "".
func imageTag(containers []corev1.Container) string {
	if len(containers) == 0 {
		return ""
	}
	img := containers[0].Image
	i := strings.LastIndex(img, ":")
	if i < 0 || i == len(img)-1 {
		return ""
	}
	tag := img[i+1:]
	// Guard against `docker.io/foo@sha256:...` — digest is not a version.
	if strings.Contains(img, "@sha256:") {
		return ""
	}
	return tag
}
