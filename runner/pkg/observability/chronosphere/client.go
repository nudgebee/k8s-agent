// Package chronosphere is a thin HTTP wrapper for Chronosphere's tracing
// search API.
//
// Action surface:
//   - chronosphere_query_traces : POST /api/unstable/data/traces/searches
//
// The action_params is forwarded as the JSON body unchanged.
package chronosphere

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
	BaseURL string
	APIKey  string // sent as Authorization: Bearer <key>
	HTTP    *http.Client
}

func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: httpClient}
}

// QueryTraces forwards params as the search request body.
func (c *Client) QueryTraces(ctx context.Context, params any) (json.RawMessage, error) {
	if c.BaseURL == "" {
		return nil, errors.New("chronosphere: base URL not configured")
	}
	if params == nil {
		return nil, errors.New("chronosphere: params required")
	}
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/unstable/data/traces/searches", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("chronosphere: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("chronosphere HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return json.RawMessage(respBody), nil
}
