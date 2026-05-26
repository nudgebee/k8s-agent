// Package elasticsearch is a thin HTTP wrapper for the ES query primitives.
// Same shape as pkg/observability/prometheus.
//
// Action surface:
//   - query_es                    : POST {index}/_search with DSL, or {index}/_plugins/_ppl with PPL
//   - query_es_indices            : GET _cat/indices?format=json
//   - query_es_index_field        : GET {index}/_mapping
//   - query_es_field_index_values : terms aggregation on {field}
package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	BaseURL      string
	HTTP         *http.Client
	ExtraHeaders http.Header
	Username     string // optional Basic-Auth
	Password     string
	APIKey       string // optional ApiKey header
}

func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: httpClient}
}

// Search runs a DSL or PPL query.
//
//	queryType = "dsl" | "ppl" (default dsl)
//	query     = for dsl: full search body as map; for ppl: query string
func (c *Client) Search(ctx context.Context, index, queryType string, query any) (json.RawMessage, error) {
	if index == "" {
		return nil, errors.New("elasticsearch: index is required")
	}
	if queryType == "" {
		queryType = "dsl"
	}
	switch queryType {
	case "dsl":
		body, err := json.Marshal(query)
		if err != nil {
			return nil, fmt.Errorf("marshal dsl: %w", err)
		}
		return c.do(ctx, http.MethodPost, "/"+index+"/_search", body, "application/json")
	case "ppl":
		s, _ := query.(string)
		body, _ := json.Marshal(map[string]any{"query": s})
		return c.do(ctx, http.MethodPost, "/_plugins/_ppl", body, "application/json")
	default:
		return nil, fmt.Errorf("elasticsearch: unsupported query_type %q", queryType)
	}
}

// Indices returns _cat/indices in JSON form.
func (c *Client) Indices(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/_cat/indices?format=json", nil, "")
}

// IndexFields returns mapping for an index.
func (c *Client) IndexFields(ctx context.Context, index string) (json.RawMessage, error) {
	if index == "" {
		return nil, errors.New("elasticsearch: index is required")
	}
	return c.do(ctx, http.MethodGet, "/"+index+"/_mapping", nil, "")
}

// FieldValues returns distinct values for one field via a terms aggregation.
func (c *Client) FieldValues(ctx context.Context, index, field string, limit int) (json.RawMessage, error) {
	if index == "" || field == "" {
		return nil, errors.New("elasticsearch: index and field required")
	}
	if limit <= 0 {
		limit = 5000
	}
	body := map[string]any{
		"size": 0,
		"aggs": map[string]any{
			"unique_values": map[string]any{
				"terms": map[string]any{"field": field, "size": limit},
			},
		},
	}
	b, _ := json.Marshal(body)
	return c.do(ctx, http.MethodPost, "/"+index+"/_search", b, "application/json")
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, contentType string) (json.RawMessage, error) {
	if c.BaseURL == "" {
		return nil, errors.New("elasticsearch: base URL not configured")
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "ApiKey "+c.APIKey)
	} else if c.Username != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	for k, vv := range c.ExtraHeaders {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("elasticsearch %s: HTTP %d: %s", path, resp.StatusCode, string(respBody))
	}
	return json.RawMessage(respBody), nil
}
