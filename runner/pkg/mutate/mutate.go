// Package mutate implements the mutating Group-D actions: delete_pod,
// delete_job, cordon, uncordon, rollout_restart, and AlertManager silence
// CRUD. Each one mutates customer cluster state; all of them MUST be served
// behind RSA partial-keys auth in production (NOT light-action).
//
// Out-of-scope for v1 — bring in follow-up commits as they're needed:
//   - drain                 : evict-pods + wait, ~150 LoC orchestration
//   - replace_workload, create_workload   : dynamic client + manifest validation
//   - create_pvc_snapshot, rightsize_pvc  : PVC-resize subresource
//   - PrometheusRule/LokiRule CRUD        : CRD manipulation, prometheus-operator
package mutate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// Mutator wraps the typed client + an optional AlertManager URL for silences.
type Mutator struct {
	Client              kubernetes.Interface
	AlertManagerURL     string
	AlertManagerHeaders map[string]string

	// LokiRulesURL: HTTP base URL for Loki rules CRUD (Loki's own API,
	// separate from the LogQL query URL — usually Loki ruler component).
	// Empty disables loki_*_alert_rule actions.
	LokiRulesURL     string
	LokiRulesHeaders map[string]string

	// Namespace is the agent's install namespace. Required by the legacy
	// alert-rule path (CreateOrReplaceAlertRule / DeleteAlertRule), which
	// locates the canonical PrometheusRule CR there.
	Namespace string

	// dynamic is the dynamic client used for CRD operations like
	// PrometheusRule. Set via SetDynamic.
	dynamic dynamic.Interface
}

func New(cs kubernetes.Interface, alertManagerURL string, headers map[string]string) *Mutator {
	return &Mutator{
		Client:              cs,
		AlertManagerURL:     alertManagerURL,
		AlertManagerHeaders: headers,
	}
}

// SetLokiRules wires the Loki rules HTTP endpoint. Optional.
func (m *Mutator) SetLokiRules(url string, headers map[string]string) {
	m.LokiRulesURL = url
	m.LokiRulesHeaders = headers
}

// SetNamespace records the agent's install namespace. Required by the legacy
// alert-rule path; everything else is independent of it.
func (m *Mutator) SetNamespace(ns string) { m.Namespace = ns }

// DeletePod removes one pod. namespace + name required.
func (m *Mutator) DeletePod(ctx context.Context, namespace, name string, gracePeriodSec *int64) error {
	if m.Client == nil {
		return errors.New("mutate: client not configured")
	}
	if namespace == "" || name == "" {
		return errors.New("mutate: namespace and name required")
	}
	opts := metav1.DeleteOptions{}
	if gracePeriodSec != nil {
		opts.GracePeriodSeconds = gracePeriodSec
	}
	return m.Client.CoreV1().Pods(namespace).Delete(ctx, name, opts)
}

// DeleteJob removes one Job (and its pods, propagation=Background by default).
func (m *Mutator) DeleteJob(ctx context.Context, namespace, name string) error {
	if m.Client == nil {
		return errors.New("mutate: client not configured")
	}
	if namespace == "" || name == "" {
		return errors.New("mutate: namespace and name required")
	}
	prop := metav1.DeletePropagationBackground
	return m.Client.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &prop,
	})
}

// Cordon marks the node unschedulable. Existing pods stay (use drain to evict).
func (m *Mutator) Cordon(ctx context.Context, nodeName string) error {
	return m.setUnschedulable(ctx, nodeName, true)
}

// Uncordon clears the unschedulable flag.
func (m *Mutator) Uncordon(ctx context.Context, nodeName string) error {
	return m.setUnschedulable(ctx, nodeName, false)
}

func (m *Mutator) setUnschedulable(ctx context.Context, nodeName string, unschedulable bool) error {
	if m.Client == nil {
		return errors.New("mutate: client not configured")
	}
	if nodeName == "" {
		return errors.New("mutate: node name required")
	}
	patch := fmt.Sprintf(`{"spec":{"unschedulable":%t}}`, unschedulable)
	_, err := m.Client.CoreV1().Nodes().Patch(ctx, nodeName, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
	return err
}

// RolloutRestart triggers a rolling restart by patching the pod-template
// `kubectl.kubernetes.io/restartedAt` annotation. Same mechanism kubectl uses.
// kind is one of: deployment, statefulset, daemonset.
func (m *Mutator) RolloutRestart(ctx context.Context, kind, namespace, name string) error {
	if m.Client == nil {
		return errors.New("mutate: client not configured")
	}
	if kind == "" || namespace == "" || name == "" {
		return errors.New("mutate: kind, namespace, name required")
	}
	patch := fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"kubectl.kubernetes.io/restartedAt":%q}}}}}`,
		time.Now().UTC().Format(time.RFC3339),
	)
	switch strings.ToLower(kind) {
	case "deployment":
		_, err := m.Client.AppsV1().Deployments(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
		return err
	case "statefulset":
		_, err := m.Client.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
		return err
	case "daemonset":
		_, err := m.Client.AppsV1().DaemonSets(namespace).Patch(ctx, name, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{})
		return err
	default:
		return fmt.Errorf("mutate: unsupported kind %q (deployment|statefulset|daemonset)", kind)
	}
}

// EvictPod uses the Eviction API which respects PodDisruptionBudgets.
// Used by Drain (not yet implemented) and as a building block for other workflows.
func (m *Mutator) EvictPod(ctx context.Context, namespace, name string) error {
	if m.Client == nil {
		return errors.New("mutate: client not configured")
	}
	eviction := &policyEviction{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	return m.Client.CoreV1().Pods(namespace).EvictV1(ctx, asPolicyEviction(eviction))
}

// Helper types let us avoid importing policy/v1 at file top level (keeps
// the import set minimal in the common case).
type policyEviction struct {
	metav1.ObjectMeta
}

// PodReadyDeadline polls the pod's Ready condition until ready or deadline.
// Used internally by RolloutRestart smoke tests; exposed for callers that
// want to wait after a mutation.
func (m *Mutator) PodReadyDeadline(ctx context.Context, namespace, name string, deadline time.Time) error {
	for time.Now().Before(deadline) {
		pod, err := m.Client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			for _, c := range pod.Status.Conditions {
				if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("pod not ready before deadline")
}
