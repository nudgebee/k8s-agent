package telemetry

import (
	"context"
	"log/slog"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// CanUpdatePrometheusRulesClusterWide asks the API server whether the agent's
// service account may update PrometheusRules in every namespace. It checks the
// real RBAC rather than a chart value, so a customised ClusterRole reports what
// the agent can actually do. Any failure reports false.
func CanUpdatePrometheusRulesClusterWide(ctx context.Context, kube kubernetes.Interface, logger *slog.Logger) bool {
	if kube == nil {
		return false
	}
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Group:    "monitoring.coreos.com",
				Resource: "prometheusrules",
				Verb:     "update",
				// Empty namespace: every namespace.
			},
		},
	}
	res, err := kube.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		if logger != nil {
			logger.Warn("prometheusrule cluster-write check failed", "err", err)
		}
		return false
	}
	return res.Status.Allowed
}
