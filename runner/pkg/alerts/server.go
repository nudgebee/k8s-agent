// Package alerts is the in-cluster receiver for AlertManager + kubewatch
// webhook sources. Per plan §2 (backend composes via primitives), this
// agent does NOT match alerts against playbooks, does NOT enrich, does NOT
// build smart findings.
//
// Stage-1A scope: the agent does the bare minimum to package each raw
// alert / kubewatch event into a Finding envelope and POSTs it to the
// existing collector `POST /v1/k8s/events` endpoint. Zero collector
// changes; the existing the backend pipeline accepts these Findings
// directly. When real enrichers ship in api-server (plan §5a), this
// default-Finding builder shrinks to a stub and api-server takes over
// composition.
//
// HTTP routes (kept identical to the legacy runner so chart configs
// don't change at cutover):
//
//	POST /api/alerts        — AlertManager webhook (Prometheus alerts)
//	POST /api/handle        — kubewatch event watcher (chart's existing target)
//	POST /api/k8s-events    — alias for /api/handle (future agent-internal watcher)
//	GET  /healthz           — liveness probe
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

type Forwarder struct {
	BackendURL string // full collector URL, e.g. https://collector.dev.nudgebee.pollux.in/v1/k8s/events
	AuthSecret string // sent as Authorization: <bare-base64(secret)> (matches relay)
	AccountID  string // sent as X-NB-Account-Id (collector adds tenant + cloud_account_id)
	Cluster    string // sent as X-NB-Cluster
	HTTP       *http.Client
	Logger     *slog.Logger

	// Engine evaluates kubewatch K8s events against the trigger matcher
	// set (pkg/triggers). Only events where a matcher fires produce a
	// Finding — "no playbook → no Finding". When unset, every K8s event
	// is dropped (matches the safe default in plan stage 2.1).
	Engine TriggerEngine

	builder      *Builder
	dropped      atomic.Uint64 // forward failures / unparseable input
	k8sUnmatched atomic.Uint64 // kubewatch events received but no matcher fired
}

// TriggerEngine is the subset of pkg/triggers.Engine the forwarder calls.
// Defined as an interface so pkg/alerts doesn't import pkg/triggers
// (the engine wires up at process boot in main.go).
type TriggerEngine interface {
	MatchK8sEvent(operation, kind string, obj, oldObj map[string]any) []MatchedTrigger
}

func NewForwarder(backendURL, authSecret, accountID, cluster string, logger *slog.Logger) *Forwarder {
	if logger == nil {
		logger = slog.Default()
	}
	return &Forwarder{
		BackendURL: backendURL,
		AuthSecret: authSecret,
		AccountID:  accountID,
		Cluster:    cluster,
		HTTP:       &http.Client{Timeout: 10 * time.Second},
		Logger:     logger,
		builder:    &Builder{AccountID: accountID, Cluster: cluster},
	}
}

// Mux returns an http.Handler exposing the alert + kubewatch + healthz
// routes. Mount under the agent's HTTP server (default :5000 — the
// long-standing AlertManager target port).
func (f *Forwarder) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/alerts", f.handleAlert)
	// kubewatch's chart targets `/api/handle`
	// (k8s-agent/charts/nudgebee-agent/templates/kubewatch-configmap.yaml:13).
	// `/api/k8s-events` is an alias for any future agent-internal event watcher.
	mux.HandleFunc("/api/handle", f.handleK8sEvent)
	mux.HandleFunc("/api/k8s-events", f.handleK8sEvent)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	return mux
}

// handleAlert receives an AlertManager webhook (one or more PrometheusAlert
// items under `alerts[]`), builds one Finding envelope per alert via the
// default builder, and POSTs each to the collector. AlertManager retries
// on non-2xx are noisy; we always 202 and meter drops via Dropped().
func (f *Forwarder) handleAlert(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	w.WriteHeader(http.StatusAccepted)

	go func() {
		envelopes, droppedSubjects, err := f.builder.FromAlertManager(body)
		if err != nil {
			f.recordDrop("alertmanager", err)
			return
		}
		if droppedSubjects > 0 {
			f.Logger.Info("alertmanager: dropped alerts with no resolvable subject",
				"count", droppedSubjects)
		}
		for i := range envelopes {
			if err := f.forward(context.Background(), &envelopes[i]); err != nil {
				f.recordDrop("alertmanager", err)
			}
		}
	}()
}

