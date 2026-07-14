// Package pinot is a thin HTTP wrapper for Apache Pinot's controller REST API.
//
// Action surface:
//   - pinot_query  : POST /query/sql (broker) — execute a SQL query,
//     falling back to POST /sql (controller) on a 404
//   - pinot_tables : GET  /tables    — list available tables (controller)
//   - pinot_schema : GET  /schemas/{table} — column layout for a table (controller)
//
// PINOT_URL is expected to point at the Pinot broker; the query path there is
// /query/sql. The /tables and /schemas endpoints are controller-only.
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
//
// The chart directs operators to point PINOT_URL at the Pinot *broker*
// (port 8099), whose query path is /query/sql. The controller (port 9000)
// serves /sql. We POST to /query/sql first (the documented setup) and fall
// back to /sql on a 404, so both broker and controller URLs work without
// the operator having to know the difference.
func (c *Client) Query(ctx context.Context, sql string) (json.RawMessage, error) {
	if sql == "" {
		return nil, errors.New("pinot: sql query is required")
	}
	body, err := json.Marshal(map[string]string{"sql": sql})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	raw, status, err := c.doRaw(ctx, http.MethodPost, "/query/sql", body, "application/json")
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		// PINOT_URL points at the controller — retry the controller path.
		raw, status, err = c.doRaw(ctx, http.MethodPost, "/sql", body, "application/json")
		if err != nil {
			return nil, err
		}
	}
	if status >= 400 {
		return nil, fmt.Errorf("pinot query: HTTP %d: %s", status, string(raw))
	}
	return raw, nil
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

// do issues a request and treats any HTTP >= 400 as an error. Used by the
// table/schema helpers where a single fixed path is expected.
func (c *Client) do(ctx context.Context, method, path string, body []byte, contentType string) (json.RawMessage, error) {
	raw, status, err := c.doRaw(ctx, method, path, body, contentType)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("pinot %s: HTTP %d: %s", path, status, string(raw))
	}
	return raw, nil
}

// doRaw issues a request and returns the body + HTTP status without treating
// a 4xx as an error, so callers (Query) can act on a 404 to retry an
// alternate path. err is non-nil only for transport/read failures.
func (c *Client) doRaw(ctx context.Context, method, path string, body []byte, contentType string) (json.RawMessage, int, error) {
	if c.BaseURL == "" {
		return nil, 0, errors.New("pinot: base URL not configured")
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return nil, 0, err
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
		return nil, 0, fmt.Errorf("pinot %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return json.RawMessage(respBody), resp.StatusCode, nil
}
