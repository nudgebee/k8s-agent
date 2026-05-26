package mutate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Handlers wires Group-D mutation actions into the dispatch registry.
// Caller MUST add these action names to the auth.Validator's RSA-required
// set, NOT the light-action allowlist (see cmd/agent/main.go).
func Handlers(m *Mutator) map[string]dispatch.Handler {
	hs := map[string]dispatch.Handler{
		"delete_pod":      wrapErr(m, handleDeletePod),
		"delete_job":      wrapErr(m, handleDeleteJob),
		"cordon":          wrapErr(m, handleCordon),
		"uncordon":        wrapErr(m, handleUncordon),
		"rollout_restart": wrapErr(m, handleRolloutRestart),
		"drain":           wrap(m, handleDrain),
	}
	if m.AlertManagerURL != "" {
		hs["get_silences"] = wrap(m, handleGetSilences)
		hs["add_silence"] = wrap(m, handleAddSilence)
		hs["delete_silence"] = wrap(m, handleDeleteSilence)
	}
	if m.dynamic != nil {
		hs["create_or_replace_alert_rule"] = wrap(m, handleCreateOrReplacePromRule)
		hs["delete_alert_rule"] = wrapErr(m, handleDeletePromRule)
		hs["replace_workload"] = wrap(m, handleReplaceWorkload)
	}
	if m.LokiRulesURL != "" {
		hs["create_loki_alert_rule"] = wrap(m, handleCreateLokiRule)
		hs["update_loki_alert_rule"] = wrap(m, handleCreateLokiRule) // upsert; same handler
		hs["delete_loki_alert_rule"] = wrap(m, handleDeleteLokiRule)
	}
	return hs
}

func wrap(m *Mutator, fn func(context.Context, *Mutator, map[string]any) (any, error)) dispatch.Handler {
	return func(ctx context.Context, p map[string]any) (any, error) {
		return fn(ctx, m, p)
	}
}