// handleK8sEvent receives a kubewatch payload at `/api/handle` (or alias)
// and runs it through the trigger engine. Effective behaviour: most
// kubewatch events match no trigger and are silently dropped — only
// events matching a registered predicate produce a Finding.
//
// Three payload shapes:
//
//	{"type":"cluster_snapshot", ...}    — drop. pkg/discovery already does
//	                                       informer-driven full sync every
//	                                       30 min.
//	{"data":{operation,kind,obj,...}}   — run through Engine.MatchK8sEvent.
//	                                       One Finding per fired matcher
//	                                       (a Pod can be both
//	                                       ImagePullBackOff and
//	                                       CrashLoopBackOff).
//	{}                                   — malformed; drop with WARN.
func (f *Forwarder) handleK8sEvent(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	w.WriteHeader(http.StatusAccepted)

	var probe struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		f.Logger.Warn("kubewatch: payload parse failed; dropping",
			"err", err, "bytes", len(body))
		return
	}
	if probe.Type == "cluster_snapshot" {
		f.Logger.Info("kubewatch: cluster_snapshot dropped (pkg/discovery already handles full sync)")
		return
	}
	if len(probe.Data) == 0 {
		f.Logger.Warn("kubewatch: payload missing both `type` and `data`; dropping",
			"bytes", len(body))
		return
	}

	if f.Engine == nil {
		// Engine unwired = drop everything. Safe default for environments
		// where matchers haven't been opted in. Operators see the
		// k8sUnmatched counter climb so it's clear events are arriving.
		f.k8sUnmatched.Add(1)
		f.Logger.Debug("kubewatch: trigger engine not configured; dropping event")
		return
	}

	// Parse the inner `data` dict to extract operation/kind/obj/oldObj.
	var inner struct {
		Operation string         `json:"operation"`
		Kind      string         `json:"kind"`
		Obj       map[string]any `json:"obj"`
		OldObj    map[string]any `json:"oldObj"`
	}
	if err := json.Unmarshal(probe.Data, &inner); err != nil {
		f.Logger.Warn("kubewatch: inner data parse failed; dropping",
			"err", err)
		return
	}

	matches := f.Engine.MatchK8sEvent(inner.Operation, inner.Kind, inner.Obj, inner.OldObj)
	if len(matches) == 0 {
		f.k8sUnmatched.Add(1)
		return
	}

	// One Finding per match. The same kubewatch event can fire multiple
	// matchers (e.g., a Pod that's both ImagePullBackOff and CrashLoopBackOff)
	// — each produces its own Finding with its own aggregation_key.
	go func() {
		for i := range matches {
			env, err := f.builder.FromMatchedTrigger(matches[i], probe.Data)
			if err != nil {
				f.recordDrop("kubewatch_matcher_"+matches[i].MatcherName, err)
				continue
			}
			if err := f.forward(context.Background(), env); err != nil {
				f.recordDrop("kubewatch_matcher_"+matches[i].MatcherName, err)
			}
		}
	}()
}

// readBody enforces POST + a 5 MB cap. Returns (body, true) on success,
// or writes an error and returns (nil, false).
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 5<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	_ = r.Body.Close()
	return body, true
}

// forward POSTs a Finding envelope to the collector's existing
// `/v1/k8s/events` endpoint. Body is plain JSON; the legacy sender gzips
// because Findings can include large evidence blocks (Loki logs etc.) —
// our Stage-1A envelopes are KB-scale (raw alert / k8s event payload only),
// so plain JSON is simpler and well under the gzip-makes-sense threshold.
func (f *Forwarder) forward(ctx context.Context, env *FindingEnvelope) error {
	if f.BackendURL == "" {
		return errors.New("backend URL not configured")
	}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.BackendURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if f.AccountID != "" {
		req.Header.Set("X-NB-Account-Id", f.AccountID)
	}
	if f.Cluster != "" {
		req.Header.Set("X-NB-Cluster", f.Cluster)
	}
	if f.AuthSecret != "" {
		// Bare base64 — same shape relay + telemetry use.
		req.Header.Set("Authorization", basicAuth(f.AuthSecret))
	}

	resp, err := f.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("backend HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (f *Forwarder) recordDrop(source string, err error) {
	f.dropped.Add(1)
	f.Logger.Error("event forward failed",
		"source", source, "err", err, "dropped_total", f.dropped.Load())
}

// Dropped returns the running count of events dropped due to forward
// failure or unparseable input. Wire to a Prometheus metric in main.
func (f *Forwarder) Dropped() uint64 { return f.dropped.Load() }

// K8sUnmatched returns the running count of kubewatch events that
// arrived but matched no trigger (or arrived with no engine wired).
// Useful for confirming the agent IS receiving traffic — a plateauing
// counter alongside zero Findings is the signature of "matchers
// running, nothing to fire on", not "agent broken".
func (f *Forwarder) K8sUnmatched() uint64 { return f.k8sUnmatched.Load() }
