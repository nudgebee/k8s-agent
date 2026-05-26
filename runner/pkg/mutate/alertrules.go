package mutate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// PrometheusRule CRD GVR. Standard prometheus-operator group.
var prometheusRuleGVR = schema.GroupVersionResource{
	Group: "monitoring.coreos.com", Version: "v1", Resource: "prometheusrules",
}

// SetDynamic wires a dynamic client. Required for the alert-rule CRUD path
// (PrometheusRule CRDs aren't typed in client-go).
func (m *Mutator) SetDynamic(d dynamic.Interface) { m.dynamic = d }

// CreateOrReplacePrometheusRule applies a PrometheusRule object. If a rule
// with the same name+namespace exists, it's overwritten (with ResourceVersion
// preservation so the apiserver doesn't reject the update).
//
// rule is the raw PrometheusRule manifest (any). It MUST contain at least
// metadata.name + metadata.namespace + spec.
func (m *Mutator) CreateOrReplacePrometheusRule(ctx context.Context, rule any) (any, error) {
	if m.dynamic == nil {
		return nil, errors.New("mutate: dynamic client not configured")
	}
	body, err := json.Marshal(rule)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	u := &unstructured.Unstructured{}
	if err := json.Unmarshal(body, &u.Object); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if u.GetName() == "" || u.GetNamespace() == "" {
		return nil, errors.New("mutate: rule.metadata.name and namespace are required")
	}
	// Force the kind/apiVersion so the apiserver knows what we're creating.
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule",
	})

	ri := m.dynamic.Resource(prometheusRuleGVR).Namespace(u.GetNamespace())
	existing, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		created, err := ri.Create(ctx, u, metav1.CreateOptions{})
		if err != nil {
			return nil, err
		}
		return created.UnstructuredContent(), nil
	}
	if err != nil {
		return nil, err
	}
	// Replace: copy ResourceVersion so the apiserver accepts the update.
	u.SetResourceVersion(existing.GetResourceVersion())
	updated, err := ri.Update(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		return nil, err
	}
	return updated.UnstructuredContent(), nil
}

// DeletePrometheusRule removes one PrometheusRule by namespace+name. Idempotent
// (NotFound is treated as success).
func (m *Mutator) DeletePrometheusRule(ctx context.Context, namespace, name string) error {
	if m.dynamic == nil {
		return errors.New("mutate: dynamic client not configured")
	}
	if namespace == "" || name == "" {
		return errors.New("mutate: namespace and name required")
	}
	err := m.dynamic.Resource(prometheusRuleGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
