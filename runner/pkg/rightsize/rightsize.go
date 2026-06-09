// Package rightsize implements the `continuous_rightsizing` action — a port of
// the legacy Python agent's rightsizing module (handled inline in the old
// receiver). For each requested workload it samples CPU/memory usage from
// Prometheus, computes a recommendation per the configured percentile +
// OOM-kill + floor + change-threshold rules, and — unless recommend_only —
// patches the workload's container resource requests/limits in place.
//
// Two deliberate deviations from the original, both documented at the call
// site: (1) default_min_memory arrives from the backend in MiB and is converted
// to bytes here so the memory floor actually applies (the Python max() compared
// bytes against a MiB-magnitude number, silently no-op-ing the floor); (2) the
// memory percentile window uses the full analysis duration instead of a
// hard-coded 1h, matching the user-facing "Analysis Duration" field.
package rightsize

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

// Defaults mirror schedule/rightsizing.py module constants.
const (
	defaultMinCPU             = 0.01
	defaultMinMemoryBytes     = 5 * 10 * 1024 * 1024 // 52,428,800 (~50Mi)
	defaultOOMKillFactor      = 1.4
	defaultChangeThreshold    = 15.0
	defaultMaxChangeThreshold = 100.0
	defaultPercentile         = 99.99
	defaultAnalysisDuration   = 24
	memoryLimitHeadroom       = 1.4 // recommended limit = request * 1.4
)

// annotationKey is the workload annotation the backend reads to correlate an
// applied change with its originating run. Matches the legacy
// VerticalRightsizeAnnotation.complete_key:
// "<base>/V1.continuous-rightsize.vertical-scaler".
const annotationKey = "workloads.nudgebee.com/V1.continuous-rightsize.vertical-scaler"

// Settings is the resolved per-run configuration. All memory values are bytes.
type Settings struct {
	MinCPU             float64
	MinMemoryBytes     float64
	OOMKillFactor      float64
	ChangeThreshold    float64
	MaxChangeThreshold float64
	CPUPercentile      float64
	MemoryPercentile   float64
	AnalysisDuration   int
	RecommendOnly      bool
	// InPlace requests a zero-downtime in-place pod resize when the cluster
	// supports it (>= 1.33); falls back to the template rollout otherwise.
	// Defaults true (the backend may set in_place=false to force a rollout).
	InPlace bool
	// ResizePolicy controls the container resizePolicy stamped onto the template
	// when a rollout occurs (cluster >= 1.33): "" (cpu/memory NotRequired),
	// "restart-memory" (memory RestartContainer), or "disabled" (don't stamp).
	ResizePolicy string
	Identifier   string
}

// Application identifies one workload to rightsize.
type Application struct {
	Name      string
	Namespace string
	Kind      string
}

