package mutate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DrainOptions controls Drain behavior. Mirrors the relevant kubectl drain
// flags. Defaults match kubectl's defaults.
type DrainOptions struct {
	GracePeriodSeconds *int64        // pod TerminationGracePeriodSeconds override
	Timeout            time.Duration // total budget; default 5m
	IgnoreDaemonSets   bool          // default true (kubectl's default)
	DeleteEmptyDirData bool          // default false; if false and pod uses emptyDir, drain refuses
	Force              bool          // delete pods without controllers (StaticPods, bare pods)
	DisableEviction    bool          // skip the Eviction API and DELETE pods directly (rare)
}

// DrainResult is the per-pod outcome.
type DrainResult struct {
	Node    string           `json:"node"`
	Pods    []DrainPodResult `json:"pods"`
	Skipped []DrainPodResult `json:"skipped"`
}

type DrainPodResult struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Status    string `json:"status"` // "evicted" | "skipped" | "failed"
	Error     string `json:"error,omitempty"`
}

// Drain cordons the node and then evicts every eligible pod, waiting for
// termination. Mirrors `kubectl drain` semantics.
//
// Flow:
//  1. Cordon node (skip if already unschedulable)
//  2. List pods on node
//  3. Filter: ignore DaemonSet pods (when IgnoreDaemonSets), refuse if
//     emptyDir-using pods present and !DeleteEmptyDirData, etc.
//  4. Evict each in parallel via the Eviction API (respects PDBs)
//  5. Wait for each evicted pod to disappear, up to Timeout
func (m *Mutator) Drain(ctx context.Context, nodeName string, opts DrainOptions) (*DrainResult, error) {
	if m.Client == nil {
		return nil, errors.New("mutate: client not configured")
	}
	if nodeName == "" {
		return nil, errors.New("mutate: node name required")
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}

	if err := m.Cordon(ctx, nodeName); err != nil {
		return nil, fmt.Errorf("cordon: %w", err)
	}

	pods, err := m.Client.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods on %s: %w", nodeName, err)
	}

	res := &DrainResult{Node: nodeName}
	type job struct {
		pod  corev1.Pod
		skip string // non-empty = skipped
	}
	jobs := make([]job, 0, len(pods.Items))
	for _, p := range pods.Items {
		if reason := skipReason(&p, opts); reason != "" {
			jobs = append(jobs, job{pod: p, skip: reason})
			continue
		}
		jobs = append(jobs, job{pod: p})
	}

	drainCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, j := range jobs {
		if j.skip != "" {
			mu.Lock()
			res.Skipped = append(res.Skipped, DrainPodResult{
				Name: j.pod.Name, Namespace: j.pod.Namespace, Status: "skipped", Error: j.skip,
			})
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(p corev1.Pod) {
			defer wg.Done()
			outcome := m.evictAndWait(drainCtx, p, opts)
			mu.Lock()
			res.Pods = append(res.Pods, outcome)
			mu.Unlock()
		}(j.pod)
	}
	wg.Wait()
	return res, nil
}

// skipReason returns a non-empty string if drain MUST skip this pod, given
// opts. Empty string means "evict it".
func skipReason(p *corev1.Pod, opts DrainOptions) string {
	if isMirrorPod(p) {
		return "mirror pod"
	}
	if hasOwner(p, "DaemonSet") {
		if opts.IgnoreDaemonSets {
			return "daemonset pod"
		}
		return "" // not skipped, will be evicted (and almost certainly fail PDB)
	}
	if !opts.Force && !hasController(p) {
		return "unmanaged pod (set Force=true to delete)"
	}
	if !opts.DeleteEmptyDirData {
		for _, v := range p.Spec.Volumes {
			if v.EmptyDir != nil {
				return "uses emptyDir (set DeleteEmptyDirData=true to evict)"
			}
		}
	}
	return ""
}

func isMirrorPod(p *corev1.Pod) bool {
	_, ok := p.Annotations["kubernetes.io/config.mirror"]
	return ok
}

func hasOwner(p *corev1.Pod, kind string) bool {
	for _, o := range p.OwnerReferences {
		if strings.EqualFold(o.Kind, kind) {
			return true
		}
	}
	return false
}

func hasController(p *corev1.Pod) bool {
	for _, o := range p.OwnerReferences {
		if o.Controller != nil && *o.Controller {
			return true
		}
	}
	return false
}

// evictAndWait evicts a single pod and polls until it's gone or the context
// is cancelled.
func (m *Mutator) evictAndWait(ctx context.Context, pod corev1.Pod, opts DrainOptions) DrainPodResult {
	out := DrainPodResult{Name: pod.Name, Namespace: pod.Namespace, Status: "evicted"}

	if opts.DisableEviction {
		err := m.Client.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
			GracePeriodSeconds: opts.GracePeriodSeconds,
		})
		if err != nil && !apierrors.IsNotFound(err) {
			out.Status = "failed"
			out.Error = err.Error()
			return out
		}
	} else {
		err := m.EvictPod(ctx, pod.Namespace, pod.Name)
		if err != nil && !apierrors.IsNotFound(err) {
			out.Status = "failed"
			out.Error = err.Error()
			return out
		}
	}

	// Poll until the pod is gone (or recreated with a different UID).
	originalUID := pod.UID
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			out.Status = "failed"
			out.Error = "timed out waiting for pod removal"
			return out
		case <-ticker.C:
			cur, err := m.Client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) || (err == nil && cur.UID != originalUID) {
				return out
			}
		}
	}
}
