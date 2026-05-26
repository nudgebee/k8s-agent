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

// Post sends one envelope. Body is gzipped if larger than 16 KB.
func (s *Sink) Post(ctx context.Context, env *Envelope) error {
	if s.URL == "" {
		return errors.New("discovery: backend URL not configured")
	}

	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	var (
		reqBody         io.Reader = bytes.NewReader(body)
		contentEncoding string
		bodyLen         = len(body)
	)
	if bodyLen > 16<<10 {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(body); err != nil {
			return fmt.Errorf("gzip write: %w", err)
		}
		if err := gw.Close(); err != nil {
			return fmt.Errorf("gzip close: %w", err)
		}
		reqBody = &buf
		contentEncoding = "gzip"
		bodyLen = buf.Len()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/k8s/discovery", reqBody)
	if err != nil {
		return err
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
		return fmt.Errorf("post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	s.Logger.Debug("discovery posted",
		"type", env.Type, "items", envelopeItemCount(env.Data),
		"full_load", env.FullLoad, "bytes", bodyLen, "encoding", contentEncoding,
	)
	return nil
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
