package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/kube"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

// AlertRulesCollector pushes Prometheus alert rules + PrometheusRule CRDs to
// the collector's /v1/k8s/discovery endpoint, which UPSERTs them into the
// `event_rules` table. Mirrors the legacy agent's `get_rules()` +
// `post_discovery("alert_rules", ...)` push.
//
// Wire shape the collector's alert_rules_handler.handle_alert_rules expects
// :
//
//	{
//	  "api_based_rules": <prom /api/v1/rules response.data>,   // optional
//	  "crd_based_rules": {"items": [<PrometheusRule CRD>, ...]}, // optional
//	}
//
// Both fields are optional — collector tolerates either being absent. CRD path
// is the canonical one because every prometheus-operator-installed alert is a
// PrometheusRule CRD; the API path adds in-line rules configured on Prometheus
// directly (not present on every cluster).
type AlertRulesCollector struct {
	Prom   *prometheus.Client // may be nil — pure CRD-mode is supported
	Kube   *kube.Client       // may be nil — only Prom rules will be pushed
	Sink   *Sink              // required
	Logger *slog.Logger
}

// Run blocks until ctx is done, emitting an envelope every `interval`. The
// first emit happens immediately so the canary populates event_rules without
// waiting for the first tick.
func (c *AlertRulesCollector) Run(ctx context.Context, interval time.Duration) error {
	if c.Sink == nil {
		return errors.New("alertrules: Sink is required")
	}
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("collector", "alert_rules", "interval", interval)

	emit := func() {
		emitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		if err := c.Collect(emitCtx); err != nil {
			logger.Error("emit failed", "err", err)
		}
	}
	emit()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			emit()
		}
	}
}

// Collect pulls rules from Prometheus (best-effort) and PrometheusRule CRDs
// (best-effort), then POSTs them as one envelope. Returns nil when both
// sources are unavailable — no envelope, no error, just a logged debug line.
//
// Exported (vs Run) so the cron handler tests / integration tests can drive
// a single emit without spinning a ticker.
func (c *AlertRulesCollector) Collect(ctx context.Context) error {
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}

	payload := map[string]any{}

	// Prometheus /api/v1/rules — best-effort. VictoriaMetrics' vmsingle
	// returns 404; we silently fall back to CRD-only mode.
	if c.Prom != nil && c.Prom.BaseURL != "" {
		raw, err := c.Prom.Rules(ctx)
		if err == nil && len(raw) > 0 {
			parsed, ok := unwrapPromRules(raw)
			if ok {
				payload["api_based_rules"] = parsed
			} else {
				logger.Debug("prometheus rules response not parseable, skipping",
					"raw_bytes", len(raw))
			}
		} else if err != nil {
			logger.Debug("prometheus /api/v1/rules unavailable, skipping",
				"err", err)
		}
	}

	// PrometheusRule CRDs — list all monitoring.coreos.com/v1 PrometheusRules.
	// camelCase keys preserved (UnstructuredContent) which is exactly what the
	// collector's handle_crd_based_rules expects (it reads `spec.groups`,
	// `rule.expr`, `rule.for`, etc. as camelCase).
	if c.Kube != nil {
		items, err := c.Kube.GetResource(ctx, kube.GetParams{
			Group:         "monitoring.coreos.com",
			Version:       "v1",
			ResourceType:  "prometheusrules",
			AllNamespaces: true,
		})
		if err == nil {
			arr, _ := items.([]any)
			if len(arr) > 0 {
				payload["crd_based_rules"] = map[string]any{"items": arr}
			}
		} else {
			// CRD not installed → tolerate. No CRDs configured is also a
			// legitimate state.
			logger.Debug("PrometheusRule CRD list failed (CRD may be missing)",
				"err", err)
		}
	}

	if len(payload) == 0 {
		logger.Debug("alert_rules: no rules to push this tick")
		return nil
	}

	// alert_rules is the one discovery type whose collector handler
	// expects Data to be a single dict, not
	// a list of items. Other discovery types batch items (services, pods,
	// nodes, …) and send Data as []any. Envelope.Data is `any` to
	// accommodate both shapes; this site sends the dict unwrapped to
	// avoid the AttributeError "'list' object has no attribute 'get'"
	// the worker raised when payload was wrapped in `[]any{payload}`.
	envelope := &Envelope{
		Type:         TypeAlertRules,
		Data:         payload,
		FullLoad:     true,
		IsFirstBatch: true,
		IsLastBatch:  true,
		TotalBatches: 1,
	}
	if err := c.Sink.Post(ctx, envelope); err != nil {
		return fmt.Errorf("post alert_rules: %w", err)
	}
	logger.Info("alert_rules: pushed",
		"has_api_rules", payload["api_based_rules"] != nil,
		"crd_count", crdCount(payload))
	return nil
}

// unwrapPromRules decodes the /api/v1/rules response wrapper
// `{status: "success", data: {groups: [...]}}` into just the data object
// . Returns ok=false when the
// shape doesn't match — caller logs + skips.
func unwrapPromRules(raw json.RawMessage) (map[string]any, bool) {
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, false
	}
	if envelope.Status != "success" || len(envelope.Data) == 0 {
		return nil, false
	}
	var data map[string]any
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		return nil, false
	}
	if _, ok := data["groups"]; !ok {
		return nil, false
	}
	return data, true
}

func crdCount(payload map[string]any) int {
	crd, ok := payload["crd_based_rules"].(map[string]any)
	if !ok {
		return 0
	}
	items, ok := crd["items"].([]any)
	if !ok {
		return 0
	}
	return len(items)
}
