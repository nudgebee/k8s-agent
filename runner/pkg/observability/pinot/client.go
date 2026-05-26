// Package pinot is a thin HTTP wrapper for Apache Pinot's controller REST API.
//
// Action surface:
//   - pinot_query  : POST /query/sql — execute a SQL query
//   - pinot_tables : GET  /tables    — list available tables
//   - pinot_schema : GET  /schemas/{table} — column layout for a table
//
// All methods forward results as raw JSON bytes; backend composes higher-level logic.
package pinot

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
	AuthToken    string // optional Bearer token
	Username     string // optional Basic-Auth
	Password     string
	HTTP         *http.Client
	ExtraHeaders http.Header
}

func New(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: httpClient}
}

// Query executes a SQL query.
// Controller (port 9000) uses /sql; broker (port 8099) uses /query/sql.
// Default targets the controller path — set PINOT_URL to the broker if needed.
func (c *Client) Query(ctx context.Context, sql string) (json.RawMessage, error) {
	if sql == "" {
		return nil, errors.New("pinot: sql query is required")
	}
	body, err := json.Marshal(map[string]string{"sql": sql})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return c.do(ctx, http.MethodPost, "/sql", body, "application/json")
}

// Tables returns the list of available Pinot tables.
func (c *Client) Tables(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/tables", nil, "")
}

// Schema returns the column schema for the given table.
func (c *Client) Schema(ctx context.Context, table string) (json.RawMessage, error) {
	if table == "" {
		return nil, errors.New("pinot: table name is required")
	}
	return c.do(ctx, http.MethodGet, "/schemas/"+table, nil, "")
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, contentType string) (json.RawMessage, error) {
	if c.BaseURL == "" {
		return nil, errors.New("pinot: base URL not configured")
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
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
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
		return nil, fmt.Errorf("pinot %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("pinot %s: HTTP %d: %s", path, resp.StatusCode, string(respBody))
	}
	return json.RawMessage(respBody), nil
}
