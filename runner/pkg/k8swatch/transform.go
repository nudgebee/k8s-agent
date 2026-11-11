package k8swatch

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// OwnTransform strips managedFields from the objects only the watcher caches,
// before they enter the cache, and nothing else: that is what kubewatch's
// informers did, so the evidence built from them is what kubewatch sent.
func OwnTransform(obj any) (any, error) {
	switch o := obj.(type) {
	case *corev1.ConfigMap:
		o.ManagedFields = nil
	case *corev1.Service:
		o.ManagedFields = nil
	case *networkingv1.Ingress:
		o.ManagedFields = nil
	case *unstructured.Unstructured:
		unstructured.RemoveNestedField(o.Object, "metadata", "managedFields")
	}
	return obj, nil
}
