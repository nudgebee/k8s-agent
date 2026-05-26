package podrunner

import (
	"context"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Handlers wires the pod_script_run_enricher action into the dispatch
// registry. Separate handler — wraps Runner.Handle in the dispatch shape
// without re-exporting the lower-level helpers.
func Handlers(r *Runner) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"pod_script_run_enricher": func(ctx context.Context, p map[string]any) (any, error) {
			return r.Handle(ctx, p)
		},
	}
}
