// Package dispatch is the action router for inbound WS messages.
//
// It owns:
//   - the action-name → handler registry
//   - normal and high-priority worker pools (WEBSOCKET_THREADPOOL_SIZE +
//     WEBSOCKET_HIGH_PRIORITY_THREADPOOL_SIZE)
//   - the per-handler 180s deadline (WEBSOCKET_TASK_TIMEOUT_SECONDS)
//   - auth check before any handler runs
//   - response envelope construction
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/nudgebee/nudgebee-agent/pkg/auth"
	"github.com/nudgebee/nudgebee-agent/pkg/relay"
)

// Handler is one action implementation. It receives a request-scoped context
// (already wrapped with the 180s deadline) and the action_params map.
// Return either data (success) or an error (will become status_code 500).
type Handler func(ctx context.Context, params map[string]any) (any, error)

// Config tunes pool sizes and timeouts.
type Config struct {
	NormalPoolSize       int           // default 10 (WEBSOCKET_THREADPOOL_SIZE)
	HighPriorityPoolSize int           // default 3  (WEBSOCKET_HIGH_PRIORITY_THREADPOOL_SIZE)
	TaskTimeout          time.Duration // default 180s
	Logger               *slog.Logger

	// LongTaskTimeout is the deadline applied to actions in LongActions when
	// invoked through HandleTrusted (the agent_task poller). It exists for
	// long-running remediations — notably the rightsize_pvc downsize
	// migration, which copies volume data via a mover pod and routinely
	// exceeds the 180s default. 0 disables the override (LongActions then run
	// under TaskTimeout). Only the trusted poller path honours this; the WS
	// Handle path always uses TaskTimeout (no long action reaches it).
	LongTaskTimeout time.Duration
	LongActions     map[string]struct{}
}

// Metrics is the optional metrics sink. Pass *metrics.Registry from main, or
// nil to disable. Kept as a narrow interface so pkg/dispatch doesn't depend
// on prometheus types.
type Metrics interface {
	OnAction(action, status string)
	OnActionDuration(action string, seconds float64)
}

// TerminalHandler answers a TerminalRequest (pod-shell start/exec/read/close).
// The dispatcher routes to it when an inbound WS message has
// `action: start|exec|read|close` instead of `body.action_name`.
type TerminalHandler interface {
	Handle(ctx context.Context, r *TerminalRequest) (resp any, status int)
}

// GrafanaHandler answers a GrafanaRequest. Routed by the dispatcher when an
// inbound WS message has `method` + `url` (no `body.action_name`). The
// X-NB-Request-Type header decides Grafana-proxy vs API-proxy.
type GrafanaHandler interface {
	HandleGrafana(ctx context.Context, r *GrafanaRequest) any
	HandleAPI(ctx context.Context, baseURL string, r *GrafanaRequest) any
	// HandlePrometheus serves X-NB-Request-Type=Prometheus payloads.
	// The backend forwards every inbound Prometheus HTTP call through
	// this path.
	HandlePrometheus(ctx context.Context, r *GrafanaRequest) any
}

