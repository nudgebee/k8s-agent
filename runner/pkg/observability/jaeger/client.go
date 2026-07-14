// Package jaeger is a thin HTTP wrapper for Jaeger's query API.
//
// Action surface:
//   - jaeger_query_traces       : GET /api/traces
//   - jaeger_query_services     : GET /api/services
//   - jaeger_query_trace_by_id  : GET /api/traces/{id}
//   - jaeger_query_operations   : GET /api/services/{service}/operations
//   - jaeger_query_metrics      : fan-out to /api/metrics/{calls,errors,
//     latencies?quantile=0.95,latencies?quantile=0.99} (Jaeger SPM)
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
	Token        string // optional Bearer token (JAEGER_TOKEN)
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

// Metrics queries Jaeger SPM (Service Performance Monitoring) metrics. It
// replicates the legacy get_metrics: the backend composer sends `services`
// and `spanKinds` (plural) plus a time window and NO metric_type. We remap
// those to Jaeger's singular `service` / `spanKind` query params and fan out
// to the four SPM endpoints, assembling a single object the backend parser
// reads: {calls, errors, latencies_p95, latencies_p99}.
func (c *Client) Metrics(ctx context.Context, params map[string]any) (json.RawMessage, error) {
	base := metricsQuery(params)

	subs := []struct {
		key      string
		metric   string
		quantile string
	}{
		{"calls", "calls", ""},
		{"errors", "errors", ""},
		{"latencies_p95", "latencies", "0.95"},
		{"latencies_p99", "latencies", "0.99"},
	}

	out := make(map[string]json.RawMessage, len(subs))
	for _, s := range subs {
		q := cloneValues(base)
		if s.quantile != "" {
			q.Set("quantile", s.quantile)
		}
		raw, status, err := c.getRaw(ctx, "/api/metrics/"+s.metric, q)
		if err != nil {
			return nil, err
		}
		if status == http.StatusNotFound {
			// Jaeger 404s /api/metrics/* when SPM isn't wired up (no
			// monitor/OTel metrics storage). Preserve the legacy friendly
			// message instead of leaking a raw 404.
			return nil, fmt.Errorf("jaeger: SPM metrics not available (monitoring storage not configured)")
		}
		if status >= 400 {
			return nil, fmt.Errorf("jaeger metrics %s: HTTP %d: %s", s.metric, status, string(raw))
		}
		out[s.key] = raw
	}
	return json.Marshal(out)
}

// metricsQuery builds the shared SPM query params, remapping the composer's
// plural `services`/`spanKinds` to Jaeger's singular `service`/`spanKind` and
// dropping any legacy `metric_type` (the fan-out covers all four metrics).
func metricsQuery(params map[string]any) url.Values {
	remapped := make(map[string]any, len(params))
	for k, v := range params {
		switch k {
		case "services", "service":
			remapped["service"] = v
		case "spanKinds", "spanKind":
			remapped["spanKind"] = v
		case "metric_type":
			// dropped
		default:
			remapped[k] = v
		}
	}
	return paramsToQuery(remapped)
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
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

// get issues a request and treats HTTP >= 400 as an error.
func (c *Client) get(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	raw, status, err := c.getRaw(ctx, path, params)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("jaeger %s: HTTP %d: %s", path, status, string(raw))
	}
	return raw, nil
}

// getRaw issues a request and returns the body + HTTP status without treating
// a 4xx as an error, so callers (Metrics) can act on a 404. err is non-nil
// only for transport/read failures.
func (c *Client) getRaw(ctx context.Context, path string, params url.Values) (json.RawMessage, int, error) {
	if c.BaseURL == "" {
		return nil, 0, errors.New("jaeger: base URL not configured")
	}
	u := c.BaseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for k, vv := range c.ExtraHeaders {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("jaeger get %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return json.RawMessage(body), resp.StatusCode, nil
}
