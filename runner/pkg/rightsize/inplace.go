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

// In-place pod resize (KEP-1287). When the cluster is >= 1.35 the rightsizer
// resizes a workload's running pods via the pod `resize` subresource instead of
// patching the controller template (which restarts every pod). It is pod-only:
// the controller template is left untouched, so pods recreated later (scale-up,
// node churn) get the old sizes until the next continuous cycle re-corrects them
// — the same drift VPA's in-place mode accepts. On any failure (cluster too old,
// patch rejected, resize Infeasible, timeout) the caller falls back to the
// existing template Update (rolling restart), so a workload is always sized.

const (
	inPlaceMinMajor = 1
	inPlaceMinMinor = 35
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
// is >= 1.35, where in-place pod resize reached GA / Stable. We require GA
// rather than the 1.33/1.34 beta because beta support was uneven across managed
// providers (e.g. EKS).
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
// Returns true when all pods were resized; false signals the caller to fall
// back to the template Update (rollout) — listing failed, a patch was rejected
// (QoS change, unsupported), a resize was Infeasible, or polling timed out.
//
// All pods are patched first so their kubelets resize in parallel, then we poll
// for completion — total time is ~the poll budget, not budget × replicas.
func (r *Rightsizer) applyInPlace(ctx context.Context, app Application, obj *unstructured.Unstructured, targets []inPlaceTarget) bool {
	if r.client == nil || len(targets) == 0 {
		return false
	}
	labels, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "selector", "matchLabels")
	if len(labels) == 0 {
		return false // can't select pods → fall back
	}
	sel := make([]string, 0, len(labels))
	for k, v := range labels {
		sel = append(sel, k+"="+v)
	}
	sort.Strings(sel) // deterministic selector string
	podList, err := r.client.CoreV1().Pods(app.Namespace).List(ctx, metav1.ListOptions{LabelSelector: strings.Join(sel, ",")})
	if err != nil || len(podList.Items) == 0 {
		return false // listing failed / no pods → fall back rather than abort
	}

	patchBytes, err := buildResizePatch(targets)
	if err != nil {
		return false
	}

	// Trigger the resize on every pod up front (kubelets actuate in parallel).
	pending := make(map[string]struct{}, len(podList.Items))
	for i := range podList.Items {
		name := podList.Items[i].Name
		if _, err := r.client.CoreV1().Pods(app.Namespace).Patch(ctx, name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}, "resize"); err != nil {
			return false // rejected → fall back to rollout
		}
		pending[name] = struct{}{}
	}

	// Poll the pending pods until all complete, any is Infeasible, or timeout.
	timer := time.NewTimer(resizePollInterval)
	defer timer.Stop()
	for attempt := 0; attempt < resizePollAttempts; attempt++ {
		for name := range pending {
			p, err := r.client.CoreV1().Pods(app.Namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				continue
			}
			switch resizeState(p) {
			case "done":
				delete(pending, name)
			case "infeasible":
				return false
			}
		}
		if len(pending) == 0 {
			return true
		}
		timer.Reset(resizePollInterval)
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
		}
	}
	return false // timed out → fall back
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

// resizePolicyList builds the container `resizePolicy` to stamp onto the
// template during a rollout, so pods created afterward carry it and future
// cycles resize them per policy. Returns nil when injection is disabled.
//
//	"" / default            -> cpu NotRequired, memory NotRequired (in-place both)
//	"restart-memory"        -> cpu NotRequired, memory RestartContainer
//	"disabled"/off/no/false -> nil (don't stamp)
func resizePolicyList(mode string) []any {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "disabled", "off", "no", "false":
		return nil
	}
	memPolicy := "NotRequired"
	if strings.EqualFold(strings.TrimSpace(mode), "restart-memory") {
		memPolicy = "RestartContainer"
	}
	return []any{
		map[string]any{"resourceName": "cpu", "restartPolicy": "NotRequired"},
		map[string]any{"resourceName": "memory", "restartPolicy": memPolicy},
	}
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