// TerminalRequest is the wire shape of a pod-shell command.
type TerminalRequest struct {
	Action    string `json:"action"`
	SessionID string `json:"session_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Command   string `json:"command,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// GrafanaRequest is the wire shape of a Grafana / API-proxy call.
type GrafanaRequest struct {
	Method        string              `json:"method"`
	URL           string              `json:"url"`
	ContentLength int                 `json:"content_length,omitempty"`
	Body          string              `json:"body,omitempty"`
	Header        map[string][]string `json:"header,omitempty"`
}

// Dispatcher routes WS messages to handlers under semaphore-bounded concurrency.
type Dispatcher struct {
	cfg      Config
	auth     *auth.Validator
	handlers map[string]Handler
	metrics  Metrics

	terminal TerminalHandler
	grafana  GrafanaHandler

	normal *semaphore.Weighted
	high   *semaphore.Weighted
}

// SetTerminal wires the pod-shell handler. Optional; if unset the dispatcher
// returns 501 to TerminalRequests.
func (d *Dispatcher) SetTerminal(h TerminalHandler) { d.terminal = h }

// SetGrafana wires the Grafana proxy. Optional; if unset the dispatcher
// returns 501 to GrafanaRequests.
func (d *Dispatcher) SetGrafana(h GrafanaHandler) { d.grafana = h }

// SetMetrics wires the metrics sink. Optional; safe to leave unset.
func (d *Dispatcher) SetMetrics(m Metrics) { d.metrics = m }

// HandleTrusted invokes a registered handler without auth validation. Used by
// the task poller (pkg/tasks): tasks are pulled over an authenticated GET
// /v1/k8s/tasks call so the payload itself is implicitly trusted — no HMAC
// signature on the wire to verify. Same pool / timeout / metrics path as
// Handle, just no auth check and no relay response envelope.
//
// Returns (data, ok=true, error=nil) on success; (nil, ok=false, nil) when
// the action_name isn't registered; (nil, ok=true, error) on handler failure.
func (d *Dispatcher) HandleTrusted(ctx context.Context, actionName string, params map[string]any, highPriority bool) (any, bool, error) {
	logger := d.cfg.Logger.With("action_name", actionName, "trusted", true)
	handler, ok := d.handlers[actionName]
	if !ok {
		return nil, false, nil
	}
	pool := d.normal
	if highPriority {
		pool = d.high
	}
	if err := pool.Acquire(ctx, 1); err != nil {
		return nil, true, fmt.Errorf("pool acquire: %w", err)
	}
	defer pool.Release(1)

	timeout := d.cfg.TaskTimeout
	if d.cfg.LongTaskTimeout > 0 {
		if _, long := d.cfg.LongActions[actionName]; long {
			timeout = d.cfg.LongTaskTimeout
		}
	}
	taskCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	data, err := handler(taskCtx, params)
	elapsed := time.Since(start)
	if d.metrics != nil {
		d.metrics.OnActionDuration(actionName, elapsed.Seconds())
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		logger.Error("trusted task: timeout", "elapsed", elapsed)
		if d.metrics != nil {
			d.metrics.OnAction(actionName, "timeout")
		}
		return nil, true, err
	case err != nil:
		logger.Error("trusted task: handler error", "err", err, "elapsed", elapsed)
		if d.metrics != nil {
			d.metrics.OnAction(actionName, "error")
		}
		return nil, true, err
	default:
		logger.Info("trusted task: ok", "elapsed", elapsed)
		if d.metrics != nil {
			d.metrics.OnAction(actionName, "ok")
		}
		return data, true, nil
	}
}

// New builds a Dispatcher. Pass nil for auth to disable auth (test only).
func New(cfg Config, v *auth.Validator, handlers map[string]Handler) *Dispatcher {
	if cfg.NormalPoolSize <= 0 {
		cfg.NormalPoolSize = 10
	}
	if cfg.HighPriorityPoolSize <= 0 {
		cfg.HighPriorityPoolSize = 3
	}
	if cfg.TaskTimeout <= 0 {
		cfg.TaskTimeout = 180 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Dispatcher{
		cfg:      cfg,
		auth:     v,
		handlers: handlers,
		normal:   semaphore.NewWeighted(int64(cfg.NormalPoolSize)),
		high:     semaphore.NewWeighted(int64(cfg.HighPriorityPoolSize)),
	}
}

// Handle is the entry point passed to relay.NewClient. It parses the inbound
// envelope, authenticates, dispatches to a handler under the appropriate pool,
// and writes the response back.
//
// Three inbound shapes (tried in order):
//
//	ExternalActionRequest — has `body.action_name`. Regular action flow.
//	GrafanaRequest        — has `method` + `url`. HTTP-proxy flow.
//	TerminalRequest       — has `action: start|exec|read|close`. Pod-shell flow.
//
// Grafana + Terminal responses must be JSON-stringified into AgentResponse.Data
// because the relay's grafana / interactive-shell handlers unmarshal it as
// `data string` (relay-server/pkg/server/handlers/{grafana,ws}.go).
func (d *Dispatcher) Handle(ctx context.Context, msg []byte, send relay.SendFunc) {
	// Probe shape with a single permissive parse — we look for the
	// discriminating fields without committing to a struct.
	var probe struct {
		Body   *json.RawMessage `json:"body,omitempty"`
		Action string           `json:"action,omitempty"`
		Method string           `json:"method,omitempty"`
		URL    string           `json:"url,omitempty"`
	}
	_ = json.Unmarshal(msg, &probe) // tolerant; downstream paths will reject bad JSON

	switch {
	case probe.Body != nil:
		// regular action flow — fall through to body parser below.
	case probe.Method != "" && probe.URL != "":
		d.handleGrafana(ctx, msg, send)
		return
	case probe.Action == "start" || probe.Action == "exec" || probe.Action == "read" || probe.Action == "close":
		d.handleTerminal(ctx, msg, send)
		return
	}

	// Parse the envelope as a generic map first so we can pass the body to the
	// auth validator (which needs the raw map for canonical-JSON re-encoding).
	var raw struct {
		Body         map[string]any `json:"body"`
		Signature    string         `json:"signature,omitempty"`
		PartialAuthA string         `json:"partial_auth_a,omitempty"`
		PartialAuthB string         `json:"partial_auth_b,omitempty"`
		RequestID    string         `json:"request_id,omitempty"`
	}
	if err := json.Unmarshal(msg, &raw); err != nil {
		d.cfg.Logger.Error("dispatch: failed to parse envelope", "err", err)
		return
	}

	actionName, _ := raw.Body["action_name"].(string)
	logger := d.cfg.Logger.With("action_name", actionName, "request_id", raw.RequestID)

	if d.auth != nil {
		authReq := &auth.Request{
			Body:         raw.Body,
			ActionName:   actionName,
			Signature:    raw.Signature,
			PartialAuthA: raw.PartialAuthA,
			PartialAuthB: raw.PartialAuthB,
		}
		if err := d.auth.Validate(authReq); err != nil {
			logger.Warn("dispatch: auth rejected", "err", err)
			d.respond(send, raw.RequestID, 401, map[string]any{"error": err.Error()})
			return
		}
	}

	handler, ok := d.handlers[actionName]
	if !ok {
		logger.Warn("dispatch: unknown action")
		d.respond(send, raw.RequestID, 404, map[string]any{"error": "action not registered: " + actionName})
		return
	}

	params, _ := raw.Body["action_params"].(map[string]any)
	highPriority := isHighPriority(params)
	pool := d.normal
	if highPriority {
		pool = d.high
	}

	if err := pool.Acquire(ctx, 1); err != nil {
		logger.Warn("dispatch: pool acquire failed", "err", err)
		d.respond(send, raw.RequestID, 503, map[string]any{"error": "agent overloaded"})
		return
	}
	defer pool.Release(1)

	taskCtx, cancel := context.WithTimeout(ctx, d.cfg.TaskTimeout)
	defer cancel()

	start := time.Now()
	data, err := handler(taskCtx, params)
	elapsed := time.Since(start)

	if d.metrics != nil {
		d.metrics.OnActionDuration(actionName, elapsed.Seconds())
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		logger.Error("dispatch: task timeout", "elapsed", elapsed)
		d.respond(send, raw.RequestID, 504, map[string]any{"error": "task timeout"})
		if d.metrics != nil {
			d.metrics.OnAction(actionName, "timeout")
		}
	case err != nil:
		logger.Error("dispatch: handler error", "err", err, "elapsed", elapsed)
		d.respond(send, raw.RequestID, 500, map[string]any{"error": err.Error()})
		if d.metrics != nil {
			d.metrics.OnAction(actionName, "error")
		}
	default:
		logger.Info("dispatch: action ok", "elapsed", elapsed)
		d.respond(send, raw.RequestID, 200, data)
		if d.metrics != nil {
			d.metrics.OnAction(actionName, "ok")
		}
	}
}

func (d *Dispatcher) respond(send relay.SendFunc, requestID string, status int, data any) {
	if requestID == "" {
		// Fire-and-forget request; no response expected.
		return
	}
	if err := send(&relay.Response{
		Action:     "response",
		RequestID:  requestID,
		StatusCode: status,
		Data:       data,
		OutputType: "actions",
	}); err != nil {
		d.cfg.Logger.Error("dispatch: send response failed", "err", err)
	}
}

// isHighPriority returns true when action_params.high_priority is set.
func isHighPriority(params map[string]any) bool {
	if params == nil {
		return false
	}
	v, _ := params["high_priority"].(bool)
	return v
}

// handleTerminal answers a TerminalRequest. The reply envelope MUST have
// `data` as a JSON-stringified TerminalResponse and `output_type=Terminal`,
// or the relay's interactive-shell handler 500s on
// `cannot unmarshal object into Go struct field AgentResponse.data of type
// string`.
func (d *Dispatcher) handleTerminal(ctx context.Context, msg []byte, send relay.SendFunc) {
	var req TerminalRequest
	if err := json.Unmarshal(msg, &req); err != nil {
		d.cfg.Logger.Error("dispatch: terminal payload parse failed", "err", err)
		return
	}
	logger := d.cfg.Logger.With("kind", "terminal", "action", req.Action, "session_id", req.SessionID, "request_id", req.RequestID)
	if d.terminal == nil {
		logger.Warn("dispatch: terminal handler not configured")
		d.respondString(send, req.RequestID, 501, map[string]any{"error": "pod_shell not enabled on this agent"}, "Terminal")
		return
	}
	resp, status := d.terminal.Handle(ctx, &req)
	logger.Info("dispatch: terminal action ok", "status", status)
	d.respondString(send, req.RequestID, status, resp, "Terminal")
}

// handleGrafana answers a GrafanaRequest. The X-NB-Request-Type header
// chooses Grafana proxy vs API proxy. Reply envelope
// has `data` as JSON-stringified GrafanaResponse and `output_type=Grafana`.
//
// The relay reads the request_id from the X-Nb-Request-Id header
// — not from a top-level field — so we extract it from
// the request headers.
func (d *Dispatcher) handleGrafana(ctx context.Context, msg []byte, send relay.SendFunc) {
	var req GrafanaRequest
	if err := json.Unmarshal(msg, &req); err != nil {
		d.cfg.Logger.Error("dispatch: grafana payload parse failed", "err", err)
		return
	}
	requestID := firstHeader(req.Header, "X-Nb-Request-Id")
	requestType := firstHeader(req.Header, "X-NB-Request-Type")
	if requestType == "" {
		requestType = "Grafana"
	}
	logger := d.cfg.Logger.With("kind", "grafana", "request_type", requestType, "request_id", requestID, "url", req.URL)

	if d.grafana == nil {
		logger.Warn("dispatch: grafana handler not configured")
		d.respondString(send, requestID, 501, map[string]any{"error": "grafana proxy not enabled on this agent"}, "Grafana")
		return
	}
	var resp any
	switch requestType {
	case "APIProxy":
		base := firstHeader(req.Header, "X-API-Base-URL")
		resp = d.grafana.HandleAPI(ctx, base, &req)
	case "Prometheus":
		// collector-server's `/prometheus-v2/*` route forwards every
		// Prometheus HTTP call this way (relay-server/pkg/utils/utils.go:77).
		// We proxy to the agent's configured PROMETHEUS_URL — same upstream
		// the prometheus_enricher action queries, just exposed as raw HTTP
		// for callers that need the full /api/v1/* surface.
		resp = d.grafana.HandlePrometheus(ctx, &req)
	default: // "Grafana"
		resp = d.grafana.HandleGrafana(ctx, &req)
	}
	logger.Info("dispatch: grafana ok")
	d.respondString(send, requestID, 200, resp, "Grafana")
}

// respondString JSON-stringifies data, then sends a Response envelope with
// Data=string(jsonBytes) and the requested OutputType.
func (d *Dispatcher) respondString(send relay.SendFunc, requestID string, status int, data any, outputType string) {
	if requestID == "" {
		// Fire-and-forget — relay didn't ask for a reply.
		return
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		d.cfg.Logger.Error("dispatch: marshal stringified data failed", "err", err, "request_id", requestID)
		return
	}
	if err := send(&relay.Response{
		Action:     "response",
		RequestID:  requestID,
		StatusCode: status,
		Data:       string(encoded),
		OutputType: outputType,
	}); err != nil {
		d.cfg.Logger.Error("dispatch: send stringified response failed", "err", err)
	}
}

// firstHeader returns the first value of a header (case-insensitive),
// matching incoming_event.header.get(name)[0].
func firstHeader(h map[string][]string, key string) string {
	for k, v := range h {
		if strings.EqualFold(k, key) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// SimpleHandler wraps a func(params)->(data, err) for callers who don't need ctx.
func SimpleHandler(fn func(params map[string]any) (any, error)) Handler {
	return func(_ context.Context, params map[string]any) (any, error) {
		return fn(params)
	}
}

// MustEncodeJSON is a convenience for handlers that already have a JSON-marshalable
// result; mainly for sanity-checking that values can round-trip.
func MustEncodeJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("MustEncodeJSON: %v", err))
	}
	return b
}
