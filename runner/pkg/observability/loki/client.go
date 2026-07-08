// Package loki is a thin HTTP wrapper around Grafana Loki's query API.
// Same pattern as pkg/observability/prometheus: agent forwards raw bytes,
// backend parses.
//
// Endpoints (https://grafana.com/docs/loki/latest/reference/loki-http-api/):
//
//	GET  /loki/api/v1/query           — instant LogQL query
//	GET  /loki/api/v1/query_range     — range LogQL query
//	GET  /loki/api/v1/labels          — list of labels
//	GET  /loki/api/v1/label/{name}/values — values for one label
//	GET  /loki/api/v1/series          — series matching a selector
package loki

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
	Username     string // optional Basic-Auth (LOKI_USERNAME)
	Password     string // optional Basic-Auth (LOKI_PASSWORD)
}

func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: httpClient}
}

func (c *Client) Query(ctx context.Context, query, atTime, limit string) (json.RawMessage, error) {
	if query == "" {
		return nil, errors.New("loki: query is required")
	}
	v := url.Values{}
	v.Set("query", query)
	if atTime != "" {
		v.Set("time", atTime)
	}
	if limit != "" {
		v.Set("limit", limit)
	}
	return c.get(ctx, "/loki/api/v1/query", v)
}

func (c *Client) QueryRange(ctx context.Context, query, start, end, step, direction, limit string) (json.RawMessage, error) {
	if query == "" {
		return nil, errors.New("loki: query is required")
	}
	v := url.Values{}
	v.Set("query", query)
	if start != "" {
		v.Set("start", start)
	}
	if end != "" {
		v.Set("end", end)
	}
	if step != "" {
		v.Set("step", step)
	}
	if direction != "" {
		v.Set("direction", direction)
	}
	if limit != "" {
		v.Set("limit", limit)
	}
	return c.get(ctx, "/loki/api/v1/query_range", v)
}

func (c *Client) Labels(ctx context.Context, start, end, query string) (json.RawMessage, error) {
	v := url.Values{}
	if start != "" {
		v.Set("start", start)
	}
	if end != "" {
		v.Set("end", end)
	}
	if query != "" {
		v.Set("query", query)
	}
	return c.get(ctx, "/loki/api/v1/labels", v)
}

func (c *Client) LabelValues(ctx context.Context, label, start, end, query string) (json.RawMessage, error) {
	if label == "" {
		return nil, errors.New("loki: label is required")
	}
	v := url.Values{}
	if start != "" {
		v.Set("start", start)
	}
	if end != "" {
		v.Set("end", end)
	}
	if query != "" {
		v.Set("query", query)
	}
	return c.get(ctx, "/loki/api/v1/label/"+url.PathEscape(label)+"/values", v)
}

func (c *Client) Series(ctx context.Context, matchers []string, start, end string) (json.RawMessage, error) {
	if len(matchers) == 0 {
		return nil, errors.New("loki: at least one matcher is required")
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
	return c.get(ctx, "/loki/api/v1/series", v)
}

func (c *Client) get(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	if c.BaseURL == "" {
		return nil, errors.New("loki: base URL not configured")
	}
	u := c.BaseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// LOKI_USERNAME/LOKI_PASSWORD → Basic-Auth header (legacy get_headers).
	if c.Username != "" && c.Password != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	for k, vv := range c.ExtraHeaders {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loki get %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("loki read %s: %w", path, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("loki %s: HTTP %d: %s", path, resp.StatusCode, string(body))
	}
	return json.RawMessage(body), nil
}