// supportedKinds maps the user-facing kind to its GVR. All are namespace-scoped
// and expose containers at spec.template.spec.containers.
var supportedKinds = map[string]schema.GroupVersionResource{
	"Deployment":  {Group: "apps", Version: "v1", Resource: "deployments"},
	"StatefulSet": {Group: "apps", Version: "v1", Resource: "statefulsets"},
	"DaemonSet":   {Group: "apps", Version: "v1", Resource: "daemonsets"},
	"ReplicaSet":  {Group: "apps", Version: "v1", Resource: "replicasets"},
	"Rollout":     {Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"},
}

// Rightsizer holds the clients the action needs: Prometheus for usage sampling
// and a dynamic client for reading + patching workloads of any supported kind.
type Rightsizer struct {
	prom *prometheus.Client
	dyn  dynamic.Interface
	// client is the typed clientset used for in-place pod resize (pod listing,
	// the resize subresource, and server-version discovery). May be nil — then
	// in-place is skipped and apply always uses the template Update (rollout).
	client kubernetes.Interface
}

// New builds a Rightsizer. prom + dyn are required; client enables in-place pod
// resize (it provides pods + discovery) and may be nil to force the rollout path.
func New(prom *prometheus.Client, dyn dynamic.Interface, client kubernetes.Interface) *Rightsizer {
	return &Rightsizer{prom: prom, dyn: dyn, client: client}
}

// Run rightsizes each application and returns one result object per workload,
// shaped to match the legacy ApplicationResource (name, namespace, kind,
// containers[]). Only hard failures (workload not found, update rejected,
// malformed spec) abort the run; an out-of-bounds recommendation is skipped
// per-resource and surfaced in the container's "skipped" list, not fatal.
func (r *Rightsizer) Run(ctx context.Context, s Settings, apps []Application) ([]map[string]any, error) {
	// Decide in-place eligibility once per run (one discovery call): apply
	// resizes to running pods without a restart when requested and the cluster
	// is >= 1.33; otherwise each workload uses the template Update (rollout).
	inPlaceEligible := s.InPlace && r.supportsInPlaceResize()
	out := make([]map[string]any, 0, len(apps))
	for _, app := range apps {
		res, err := r.rightsizeWorkload(ctx, s, app, inPlaceEligible)
		if err != nil {
			return nil, err
		}
		if res != nil {
			out = append(out, res)
		}
	}
	return out, nil
}

type containerStat struct {
	cpuP99    float64 // cores
	memoryMax float64 // bytes
	oomKill   float64 // bytes (memory limit observed at OOMKill time, 0 if none)
	hasData   bool
}

func (r *Rightsizer) rightsizeWorkload(ctx context.Context, s Settings, app Application, inPlaceEligible bool) (map[string]any, error) {
	gvr, ok := supportedKinds[app.Kind]
	if !ok {
		return nil, fmt.Errorf("rightsize: unsupported kind %q (supported: Deployment, StatefulSet, DaemonSet, ReplicaSet, Rollout)", app.Kind)
	}

	ri := r.dyn.Resource(gvr).Namespace(app.Namespace)
	obj, err := ri.Get(ctx, app.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("rightsize: get %s %s/%s: %w", app.Kind, app.Namespace, app.Name, err)
	}

	containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil || len(containers) == 0 {
		return nil, fmt.Errorf("rightsize: %s %s/%s has no spec.template.spec.containers", app.Kind, app.Namespace, app.Name)
	}

	performUpdate := false
	resultContainers := make([]map[string]any, 0, len(containers))
	annotationChanges := make([]map[string]any, 0, len(containers))
	// Targets for an in-place resize: only the containers that actually changed,
	// with the values that would be applied. Parallels the template mutation.
	inPlaceTargets := make([]inPlaceTarget, 0, len(containers))

	for i := range containers {
		c, _ := containers[i].(map[string]any)
		if c == nil {
			continue
		}
		name, _ := c["name"].(string)

		reqCPU, reqMem, limCPU, limMem := containerResourceStrings(c)
		currentCPU := parseCPUCores(reqCPU)
		currentMem := parseMemBytes(reqMem)

		stat, err := r.getResourceUsage(ctx, s, app, name)
		if err != nil {
			// Operational failure (e.g. Prometheus unreachable) — abort the
			// workload rather than silently skip and report success.
			return nil, fmt.Errorf("rightsize: %s %s/%s container %q: %w", app.Kind, app.Namespace, app.Name, name, err)
		}
		if !stat.hasData {
			// No usage series at all for this container — nothing to base a
			// recommendation on. Skip it (legacy: get_resource_usage → None).
			continue
		}

		recMem := getRecommendedMemory(s, stat.memoryMax, stat.oomKill)
		recMemLimit := recMem * memoryLimitHeadroom
		if currentMem > recMemLimit {
			recMemLimit = currentMem
		}
		recCPU := getRecommendedCPU(s, stat.cpuP99)

		fmtRecMem := formatAndRoundUnit(recMem)
		fmtRecCPU := formatAndRoundUnit(recCPU)
		fmtRecMemLimit := formatAndRoundUnit(recMemLimit)

		// Compare the *formatted* recommendation against the current request so
		// the change-% reflects what would actually be applied (rounding folded
		// in), matching the original.
		recCPUcores := parseCPUCores(fmtRecCPU)
		recMemBytes := parseMemBytes(fmtRecMem)

		cpuChangePct := pctChange(recCPUcores, currentCPU)
		memChangePct := pctChange(recMemBytes, currentMem)

		// Decide each resource independently. A change within the threshold is
		// left alone; a change at/above the max-change guard is *skipped* (not
		// applied) and recorded — one oversized resource no longer aborts the
		// whole run, and CPU can still be applied while memory is skipped (or
		// vice-versa).
		changed := false
		var skipped []string
		newReqCPU, newReqMem := reqCPU, reqMem
		newLimCPU, newLimMem := limCPU, limMem

		switch {
		case memChangePct <= s.ChangeThreshold:
			// within tolerance — leave memory as-is
		case memChangePct >= s.MaxChangeThreshold:
			skipped = append(skipped, fmt.Sprintf("memory change %.1f%% >= max %.0f%% (current %s → recommended %s)",
				memChangePct, s.MaxChangeThreshold, orZero(reqMem), fmtRecMem))
		default:
			newReqMem = fmtRecMem
			newLimMem = fmtRecMemLimit
			changed = true
		}

		switch {
		case cpuChangePct <= s.ChangeThreshold:
			// within tolerance — leave CPU as-is
		case cpuChangePct >= s.MaxChangeThreshold:
			skipped = append(skipped, fmt.Sprintf("cpu change %.1f%% >= max %.0f%% (current %s → recommended %s)",
				cpuChangePct, s.MaxChangeThreshold, orZero(reqCPU), fmtRecCPU))
		default:
			newReqCPU = fmtRecCPU
			newLimCPU = "" // drop CPU limit — recommendation only sets the request
			changed = true
		}

		if changed {
			performUpdate = true
			setContainerResources(c, newReqCPU, newReqMem, newLimCPU, newLimMem)
			// On a >=1.33 cluster, stamp resizePolicy onto containers that lack
			// one so a rollout (this fallback or a later cycle) leaves the new
			// pods in-place-resizable per policy. Only persisted if we end up on
			// the template-Update path below; never overwrites an existing
			// (operator/GitOps-set) resizePolicy.
			if inPlaceEligible {
				if _, has := c["resizePolicy"]; !has {
					if rp := resizePolicyList(s.ResizePolicy); rp != nil {
						c["resizePolicy"] = rp
					}
				}
			}
			containers[i] = c
			inPlaceTargets = append(inPlaceTargets, inPlaceTarget{
				name: name, reqCPU: newReqCPU, reqMem: newReqMem, limCPU: newLimCPU, limMem: newLimMem,
			})
		}

		result := map[string]any{
			"name": name,
			"resources": map[string]any{
				"requests": map[string]any{"cpu": reqCPU, "memory": reqMem},
				"limits":   map[string]any{"cpu": limCPU, "memory": limMem},
			},
			// recommended_resources always reflects the computed recommendation,
			// whether or not it was applied, so recommend_only runs and skipped
			// resources still surface the target sizing.
			"recommended_resources": map[string]any{
				"requests": map[string]any{"cpu": fmtRecCPU, "memory": fmtRecMem},
				"limits":   map[string]any{"cpu": "", "memory": fmtRecMemLimit},
			},
			"changed": changed,
		}
		if len(skipped) > 0 {
			result["skipped"] = skipped
		}
		resultContainers = append(resultContainers, result)

		annotationChanges = append(annotationChanges, map[string]any{
			"container_name":      name,
			"cpu_request":         fmtRecCPU,
			"cpu_limit":           nil,
			"memory_request":      fmtRecMem,
			"memory_limit":        fmtRecMemLimit,
			"prev_cpu_request":    nullable(reqCPU),
			"prev_cpu_limit":      nullable(limCPU),
			"prev_memory_request": nullable(reqMem),
			"prev_memory_limit":   nullable(limMem),
		})
	}

	if performUpdate && !s.RecommendOnly {
		appliedInPlace := false
		if inPlaceEligible {
			// Resize the running pods without a restart. On any failure
			// (rejected, Infeasible, timeout) appliedInPlace stays false and we
			// fall through to the template Update (rolling restart) below.
			ok, err := r.applyInPlace(ctx, app, obj, inPlaceTargets, s.Identifier, annotationChanges)
			if err != nil {
				return nil, fmt.Errorf("rightsize: in-place resize %s %s/%s: %w", app.Kind, app.Namespace, app.Name, err)
			}
			appliedInPlace = ok
		}
		if !appliedInPlace {
			if err := unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", "containers"); err != nil {
				return nil, fmt.Errorf("rightsize: rebuild containers for %s %s/%s: %w", app.Kind, app.Namespace, app.Name, err)
			}
			writeAnnotation(obj, s.Identifier, annotationChanges)
			if _, err := ri.Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
				return nil, fmt.Errorf("rightsize: apply %s %s/%s: %w", app.Kind, app.Namespace, app.Name, err)
			}
		}
	}

	return map[string]any{
		"name":       app.Name,
		"namespace":  app.Namespace,
		"kind":       app.Kind,
		"containers": resultContainers,
	}, nil
}

