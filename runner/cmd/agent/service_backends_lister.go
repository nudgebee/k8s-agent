package main

import (
	"context"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/nudgebee/nudgebee-agent/pkg/triggers"
)

// rolloutGVR is the Argo Rollouts CRD (same GVR pkg/mutate uses). Checked
// in the workload-template sweep so a Rollout scaled to zero doesn't
// false-positive as a selector mismatch.
var rolloutGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}

// serviceBackendsLister wraps a typed clientset to satisfy
// triggers.ServiceBackendsLister. Used by the service_no_endpoints
// matcher to resolve a Service's spec.selector against live pods and
// workload pod templates.
//
// Implementation notes:
//   - ResourceVersion="0" serves every LIST from the apiserver watch
//     cache instead of a quorum read from etcd — same rationale as
//     k8s_events_lister.go. Label selectors aren't indexed in etcd, so
//     the etcd path would scan the whole namespace anyway; the watch
//     cache is eventually-consistent, which is fine for a "does anything
//     match right now" probe (the matcher's 6h rate limit absorbs any
//     staleness-induced double fire).
//   - The workload sweep checks Deployments, StatefulSets, DaemonSets and
//     (when the CRD is installed) Argo Rollouts. Bare ReplicaSets /
//     ReplicationControllers are rare and, when they have replicas > 0,
//     their pods exist — so the pod probe already covers them before the
//     template sweep is consulted.
type serviceBackendsLister struct {
	cs  kubernetes.Interface
	dyn dynamic.Interface // optional; enables the Rollouts template check
}

func newServiceBackendsLister(cs kubernetes.Interface, dyn dynamic.Interface) triggers.ServiceBackendsLister {
	return &serviceBackendsLister{cs: cs, dyn: dyn}
}

func (l *serviceBackendsLister) AnyPodMatching(ctx context.Context, namespace string, selector map[string]string) (bool, error) {
	if l.cs == nil || namespace == "" || len(selector) == 0 {
		return false, nil
	}
	list, err := l.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector:   labels.Set(selector).String(),
		ResourceVersion: "0",
	})
	if err != nil {
		return false, err
	}
	return len(list.Items) > 0, nil
}

func (l *serviceBackendsLister) AnyWorkloadTemplateMatching(ctx context.Context, namespace string, selector map[string]string) (bool, error) {
	if l.cs == nil || namespace == "" || len(selector) == 0 {
		return false, nil
	}
	// A Service selector is a plain equality map: it matches a pod
	// template whose labels carry every selector pair.
	sel := labels.SelectorFromSet(labels.Set(selector))
	opts := metav1.ListOptions{ResourceVersion: "0"}

	deps, err := l.cs.AppsV1().Deployments(namespace).List(ctx, opts)
	if err != nil {
		return false, err
	}
	for i := range deps.Items {
		if sel.Matches(labels.Set(deps.Items[i].Spec.Template.Labels)) {
			return true, nil
		}
	}
	stss, err := l.cs.AppsV1().StatefulSets(namespace).List(ctx, opts)
	if err != nil {
		return false, err
	}
	for i := range stss.Items {
		if sel.Matches(labels.Set(stss.Items[i].Spec.Template.Labels)) {
			return true, nil
		}
	}
	dss, err := l.cs.AppsV1().DaemonSets(namespace).List(ctx, opts)
	if err != nil {
		return false, err
	}
	for i := range dss.Items {
		if sel.Matches(labels.Set(dss.Items[i].Spec.Template.Labels)) {
			return true, nil
		}
	}
	return l.anyRolloutTemplateMatching(ctx, namespace, sel)
}

// anyRolloutTemplateMatching sweeps Argo Rollouts (a common CRD-managed
// workload) so a Rollout scaled to zero registers as a scale state rather
// than a selector mismatch. Rollouts using workloadRef point at a
// Deployment, which the typed sweep above already covers. A cluster
// without the CRD (404) counts as "no rollouts", not an error.
func (l *serviceBackendsLister) anyRolloutTemplateMatching(ctx context.Context, namespace string, sel labels.Selector) (bool, error) {
	if l.dyn == nil {
		return false, nil
	}
	list, err := l.dyn.Resource(rolloutGVR).Namespace(namespace).List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil // CRD not installed
		}
		return false, err
	}
	for i := range list.Items {
		tmplLabels, _, _ := unstructured.NestedStringMap(list.Items[i].Object, "spec", "template", "metadata", "labels")
		if len(tmplLabels) > 0 && sel.Matches(labels.Set(tmplLabels)) {
			return true, nil
		}
	}
	return false, nil
}

func (l *serviceBackendsLister) ListPodLabels(ctx context.Context, namespace string, limit int) ([]triggers.PodLabelSample, error) {
	if l.cs == nil || namespace == "" || limit <= 0 {
		return nil, nil
	}
	list, err := l.cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		ResourceVersion: "0",
	})
	if err != nil {
		return nil, err
	}
	// The apiserver returns list items sorted by metadata.name, so taking
	// the first `limit` is already the stable prefix; the sort below only
	// re-asserts order over those few rows (cheap) in case a cache path
	// ever returns them unsorted.
	n := len(list.Items)
	if n > limit {
		n = limit
	}
	samples := make([]triggers.PodLabelSample, 0, n)
	for i := 0; i < n; i++ {
		samples = append(samples, triggers.PodLabelSample{
			Name:   list.Items[i].Name,
			Labels: list.Items[i].Labels,
		})
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].Name < samples[j].Name })
	return samples, nil
}
