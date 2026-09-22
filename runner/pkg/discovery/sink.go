// Package discovery is the in-cluster Kubernetes discovery subsystem. It
// drives a client-go shared informer factory, translates kube objects into
// the wire format the existing collector expects, and POSTs them to
// /v1/k8s/discovery on the backend.
//
// Wire envelope:
//
//	{
//	  "type":           "service" | "node" | "job" | "namespace" | "status",
//	  "data":           [ ...resources... ],
//	  "full_load":      bool,
//	  "batch_id":       string,
//	  "batch_sequence": int,
//	  "total_batches":  int,
//	  "is_first_batch": bool,
//	  "is_last_batch":  bool,
//	  "metadata":       { ... }
//	}
//
// `tenant` and `cloud_account_id` are NOT included in the agent's payload —
// the collector reads them from auth headers and injects them server-side.
package discovery

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Type is the discovery resource bucket the collector handles.
type Type string

const (
	TypeService     Type = "service" // Pods + workload kinds (Deployment/StatefulSet/DaemonSet/ReplicaSet/Rollout/DeploymentConfig)
	TypeNode        Type = "node"
	TypeJob         Type = "job" // Jobs + CronJobs
	TypeNamespace   Type = "namespace"
	TypeHelmRelease Type = "helm_release"
	// TypeAlertRules carries Prometheus alert rules (api/v1/rules + PrometheusRule
	// CRDs) to the collector's alert_rules_handler, which UPSERTs into the
	// `event_rules` table.
	TypeAlertRules Type = "alert_rules"
)