// getResourceUsage samples cpu_p99, memory_max and oom_kill_limit for one
// container. hasData is false only when neither the CPU nor the memory query
// returned any series (so a genuinely-idle container with 0 CPU still counts).
// CPU/memory query errors are fatal (returned); the OOM query is best-effort
// (almost always empty) and its error is ignored.
func (r *Rightsizer) getResourceUsage(ctx context.Context, s Settings, app Application, container string) (containerStat, error) {
	cpu, cpuOK, err := r.queryMax(ctx, cpuUsageQuery(s, app, container))
	if err != nil {
		return containerStat{}, fmt.Errorf("cpu query: %w", err)
	}
	mem, memOK, err := r.queryMax(ctx, memoryUsageQuery(s, app, container))
	if err != nil {
		return containerStat{}, fmt.Errorf("memory query: %w", err)
	}
	oom, _, _ := r.queryMax(ctx, oomKillQuery(s, app, container))
	return containerStat{cpuP99: cpu, memoryMax: mem, oomKill: oom, hasData: cpuOK || memOK}, nil
}

func getRecommendedCPU(s Settings, cpu float64) float64 {
	cpu = math.Ceil(cpu*1000) / 1000.0 // round up to the nearest millicore
	return math.Max(cpu, s.MinCPU)
}

