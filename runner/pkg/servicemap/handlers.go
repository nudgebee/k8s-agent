package servicemap

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
	"github.com/nudgebee/nudgebee-agent/pkg/enrichers"
)

// Handlers wires the service-map actions. service_map and
// service_map_enricher share the same Build implementation here —
// both route into generate_service_map — but they have different wire
// shapes:
//
//   - `service_map` is called directly by the UI
//     (KubernetesServiceMap.jsx:442, ServiceMapCard.js:118). UI reads
//     `response.data.data` as an array, so the agent must wrap the
//     []Application in `{data: [...]}`.
//   - `service_map_enricher` and `traces_dependency_map` are called by
//     api-server playbooks / LLM tools. They expect a Finding envelope
//     (findings[0].evidence[0].data → JSON-stringified blocks) like
//     every other enricher, so we wrap via the standard
//     enrichers.JSONBlock + FindingResponse path.
//
// Without these distinct wrappers, the canary's eBPF Service Map page
// rendered empty even though the agent returned a healthy ~1MB
// []Application slice — `res.data.data` was undefined because the
// outer wrapper key was missing.
func Handlers(s *Service, accountID string) map[string]dispatch.Handler {
	if s == nil {
		return nil
	}
	build := func(ctx context.Context, p map[string]any) ([]Application, error) {
		return s.Build(ctx, parseFilterParams(p))
	}

	// service_map: thin {data: [...]} wrapper for the UI.
	directWrap := func(ctx context.Context, p map[string]any) (any, error) {
		apps, err := build(ctx, p)
		if err != nil {
			return nil, err
		}
		return map[string]any{"data": apps}, nil
	}

	// service_map_enricher / traces_dependency_map: Finding-shape envelope.
	enricherWrap := func(ctx context.Context, p map[string]any) (any, error) {
		apps, err := build(ctx, p)
		if err != nil {
			return nil, err
		}
		block, err := enrichers.JSONBlock(apps)
		if err != nil {
			return nil, err
		}
		return enrichers.FindingResponse(accountID, uuid.Nil, block)
	}

	return map[string]dispatch.Handler{
		"service_map":           directWrap,
		"service_map_enricher":  enricherWrap,
		"traces_dependency_map": enricherWrap,
	}
}

func parseFilterParams(p map[string]any) FilterParams {
	out := FilterParams{
		WorkloadName:      str(p, "workload_name"),
		WorkloadNamespace: str(p, "workload_namespace"),
		StartTime:         str(p, "r_start_time"),
		EndTime:           str(p, "r_end_time"),
	}
	if v, ok := p["duration"].(float64); ok {
		out.Duration = int(v)
	}
	// Some callers nest the filter under "workload_filter".
	if wf, ok := p["workload_filter"].(map[string]any); ok {
		if out.WorkloadName == "" {
			out.WorkloadName = str(wf, "workload_name")
		}
		if out.WorkloadNamespace == "" {
			out.WorkloadNamespace = str(wf, "workload_namespace")
		}
	}
	return out
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

// errNoProm is returned when callers try to dispatch service_map without
// a Prometheus client wired. Kept exported for tests that want to assert
// the exact error.
var errNoProm = errors.New("servicemap: prometheus client not configured")

var _ = errNoProm // silence unused warning when tests don't import it
