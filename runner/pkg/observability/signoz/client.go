// Package signoz is a thin HTTP wrapper for Signoz's query API.
//
// Action surface:
//   - signoz_query_range   : POST /api/v3/query_range
//   - signoz_label_suggest : POST /api/v3/autocomplete/attribute_keys
//   - signoz_value_suggest : POST /api/v3/autocomplete/attribute_values
//
// All three forward the action_params JSON as the request body and return
// the raw response. Signoz's body shapes are passthrough; backend composes.
package signoz

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
	APIKey       string
	HTTP         *http.Client
	ExtraHeaders http.Header
}

func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: httpClient}
}

func (c *Client) QueryRange(ctx context.Context, params any) (json.RawMessage, error) {
	return c.post(ctx, "/api/v3/query_range", params)
}

func (c *Client) LabelSuggest(ctx context.Context, params any) (json.RawMessage, error) {
	return c.post(ctx, "/api/v3/autocomplete/attribute_keys", params)
}

func (c *Client) ValueSuggest(ctx context.Context, params any) (json.RawMessage, error) {
	return c.post(ctx, "/api/v3/autocomplete/attribute_values", params)
}

func (c *Client) post(ctx context.Context, path string, params any) (json.RawMessage, error) {
	if c.BaseURL == "" {
		return nil, errors.New("signoz: base URL not configured")
	}
	if params == nil {
		return nil, errors.New("signoz: params required")
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("SIGNOZ-API-KEY", c.APIKey)
	}
	for k, vv := range c.ExtraHeaders {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("signoz %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("signoz %s: HTTP %d: %s", path, resp.StatusCode, string(respBody))
	}
	return json.RawMessage(respBody), nil
}