func getRecommendedMemory(s Settings, memory, oomKillLimit float64) float64 {
	rec := memory
	if oomKillLimit > 0 {
		rec = math.Max(rec, oomKillLimit*s.OOMKillFactor)
	}
	// The floor always applies — even after an OOM bump — so a kill at a tiny
	// limit can't drive the recommendation below the configured minimum.
	// (the legacy code skipped the floor on the OOM path; applying it here is safer.)
	return math.Max(rec, s.MinMemoryBytes)
}

// pctChange returns the absolute percent change of rec vs current. A current of
// 0 (no existing request) yields 100, matching the original — which means such
// a container hits the max-change guard and aborts the run rather than being
// silently sized from nothing.
func pctChange(rec, current float64) float64 {
	if current == 0 {
		return 100
	}
	return math.Abs(rec-current) / current * 100
}

// --- Prometheus queries -----------------------------------------------------
//
// The cluster label (__CLUSTER__ in the legacy queries) is omitted: the agent queries its
// own in-cluster Prometheus directly, so there is no cross-cluster selector to
// inject (the relay substitutes __CLUSTER__ only on the proxied query path).

func cpuUsageQuery(s Settings, app Application, container string) string {
	dur := fmt.Sprintf("%dh", s.AnalysisDuration)
	return fmt.Sprintf(
		`quantile_over_time(%s, max(rate(container_cpu_usage_seconds_total{namespace="%s",pod=~"%s-.*",container="%s"}[1m])) by (container, pod, job, namespace)[%s:1m])`,
		quantileFraction(s.CPUPercentile), app.Namespace, app.Name, container, dur)
}

func memoryUsageQuery(s Settings, app Application, container string) string {
	dur := fmt.Sprintf("%dh", s.AnalysisDuration)
	return fmt.Sprintf(
		`quantile_over_time(%s, max(container_memory_usage_bytes{namespace="%s",pod=~"%s-.*",container="%s"}) by (container, pod, job, namespace)[%s:1m])`,
		quantileFraction(s.MemoryPercentile), app.Namespace, app.Name, container, dur)
}

func oomKillQuery(s Settings, app Application, container string) string {
	dur := fmt.Sprintf("%dh", s.AnalysisDuration)
	return fmt.Sprintf(
		`max_over_time((max(kube_pod_container_resource_limits{resource="memory",namespace="%s",pod=~"%s-.*",container="%s"}) by (pod, container, namespace, job) `+
			`* on(pod, container, namespace, job) group_left(reason) `+
			`max(kube_pod_container_status_last_terminated_reason{reason="OOMKilled",namespace="%s",pod=~"%s-.*",container="%s"}) by (pod, container, job, namespace, reason))[%s:1m])`,
		app.Namespace, app.Name, container, app.Namespace, app.Name, container, dur)
}

