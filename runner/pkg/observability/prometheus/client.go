// Package prometheus is a thin HTTP wrapper around the Prometheus query API.
// The agent does not parse, transform, or compose results — it forwards the
// raw response bytes back to the backend, which owns all enrichment logic
// (per plan §2: backend composes via primitives).
//
// Endpoints covered (https://prometheus.io/docs/prometheus/latest/querying/api/):
//
//	GET  /api/v1/query           — instant query
//	GET  /api/v1/query_range     — range query
//	GET  /api/v1/labels          — list of all labels
//	GET  /api/v1/label/{name}/values — values for one label
//	GET  /api/v1/series          — series matching a selector
//	GET  /api/v1/alerts          — active alerts
//
// All responses are returned as raw JSON. The action handler in handlers.go
// shapes the dispatch.Handler signature on top.
package prometheus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a Prometheus HTTP client. One Client per agent process; concurrent
// safe (uses an http.Client which is concurrent-safe).
type Client struct {
	BaseURL string
	HTTP    *http.Client

	// ExtraHeaders are sent on every request. Used for X-Scope-OrgID
	// (Cortex/Mimir multi-tenant) and any auth headers the operator
	// configures via the runner secret today (LOKI_EXTRA_HEADER pattern).
	ExtraHeaders http.Header

	// Auth applies a managed-provider auth scheme (AWS SigV4 / Azure AD /
	// Coralogix) to each request. Nil when the backend uses plain headers
	// or no auth. See auth.go.
	Auth Authorizer
}

// New returns a Client. Pass an empty BaseURL to disable Prometheus access
// (handlers will reject calls).
func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    httpClient,
	}
}

// Query runs an instant query (/api/v1/query).
//
//	query (required): PromQL expression
//	time:             RFC3339 or unix timestamp; empty means "now"
//	timeout:          server-side eval timeout, e.g. "30s"
func (c *Client) Query(ctx context.Context, query, atTime, timeout string) (json.RawMessage, error) {
	if query == "" {
		return nil, errors.New("prometheus: query is required")
	}
	v := url.Values{}
	v.Set("query", query)
	if atTime != "" {
		v.Set("time", atTime)
	}
	if timeout != "" {
		v.Set("timeout", timeout)
	}
	return c.get(ctx, "/api/v1/query", v)
}

// QueryRange runs a range query (/api/v1/query_range).
//
//	query, start, end, step are all required.
func (c *Client) QueryRange(ctx context.Context, query, start, end, step, timeout string) (json.RawMessage, error) {
	if query == "" || start == "" || end == "" || step == "" {
		return nil, errors.New("prometheus: query, start, end, step all required")
	}
	v := url.Values{}
	v.Set("query", query)
	v.Set("start", start)
	v.Set("end", end)
	v.Set("step", step)
	if timeout != "" {
		v.Set("timeout", timeout)
	}
	return c.get(ctx, "/api/v1/query_range", v)
}

// Labels lists all label names (/api/v1/labels). start/end optional.
func (c *Client) Labels(ctx context.Context, start, end string, matchers []string) (json.RawMessage, error) {
	v := url.Values{}
	if start != "" {
		v.Set("start", start)
	}
	if end != "" {
		v.Set("end", end)
	}
	for _, m := range matchers {
		v.Add("match[]", m)
	}
	return c.get(ctx, "/api/v1/labels", v)
}

// LabelValues lists values for one label (/api/v1/label/{name}/values).
func (c *Client) LabelValues(ctx context.Context, label, start, end string, matchers []string) (json.RawMessage, error) {
	if label == "" {
		return nil, errors.New("prometheus: label is required")
	}
	v := url.Values{}
	if start != "" {
		v.Set("start", start)
	}
	if end != "" {
		v.Set("end", end)
	}
	for _, m := range matchers {
		v.Add("match[]", m)
	}
	return c.get(ctx, "/api/v1/label/"+url.PathEscape(label)+"/values", v)
}

// Series lists series matching the selector (/api/v1/series).
func (c *Client) Series(ctx context.Context, matchers []string, start, end string) (json.RawMessage, error) {
	if len(matchers) == 0 {
		return nil, errors.New("prometheus: at least one matcher is required")
	}
	v := url.Values{}
	for _, m := range matchers {
		v.Add("match[]", m)
	}
	if start != "" {
		v.Set("start", start)
	}
	if end != "" {
		v.Set("end", end)
	}
	return c.get(ctx, "/api/v1/series", v)
}

// Alerts returns active alerts (/api/v1/alerts).
func (c *Client) Alerts(ctx context.Context) (json.RawMessage, error) {
	return c.get(ctx, "/api/v1/alerts", nil)
}

// Rules returns all configured alert/recording rule groups
// (/api/v1/rules). Used by pkg/discovery/alertrules to feed the
// collector's `event_rules` table.
//
// Not every Prometheus-compatible backend exposes this endpoint —
// VictoriaMetrics' vmsingle returns 404 (rules live on vmalert). Callers
// should tolerate the resulting `unexpected status 404` and fall back to
// the PrometheusRule CRD path.
func (c *Client) Rules(ctx context.Context) (json.RawMessage, error) {
	return c.get(ctx, "/api/v1/rules", nil)
}

// Flags returns the running-config flags (/api/v1/status/flags). Used by
// the telemetry poster to surface `retentionTime` to the UI so operators
// know how far back they can query metrics. VictoriaMetrics' vmsingle
// returns 404 here, so callers must tolerate that.
func (c *Client) Flags(ctx context.Context) (json.RawMessage, error) {
	return c.get(ctx, "/api/v1/status/flags", nil)
}

func (c *Client) get(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	if c.BaseURL == "" {
		return nil, errors.New("prometheus: base URL not configured")
	}
	u := c.BaseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, vv := range c.ExtraHeaders {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	// Auth is applied last so provider signing (AWS SigV4) covers the final
	// header set, including any ExtraHeaders above.
	if c.Auth != nil {
		if err := c.Auth.Apply(ctx, req); err != nil {
			return nil, fmt.Errorf("prometheus auth: %w", err)
		}
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prometheus get %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("prometheus read %s: %w", path, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("prometheus %s: HTTP %d: %s", path, resp.StatusCode, string(body))
	}
	return json.RawMessage(body), nil
}
