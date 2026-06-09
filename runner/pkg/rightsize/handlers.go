package rightsize

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Handlers wires the continuous_rightsizing action into the dispatch registry.
//
// The action is driven by runbook-server (manual workflow task + AutoOptimize
// generator) via the relay's synchronous /request path. It samples Prometheus
// and — unless recommend_only — patches workload resource requests/limits.
func Handlers(r *Rightsizer) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"continuous_rightsizing": r.handle,
	}
}

// handle parses the {settings, applications} action_params, runs the rightsizer
// and returns the legacy response envelope: {success, data:[ApplicationResource]}.
// On failure it returns success=false with the error message so runbook-server's
// checkContinuousRightsizeAgentResp surfaces it (the dispatcher still replies
// HTTP 200; in-band success drives the task outcome).
func (r *Rightsizer) handle(ctx context.Context, params map[string]any) (any, error) {
	s, err := parseSettings(params["settings"])
	if err != nil {
		return failure(err), nil
	}
	apps, err := parseApplications(params["applications"])
	if err != nil {
		return failure(err), nil
	}
	if len(apps) == 0 {
		return failure(fmt.Errorf("rightsize: no applications provided")), nil
	}

	results, err := r.Run(ctx, s, apps)
	if err != nil {
		return failure(err), nil
	}
	return map[string]any{"success": true, "data": results}, nil
}

func failure(err error) map[string]any {
	return map[string]any{"success": false, "data": nil, "msg": err.Error()}
}

// parseSettings resolves the wire settings (with backend defaults) into a
// Settings. Notably: default_min_memory arrives in MiB and is converted to
// bytes; a percentile of 0 ("NB Algo") falls back to the default percentile.
func parseSettings(raw any) (Settings, error) {
	m, _ := raw.(map[string]any)
	if m == nil {
		return Settings{}, fmt.Errorf("rightsize: missing settings")
	}

	s := Settings{
		MinCPU:             numOr(m, "default_min_cpu", defaultMinCPU),
		MinMemoryBytes:     numOr(m, "default_min_memory", 0) * 1024 * 1024, // MiB → bytes
		OOMKillFactor:      numOr(m, "oom_kill_increase_factor", defaultOOMKillFactor),
		ChangeThreshold:    numOr(m, "change_threshold", defaultChangeThreshold),
		MaxChangeThreshold: numOr(m, "max_change_threshold", defaultMaxChangeThreshold),
		CPUPercentile:      numOr(m, "cpu_analysis_percentile", 0),
		MemoryPercentile:   numOr(m, "memory_analysis_percentile", 0),
		AnalysisDuration:   int(numOr(m, "default_analysis_duration_hour", defaultAnalysisDuration)),
		RecommendOnly:      boolOr(m, "recommend_only", false),
		InPlace:            boolOr(m, "in_place", true),
		Identifier:         strField(m, "identifier"),
	}

	if s.MinMemoryBytes <= 0 {
		s.MinMemoryBytes = defaultMinMemoryBytes
	}
	if s.OOMKillFactor <= 0 {
		s.OOMKillFactor = defaultOOMKillFactor
	}
	if s.MaxChangeThreshold <= 0 {
		s.MaxChangeThreshold = defaultMaxChangeThreshold
	}
	if s.AnalysisDuration <= 0 {
		s.AnalysisDuration = defaultAnalysisDuration
	}
	// percentile 0 means "use the agent default" (the "NB Algo" form option).
	if s.CPUPercentile <= 0 {
		s.CPUPercentile = defaultPercentile
	}
	if s.MemoryPercentile <= 0 {
		s.MemoryPercentile = defaultPercentile
	}
	return s, nil
}

func parseApplications(raw any) ([]Application, error) {
	list, _ := raw.([]any)
	if list == nil {
		return nil, fmt.Errorf("rightsize: applications must be a list")
	}
	apps := make([]Application, 0, len(list))
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m == nil {
			continue
		}
		app := Application{
			Name:      strField(m, "name"),
			Namespace: strField(m, "namespace"),
			Kind:      strField(m, "kind"),
		}
		if app.Name == "" || app.Namespace == "" || app.Kind == "" {
			return nil, fmt.Errorf("rightsize: application requires name, namespace and kind (got %+v)", m)
		}
		apps = append(apps, app)
	}
	return apps, nil
}

// numOr coerces a JSON number/string at key into float64, falling back to def.
func numOr(m map[string]any, key string, def float64) float64 {
	v, ok := m[key]
	if !ok {
		return def
	}
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
	case string:
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f
		}
	}
	return def
}

func boolOr(m map[string]any, key string, def bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return def
}

func strField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
