// Package control implements the agent's hot-reload primitives. The
// legacy refresh_playbook action reloaded a YAML playbook; this agent
// has no playbook YAML (per the deprecation plan playbooks moved to the
// backend), so refresh_playbook here pulls the action allowlist from the
// backend so operators can add new actions to a running agent without a
// customer Helm upgrade.
//
// Action surface:
//   - refresh_playbook : reload the light-action allowlist from the backend
//
// Design:
//   - Allowlist source is GET <NUDGEBEE_ENDPOINT>/v1/agent/config
//
// returns {"light_actions": ["ping","echo",...]}).
//   - On success, atomically swaps the validator's light-action set.
//   - When the backend URL is unset, refresh_playbook is a no-op that
//     returns {refreshed: false, reason: "no config source"}. The handler
//     stays registered so manual probes still see "ok".
package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/auth"
	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Refresher pulls config from the backend and applies it to the live Validator.
type Refresher struct {
	BackendURL    string // e.g. https://api.nudgebee.com — appended with /v1/agent/config
	AuthSecretKey string // sent as Basic-Auth, same as relay
	AccountID     string
	ClusterName   string
	Validator     *auth.Validator
	HTTP          *http.Client

	// Static defaults — actions that are ALWAYS in the allowlist regardless
	// of what the backend says. ping/echo/health probes belong here.
	StaticActions []string
}

// New returns a Refresher with sensible HTTP defaults.
func New(backendURL, authSecret, accountID, cluster string, v *auth.Validator) *Refresher {
	return &Refresher{
		BackendURL:    strings.TrimRight(backendURL, "/"),
		AuthSecretKey: authSecret,
		AccountID:     accountID,
		ClusterName:   cluster,
		Validator:     v,
		HTTP:          &http.Client{Timeout: 15 * time.Second},
	}
}

// configResponse is the shape we expect from /v1/agent/config. Keep it
// intentionally narrow — the agent only consumes the allowlist for now;
// other config fields can be added without forcing a Helm upgrade.
type configResponse struct {
	LightActions []string `json:"light_actions"`
}

// Refresh fetches the latest config and applies it. Returns a result map
// safe to surface back to the caller.
func (r *Refresher) Refresh(ctx context.Context) (map[string]any, error) {
	if r.Validator == nil {
		return nil, fmt.Errorf("control: validator not configured")
	}
	if r.BackendURL == "" {
		return map[string]any{
			"refreshed": false,
			"reason":    "no config source — NUDGEBEE_ENDPOINT not set",
		}, nil
	}

	url := r.BackendURL + "/v1/agent/config"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if r.AuthSecretKey != "" {
		// Bare base64, no "Basic " prefix — matches the legacy sink format.
		req.Header.Set("Authorization",
			base64.StdEncoding.EncodeToString([]byte(r.AuthSecretKey)))
	}
	if r.AccountID != "" {
		req.Header.Set("X-NB-Account-Id", r.AccountID)
	}
	if r.ClusterName != "" {
		req.Header.Set("X-NB-Cluster", r.ClusterName)
	}

	resp, err := r.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("control: fetch config: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("control: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var cfg configResponse
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("control: parse config: %w", err)
	}

	merged := make(map[string]struct{}, len(cfg.LightActions)+len(r.StaticActions))
	for _, a := range r.StaticActions {
		merged[a] = struct{}{}
	}
	for _, a := range cfg.LightActions {
		merged[a] = struct{}{}
	}
	r.Validator.SetLightActions(merged)

	return map[string]any{
		"refreshed":     true,
		"action_count":  len(merged),
		"backend_count": len(cfg.LightActions),
		"static_count":  len(r.StaticActions),
	}, nil
}

// Handlers wires refresh_playbook into the dispatch registry.
func Handlers(r *Refresher) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"refresh_playbook": func(ctx context.Context, _ map[string]any) (any, error) {
			return r.Refresh(ctx)
		},
	}
}
