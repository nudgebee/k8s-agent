package rightsize

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// In-place pod resize (KEP-1287). When the cluster is >= 1.33 the rightsizer
// resizes a workload's running pods via the pod `resize` subresource instead of
// patching the controller template (which restarts every pod). It is pod-only:
// the controller template is left untouched, so pods recreated later (scale-up,
// node churn) get the old sizes until the next continuous cycle re-corrects them
// — the same drift VPA's in-place mode accepts. On any failure (cluster too old,
// patch rejected, resize Infeasible, timeout) the caller falls back to the
// existing template Update (rolling restart), so a workload is always sized.

const (
	inPlaceMinMajor = 1
	inPlaceMinMinor = 33
	// resize status poll budget.
	resizePollInterval = 3 * time.Second
	resizePollAttempts = 20 // ~60s
)

// inPlaceTarget is the new resources for one container that actually changed.
type inPlaceTarget struct {
	name   string
	reqCPU string
	reqMem string
	limCPU string
	limMem string
}

var k8sVersionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)`)

// k8sAtLeastInPlace reports whether a cluster gitVersion (e.g. "v1.35.3-gke.x")
// is >= 1.33, the point where in-place pod resize is on by default.
func k8sAtLeastInPlace(gitVersion string) bool {
	m := k8sVersionRe.FindStringSubmatch(strings.TrimSpace(gitVersion))
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major > inPlaceMinMajor || (major == inPlaceMinMajor && minor >= inPlaceMinMinor)
}

// supportsInPlaceResize checks the cluster server version via discovery. Fails
// closed (false) when the typed client is absent or discovery errors.
func (r *Rightsizer) supportsInPlaceResize() bool {
	if r.client == nil {
		return false
	}
	v, err := r.client.Discovery().ServerVersion()
	if err != nil || v == nil {
		return false
	}
	return k8sAtLeastInPlace(v.GitVersion)
}

// applyInPlace resizes every pod of the workload via the resize subresource.
// Returns (true, nil) when all pods were resized; (false, nil) signals the
// caller to fall back to the template Update (rollout). A non-nil error is a
// hard failure that aborts the run.
func (r *Rightsizer) applyInPlace(ctx context.Context, app Application, obj *unstructured.Unstructured, targets []inPlaceTarget, identifier string, changes []map[string]any) (bool, error) {
	if r.client == nil || len(targets) == 0 {
		return false, nil
	}
	labels, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "selector", "matchLabels")
	if len(labels) == 0 {
		return false, nil // can't select pods → fall back
	}
	sel := make([]string, 0, len(labels))
	for k, v := range labels {
		sel = append(sel, k+"="+v)
	}
	sort.Strings(sel) // deterministic selector string
	podList, err := r.client.CoreV1().Pods(app.Namespace).List(ctx, metav1.ListOptions{LabelSelector: strings.Join(sel, ",")})
	if err != nil {
		return false, nil // listing failed → fall back rather than abort
	}
	if len(podList.Items) == 0 {
		return false, nil
	}

	patchBytes, err := buildResizePatch(targets)
	if err != nil {
		return false, nil
	}
	annotationPatch := buildPodAnnotationPatch(identifier, changes)

	for i := range podList.Items {
		pod := podList.Items[i]
		_, err := r.client.CoreV1().Pods(app.Namespace).Patch(ctx, pod.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}, "resize")
		if err != nil {
			// Rejected (QoS change, unsupported, etc.) → fall back to rollout.
			return false, nil
		}
		if annotationPatch != nil {
			// Best-effort traceability; the resize subresource can't carry metadata.
			_, _ = r.client.CoreV1().Pods(app.Namespace).Patch(ctx, pod.Name, types.StrategicMergePatchType, annotationPatch, metav1.PatchOptions{})
		}
		if !r.waitForResize(ctx, app.Namespace, pod.Name) {
			return false, nil // Infeasible / timed out → fall back.
		}
	}
	return true, nil
}

// waitForResize polls the pod's KEP-1287 status conditions. Returns true when
// the resize completes; false on Infeasible/error or when the budget is spent.
func (r *Rightsizer) waitForResize(ctx context.Context, namespace, pod string) bool {
	for attempt := 0; attempt < resizePollAttempts; attempt++ {
		p, err := r.client.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
		if err == nil {
			switch resizeState(p) {
			case "done":
				return true
			case "infeasible":
				return false
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(resizePollInterval):
		}
	}
	return false
}

// resizeState classifies a pod's resize status from its conditions. With neither
// PodResizePending nor PodResizeInProgress present, the resize is complete.
func resizeState(p *corev1.Pod) string {
	for _, c := range p.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch string(c.Type) {
		case "PodResizePending":
			if strings.EqualFold(c.Reason, "Infeasible") {
				return "infeasible"
			}
			return "pending"
		case "PodResizeInProgress":
			if strings.EqualFold(c.Reason, "Error") {
				return "infeasible"
			}
			return "inprogress"
		}
	}
	return "done"
}

// buildResizePatch builds the strategic-merge body for the resize subresource:
// {"spec":{"containers":[{"name","resources":{requests,limits}}]}}. Only
// non-empty values are written (in-place sets, never removes).
func buildResizePatch(targets []inPlaceTarget) ([]byte, error) {
	containers := make([]map[string]any, 0, len(targets))
	for _, t := range targets {
		requests := map[string]any{}
		if t.reqCPU != "" {
			requests["cpu"] = t.reqCPU
		}
		if t.reqMem != "" {
			requests["memory"] = t.reqMem
		}
		limits := map[string]any{}
		if t.limCPU != "" {
			limits["cpu"] = t.limCPU
		}
		if t.limMem != "" {
			limits["memory"] = t.limMem
		}
		resources := map[string]any{}
		if len(requests) > 0 {
			resources["requests"] = requests
		}
		if len(limits) > 0 {
			resources["limits"] = limits
		}
		if len(resources) == 0 {
			continue
		}
		containers = append(containers, map[string]any{"name": t.name, "resources": resources})
	}
	if len(containers) == 0 {
		return nil, fmt.Errorf("rightsize: no container resources to resize in place")
	}
	return json.Marshal(map[string]any{"spec": map[string]any{"containers": containers}})
}

// buildPodAnnotationPatch builds a metadata-annotation merge patch carrying the
// same vertical-scaler payload writeAnnotation stamps on the controller. Returns
// nil if the payload can't be built (annotation is best-effort).
func buildPodAnnotationPatch(identifier string, changes []map[string]any) []byte {
	value, err := annotationValue(identifier, changes)
	if err != nil {
		return nil
	}
	b, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{annotationKey: value}},
	})
	if err != nil {
		return nil
	}
	return b
}