// Envelope is what gets POSTed to /v1/k8s/discovery.
//
// Data is `any` rather than `[]any` so resource types whose collector
// handlers expect a dict (e.g. alert_rules, where `rules.get("api_based_rules")`
// requires a Mapping, not a list) can pass the payload through unwrapped.
// Resource types that batch items (service / pod / node / namespace /
// helm_release) continue to set Data to a []any slice as before; JSON
// encoding handles either.
type Envelope struct {
	Type          Type           `json:"type"`
	Data          any            `json:"data"`
	FullLoad      bool           `json:"full_load"`
	BatchID       string         `json:"batch_id,omitempty"`
	BatchSequence int            `json:"batch_sequence,omitempty"`
	TotalBatches  int            `json:"total_batches,omitempty"`
	IsFirstBatch  bool           `json:"is_first_batch,omitempty"`
	IsLastBatch   bool           `json:"is_last_batch,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// Sink POSTs Envelope JSON to the backend. Concurrent-safe via the underlying
// http.Client; one Sink per agent process.
type Sink struct {
	URL        string // backend base URL, e.g. https://api.nudgebee.com
	AuthSecret string // sent as Basic-Auth, same as relay
	AccountID  string // X-NB-Account-Id header
	Cluster    string // X-NB-Cluster header
	HTTP       *http.Client
	Logger     *slog.Logger
	// Metrics, when set, records the outcome of every Post. nil disables
	// recording (tests, and any caller without a registry).
	Metrics SinkMetrics
}

// SinkMetrics records discovery POST outcomes. *metrics.Registry satisfies it.
// Declared here rather than importing pkg/metrics so the dependency points the
// same way as dispatch.Metrics.
type SinkMetrics interface {
	OnDiscoveryPost(typ string, fullLoad bool)
	OnDiscoveryError(typ string)
}

func NewSink(backendURL, authSecret, accountID, cluster string, logger *slog.Logger) *Sink {
	if logger == nil {
		logger = slog.Default()
	}
	return &Sink{
		URL:        strings.TrimRight(backendURL, "/"),
		AuthSecret: authSecret,
		AccountID:  accountID,
		Cluster:    cluster,
		HTTP:       &http.Client{Timeout: 60 * time.Second},
		Logger:     logger,
	}
}

// postBackoff is the wait before each retry after the first attempt, so a
// delivery gets 4 tries over ~22s before it is given up on.
//
// Sized against what it is for: the backend publishes every discovery payload
// to RabbitMQ inline, so a broker restart or reschedule makes it answer 503 for
// as long as the broker is away. Before this retry existed the payload was
// simply dropped and the cluster's inventory went stale until the next resync
// (DISCOVERY_RESYNC, 30m by default).
//
// It is deliberately short of covering a long outage. These posts run on the
// per-type worker loops, and a goroutine parked for minutes stops that type's
// deltas flowing for just as long — the same staleness by another route.
// Rolling restarts and pod reschedules are the case this catches; a
// multi-minute broker outage still falls through to the next resync.
var postBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

// Post sends one envelope, retrying while the backend reports a condition that
// is its own rather than ours. Body is gzipped if larger than 16 KB.
//
// Every exit path is metered, so discovery_posts_total / discovery_errors_total
// account for the whole POST — marshal and gzip failures included, not just the
// HTTP call. One Post is one metric sample however many attempts it took.
func (s *Sink) Post(ctx context.Context, env *Envelope) (err error) {
	if s.Metrics != nil {
		defer func() {
			if err != nil {
				s.Metrics.OnDiscoveryError(string(env.Type))
				return
			}
			s.Metrics.OnDiscoveryPost(string(env.Type), env.FullLoad)
		}()
	}

	if s.URL == "" {
		return errors.New("discovery: backend URL not configured")
	}

	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	var contentEncoding string
	if len(body) > 16<<10 {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(body); err != nil {
			return fmt.Errorf("gzip write: %w", err)
		}
		if err := gw.Close(); err != nil {
			return fmt.Errorf("gzip close: %w", err)
		}
		// Reassigned rather than streamed from the buffer: every attempt needs
		// its own reader over the same bytes, and a consumed bytes.Buffer would
		// send an empty body on the retry.
		body = buf.Bytes()
		contentEncoding = "gzip"
	}

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(postBackoff[attempt-1]):
			}
		}

		retriable, attemptErr := s.post(ctx, body, contentEncoding)
		if attemptErr == nil {
			err = nil
			break
		}
		err = attemptErr
		if !retriable || attempt >= len(postBackoff) {
			return err
		}
		s.Logger.Warn("discovery post failed, retrying",
			"type", env.Type, "attempt", attempt+1, "retry_in", postBackoff[attempt], "err", attemptErr,
		)
	}
	s.Logger.Debug("discovery posted",
		"type", env.Type, "items", envelopeItemCount(env.Data),
		"full_load", env.FullLoad, "bytes", len(body), "encoding", contentEncoding,
	)
	return nil
}

// post makes one attempt. The bool reports whether retrying could plausibly
// succeed: a transport error or a backend that says it is unavailable, but not
// a 4xx — bad credentials or a malformed envelope fail identically every time,
// and retrying them just adds load to an already-unhappy backend.
func (s *Sink) post(ctx context.Context, body []byte, contentEncoding string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/k8s/discovery", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	if s.AuthSecret != "" {
		req.Header.Set("Authorization", basicAuth(s.AuthSecret))
	}
	if s.AccountID != "" {
		req.Header.Set("X-NB-Account-Id", s.AccountID)
	}
	if s.Cluster != "" {
		req.Header.Set("X-NB-Cluster", s.Cluster)
	}

	resp, err := s.HTTP.Do(req)
	if err != nil {
		// Don't retry a cancelled or timed-out context: the caller is shutting
		// down, or its own deadline is already spent.
		return ctx.Err() == nil, fmt.Errorf("post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return isRetriableStatus(resp.StatusCode), fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return false, nil
}

// isRetriableStatus reports whether the backend is telling us to come back
// later rather than telling us we are wrong.
//
// 503 is the one that matters in practice: the collector answers it when its
// message broker is unreachable. 502/504 cover an ingress that lost its
// upstream mid-restart, and 429 is a rate limit, which is a wait by definition.
// A bare 500 is excluded on purpose — it means the collector hit something it
// did not expect, and replaying the same payload into that is not a fix.
func isRetriableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// envelopeItemCount returns the array length for list-shaped Data
// (services / pods / …) and 1 for dict-shaped Data (alert_rules); 0
// when nil. Diagnostic-only, doesn't affect the wire payload.
func envelopeItemCount(d any) int {
	if d == nil {
		return 0
	}
	if arr, ok := d.([]any); ok {
		return len(arr)
	}
	return 1
}
