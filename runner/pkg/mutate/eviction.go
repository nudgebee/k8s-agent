package mutate

import (
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// asPolicyEviction converts our internal helper type to the typed policy/v1
// Eviction the client-go EvictV1 method expects.
func asPolicyEviction(e *policyEviction) *policyv1.Eviction {
	return &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e.Name,
			Namespace: e.Namespace,
		},
	}
}
