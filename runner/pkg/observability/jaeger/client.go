// Package jaeger is a thin HTTP wrapper for Jaeger's query API.
//
// Action surface:
//   - jaeger_query_traces       : GET /api/traces
//   - jaeger_query_services     : GET /api/services
//   - jaeger_query_trace_by_id  : GET /api/traces/{id}
//   - jaeger_query_operations   : GET /api/services/{service}/operations
//   - jaeger_query_metrics      : GET /api/metrics/{type} (Jaeger SPM)
package jaeger

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

type Client struct {
	BaseURL      string
	HTTP         *http.Client
	ExtraHeaders http.Header
}

func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: httpClient}
}

// Traces searches traces. params is forwarded as the query string; common
// keys are service, operation, start, end, limit, tags.
func (c *Client) Traces(ctx context.Context, params map[string]any) (json.RawMessage, error) {
	v := paramsToQuery(params)
	return c.get(ctx, "/api/traces", v)
}

// Services lists known services.
func (c *Client) Services(ctx context.Context) (json.RawMessage, error) {
	return c.get(ctx, "/api/services", nil)
}

// TraceByID fetches a single trace.
func (c *Client) TraceByID(ctx context.Context, id string) (json.RawMessage, error) {
	if id == "" {
		return nil, errors.New("jaeger: trace id required")
	}
	return c.get(ctx, "/api/traces/"+url.PathEscape(id), nil)
}

// Operations lists operations for a service.
func (c *Client) Operations(ctx context.Context, service string) (json.RawMessage, error) {
	if service == "" {
		return nil, errors.New("jaeger: service required")
	}
	return c.get(ctx, "/api/services/"+url.PathEscape(service)+"/operations", nil)
}

// Metrics queries Jaeger SPM metrics. metricType is one of latencies,
// call_rates, error_rates, min_step.
func (c *Client) Metrics(ctx context.Context, metricType string, params map[string]any) (json.RawMessage, error) {
	if metricType == "" {
		return nil, errors.New("jaeger: metric_type required")
	}
	return c.get(ctx, "/api/metrics/"+url.PathEscape(metricType), paramsToQuery(params))
}

func paramsToQuery(params map[string]any) url.Values {
	v := url.Values{}
	for k, val := range params {
		switch x := val.(type) {
		case string:
			if x != "" {
				v.Set(k, x)
			}
		case []string:
			for _, s := range x {
				v.Add(k, s)
			}
		case []any:
			for _, s := range x {
				if str, ok := s.(string); ok {
					v.Add(k, str)
				}
			}
		case float64:
			v.Set(k, fmt.Sprintf("%g", x))
		case int:
			v.Set(k, fmt.Sprintf("%d", x))
		case bool:
			v.Set(k, fmt.Sprintf("%t", x))
		}
	}
	return v
}

func (c *Client) get(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	if c.BaseURL == "" {
		return nil, errors.New("jaeger: base URL not configured")
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
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jaeger get %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("jaeger %s: HTTP %d: %s", path, resp.StatusCode, string(body))
	}
	return json.RawMessage(body), nil
}