// wrapErr is for the K8s mutations that return only error; we wrap the
// success case as {ok: true}.
func wrapErr(m *Mutator, fn func(context.Context, *Mutator, map[string]any) error) dispatch.Handler {
	return func(ctx context.Context, p map[string]any) (any, error) {
		if err := fn(ctx, m, p); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
}

func handleDeletePod(ctx context.Context, m *Mutator, p map[string]any) error {
	var grace *int64
	if g, ok := p["grace_period_seconds"].(float64); ok {
		v := int64(g)
		grace = &v
	}
	return m.DeletePod(ctx, str(p, "namespace"), str(p, "name"), grace)
}

func handleDeleteJob(ctx context.Context, m *Mutator, p map[string]any) error {
	return m.DeleteJob(ctx, str(p, "namespace"), str(p, "name"))
}

func handleCordon(ctx context.Context, m *Mutator, p map[string]any) error {
	return m.Cordon(ctx, str(p, "node"))
}

func handleUncordon(ctx context.Context, m *Mutator, p map[string]any) error {
	return m.Uncordon(ctx, str(p, "node"))
}

func handleRolloutRestart(ctx context.Context, m *Mutator, p map[string]any) error {
	return m.RolloutRestart(ctx, str(p, "kind"), str(p, "namespace"), str(p, "name"))
}

func handleDrain(ctx context.Context, m *Mutator, p map[string]any) (any, error) {
	opts := DrainOptions{
		IgnoreDaemonSets:   getBool(p, "ignore_daemonsets", true),
		DeleteEmptyDirData: getBool(p, "delete_emptydir_data", false),
		Force:              getBool(p, "force", false),
		DisableEviction:    getBool(p, "disable_eviction", false),
	}
	if t, ok := p["timeout_seconds"].(float64); ok {
		opts.Timeout = time.Duration(t) * time.Second
	}
	if g, ok := p["grace_period_seconds"].(float64); ok {
		v := int64(g)
		opts.GracePeriodSeconds = &v
	}
	return m.Drain(ctx, str(p, "node"), opts)
}

func getBool(m map[string]any, k string, fallback bool) bool {
	if m == nil {
		return fallback
	}
	if v, ok := m[k].(bool); ok {
		return v
	}
	return fallback
}

func handleCreateOrReplacePromRule(ctx context.Context, m *Mutator, p map[string]any) (any, error) {
	rule, ok := p["rule"]
	if !ok {
		// Allow the caller to pass the manifest at the top level too.
		rule = p
	}
	return m.CreateOrReplacePrometheusRule(ctx, rule)
}

func handleDeletePromRule(ctx context.Context, m *Mutator, p map[string]any) error {
	return m.DeletePrometheusRule(ctx, str(p, "namespace"), str(p, "name"))
}

// handleReplaceWorkload accepts the body as either:
//   - a per-kind field (`deployment`, `daemonset`, `statefulset`,
//     `replicaset`, `rollout`, `nodepool`, `ec2nodeclass`) — matches
//     the ReplaceNamespacedWorkload params shape verbatim, so a caller
//     forwarding ReplaceNamespacedWorkload.dict() works unchanged.
//   - a single `body` field with the manifest — for Go-shaped callers
//     that don't want per-kind fields.
//   - top-level `apiVersion`/`kind`/`metadata`/`spec` — when the params
//     map IS the manifest. Useful for kubectl-style invocations.
//
// `kind` / `name` / `namespace` come from explicit params (the legacy
// implementation reads them from the trigger event; for an RPC-style
// invocation they have to be passed in). The success response carries
// the updated object plus a `message` formatted as
// `<Kind>/<ns>/<name> updated` markdown so callers that key off the
// message string don't need to special-case.
func handleReplaceWorkload(ctx context.Context, m *Mutator, p map[string]any) (any, error) {
	kind := str(p, "kind")
	if kind == "" {
		return nil, errors.New("replace_workload: kind required (Deployment|DaemonSet|StatefulSet|ReplicaSet|Rollout|NodePool|EC2NodeClass)")
	}
	name := str(p, "name")
	namespace := str(p, "namespace")
	body := pickReplaceBody(kind, p)
	if body == nil {
		return nil, fmt.Errorf("replace_workload: body required (pass under %q, %q, or full manifest at top level)",
			perKindBodyField(kind), "body")
	}
	updated, err := m.ReplaceWorkload(ctx, kind, namespace, name, body)
	if err != nil {
		return nil, err
	}
	loc := name
	if namespace != "" {
		loc = namespace + "/" + name
	}
	return map[string]any{
		"updated": updated,
		"message": fmt.Sprintf("%s/%s updated", kind, loc),
	}, nil
}

// pickReplaceBody walks the three shapes documented on handleReplaceWorkload
// and returns the first non-nil body. Returning `nil` triggers the missing-
// body error path; the caller surfaces a hint pointing at the right field
// name for the kind they sent.
func pickReplaceBody(kind string, p map[string]any) any {
	if p == nil {
		return nil
	}
	if v, ok := p[perKindBodyField(kind)]; ok && v != nil {
		return v
	}
	if v, ok := p["body"]; ok && v != nil {
		return v
	}
	// Top-level manifest detection: apiVersion + kind + metadata together
	// strongly indicate the params map IS the manifest. We don't accept
	// metadata alone (some callers send {kind, name, namespace, metadata:
	// {labels: {...}}} which isn't a manifest).
	if _, hasAV := p["apiVersion"]; hasAV {
		if _, hasKind := p["kind"]; hasKind {
			if _, hasMeta := p["metadata"]; hasMeta {
				return p
			}
		}
	}
	return nil
}

// perKindBodyField maps each supported kind to the lowercase field name
// ReplaceNamespacedWorkload uses (deployment, daemonset, …). Returns ""
// for unsupported kinds — the upstream kind check catches those before
// we get here.
func perKindBodyField(kind string) string {
	switch kind {
	case "Deployment":
		return "deployment"
	case "DaemonSet":
		return "daemonset"
	case "StatefulSet":
		return "statefulset"
	case "ReplicaSet":
		return "replicaset"
	case "Rollout":
		return "rollout"
	case "NodePool":
		return "nodepool"
	case "EC2NodeClass":
		return "ec2nodeclass"
	}
	return ""
}

func handleCreateLokiRule(ctx context.Context, m *Mutator, p map[string]any) (any, error) {
	body, _ := p["body"].(string)
	resp, err := m.CreateOrReplaceLokiAlertRule(ctx, str(p, "namespace"), body)
	if err != nil {
		return nil, err
	}
	return map[string]any{"raw": string(resp)}, nil
}

func handleDeleteLokiRule(ctx context.Context, m *Mutator, p map[string]any) (any, error) {
	resp, err := m.DeleteLokiAlertRule(ctx, str(p, "namespace"), str(p, "group"))
	if err != nil {
		return nil, err
	}
	return map[string]any{"raw": string(resp)}, nil
}

func handleGetSilences(ctx context.Context, m *Mutator, p map[string]any) (any, error) {
	var filters []string
	if f, ok := p["filters"].([]any); ok {
		for _, x := range f {
			if s, ok := x.(string); ok {
				filters = append(filters, s)
			}
		}
	}
	body, err := m.GetSilences(ctx, filters)
	if err != nil {
		return nil, err
	}
	return map[string]any{"raw": string(body)}, nil
}

func handleAddSilence(ctx context.Context, m *Mutator, p map[string]any) (any, error) {
	body, err := ParseSilenceBody(p)
	if err != nil {
		return nil, err
	}
	resp, err := m.AddSilence(ctx, body)
	if err != nil {
		return nil, err
	}
	return map[string]any{"raw": string(resp)}, nil
}

func handleDeleteSilence(ctx context.Context, m *Mutator, p map[string]any) (any, error) {
	resp, err := m.DeleteSilence(ctx, str(p, "id"))
	if err != nil {
		return nil, err
	}
	return map[string]any{"raw": string(resp)}, nil
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}