// quantileFraction converts a percentile (0-100) to the fraction PromQL's
// quantile_over_time expects, rounded to 2 decimals exactly like the original
// (round(p/100, 2) — note 99.99 collapses to 1.0, i.e. the peak).
func quantileFraction(percentile float64) string {
	f := math.Round(percentile) / 100
	if f > 1 {
		f = 1
	}
	if f < 0 {
		f = 0
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// queryMax runs an instant query and returns the max sample value across all
// returned series, plus whether any series was returned. An operational error
// (Prometheus down, HTTP error, bad JSON, status != success) is returned so the
// caller can fail the action loudly instead of mistaking it for "no usage data"
// — which on a mutating action would silently skip every container and report
// success. An empty-but-successful result is NOT an error (hasData=false).
func (r *Rightsizer) queryMax(ctx context.Context, promQL string) (float64, bool, error) {
	raw, err := r.prom.Query(ctx, promQL, "", "30s")
	if err != nil {
		return 0, false, err
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, false, fmt.Errorf("decode prometheus response: %w", err)
	}
	if resp.Status != "success" {
		return 0, false, fmt.Errorf("prometheus query status %q", resp.Status)
	}
	max := math.NaN()
	for _, series := range resp.Data.Result {
		if len(series.Value) < 2 {
			continue
		}
		v, ok := toFloat(series.Value[1])
		if !ok || math.IsNaN(v) {
			continue
		}
		if math.IsNaN(max) || v > max {
			max = v
		}
	}
	if math.IsNaN(max) {
		return 0, len(resp.Data.Result) > 0, nil
	}
	return max, true, nil
}

// --- workload (unstructured) helpers ---------------------------------------

// containerResourceStrings extracts the request/limit cpu+memory strings from
// an unstructured container map. Missing values come back as "".
func containerResourceStrings(c map[string]any) (reqCPU, reqMem, limCPU, limMem string) {
	res, _ := c["resources"].(map[string]any)
	if res == nil {
		return "", "", "", ""
	}
	if req, _ := res["requests"].(map[string]any); req != nil {
		reqCPU = asString(req["cpu"])
		reqMem = asString(req["memory"])
	}
	if lim, _ := res["limits"].(map[string]any); lim != nil {
		limCPU = asString(lim["cpu"])
		limMem = asString(lim["memory"])
	}
	return reqCPU, reqMem, limCPU, limMem
}

// setContainerResources writes the new request/limit values back into the
// unstructured container map, creating the resources/requests/limits maps as
// needed. An empty value clears that key.
func setContainerResources(c map[string]any, reqCPU, reqMem, limCPU, limMem string) {
	res, _ := c["resources"].(map[string]any)
	if res == nil {
		res = map[string]any{}
	}
	req, _ := res["requests"].(map[string]any)
	if req == nil {
		req = map[string]any{}
	}
	lim, _ := res["limits"].(map[string]any)
	if lim == nil {
		lim = map[string]any{}
	}

	setOrDelete(req, "cpu", reqCPU)
	setOrDelete(req, "memory", reqMem)
	setOrDelete(lim, "cpu", limCPU)
	setOrDelete(lim, "memory", limMem)

	res["requests"] = req
	if len(lim) == 0 {
		delete(res, "limits")
	} else {
		res["limits"] = lim
	}
	c["resources"] = res
}

func setOrDelete(m map[string]any, key, val string) {
	if val == "" {
		delete(m, key)
		return
	}
	m[key] = val
}

// writeAnnotation stamps the vertical-rightsize annotation onto the workload so
// the backend can correlate the applied change with this run. Best-effort:
// matches the legacy compact-JSON {identifier, time, container_changes}.
// annotationValue builds the compact-JSON vertical-scaler payload
// {identifier, time, container_changes} the backend reads to correlate a run.
func annotationValue(identifier string, changes []map[string]any) (string, error) {
	payload := map[string]any{
		"identifier":        identifier,
		"time":              time.Now().UTC().Format("2006-01-02T15:04:05.000000"),
		"container_changes": changes,
	}
	// json.Marshal already emits compact JSON (no inter-token spaces); the
	// original's space-stripping was both redundant and unsafe — it would
	// corrupt any value legitimately containing a space.
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func writeAnnotation(obj *unstructured.Unstructured, identifier string, changes []map[string]any) {
	value, err := annotationValue(identifier, changes)
	if err != nil {
		return
	}
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[annotationKey] = value
	obj.SetAnnotations(ann)
}

// --- small value helpers ----------------------------------------------------

func toFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}
