// Package signoz is a thin HTTP wrapper for Signoz's query API.
//
// Action surface:
//   - signoz_query_range   : POST /api/v3/query_range
//   - signoz_label_suggest : GET  /api/v3/autocomplete/attribute_keys
//   - signoz_value_suggest : GET  /api/v3/autocomplete/attribute_values
//
// query_range forwards the action_params JSON as the request body. The
// autocomplete suggests take their params as URL query string (Signoz serves
// those endpoints via GET). All return the raw response; backend composes.
package signoz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	BaseURL      string
	APIKey       string
	User         string
	Password     string
	HTTP         *http.Client
	ExtraHeaders http.Header

	// jwt caches the access token minted via /api/v1/login when
	// user/password auth is used. Guarded by mu so concurrent action
	// handlers don't each hit the login endpoint.
	mu        sync.Mutex
	jwt       string
	jwtExpiry int64 // unix seconds
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
	return c.get(ctx, "/api/v3/autocomplete/attribute_keys", params)
}

func (c *Client) ValueSuggest(ctx context.Context, params any) (json.RawMessage, error) {
	return c.get(ctx, "/api/v3/autocomplete/attribute_values", params)
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
	return c.do(ctx, req, path)
}

func (c *Client) get(ctx context.Context, path string, params any) (json.RawMessage, error) {
	if c.BaseURL == "" {
		return nil, errors.New("signoz: base URL not configured")
	}
	reqURL := c.BaseURL + path
	if q := toQuery(params); len(q) > 0 {
		reqURL += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, req, path)
}

// do applies auth + extra headers, sends the request, and returns the raw body
// or an error for any non-2xx/3xx status. Shared by post and get.
func (c *Client) do(ctx context.Context, req *http.Request, path string) (json.RawMessage, error) {
	if err := c.applyAuth(ctx, req); err != nil {
		return nil, err
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

// toQuery flattens a JSON-object params payload into URL query values so the
// autocomplete GET endpoints receive dataSource/aggregateOperator/searchText/etc.
// Nested arrays/objects are JSON-encoded; scalars are stringified.
func toQuery(params any) url.Values {
	if params == nil {
		return nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	q := url.Values{}
	for k, v := range m {
		switch val := v.(type) {
		case nil:
			// skip nulls
		case string:
			q.Set(k, val)
		case bool:
			q.Set(k, strconv.FormatBool(val))
		case float64:
			if val == float64(int64(val)) {
				q.Set(k, strconv.FormatInt(int64(val), 10))
			} else {
				q.Set(k, strconv.FormatFloat(val, 'f', -1, 64))
			}
		default:
			if jb, err := json.Marshal(val); err == nil {
				q.Set(k, string(jb))
			}
		}
	}
	return q
}

// applyAuth sets the auth header on req. Prefers the API key when set,
// otherwise mints (and caches) a JWT via user/password login. Mirrors the
// legacy Python SignozClient.headers precedence.
func (c *Client) applyAuth(ctx context.Context, req *http.Request) error {
	if c.APIKey != "" {
		req.Header.Set("SIGNOZ-API-KEY", c.APIKey)
		return nil
	}
	if c.User != "" && c.Password != "" {
		token, err := c.jwtToken(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// loginResponse is the /api/v1/login payload shape.
type loginResponse struct {
	AccessJWT       string `json:"accessJwt"`
	AccessJWTExpiry int64  `json:"accessJwtExpiry"` // unix seconds
}

// jwtToken returns a cached JWT if it's still valid (with a 60s skew),
// otherwise logs in against /api/v1/login to mint a fresh one.
func (c *Client) jwtToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jwt != "" && c.jwtExpiry > time.Now().Unix()+60 {
		return c.jwt, nil
	}
	body, err := json.Marshal(map[string]string{"email": c.User, "password": c.Password})
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("signoz login: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("signoz login: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var lr loginResponse
	if err := json.Unmarshal(respBody, &lr); err != nil {
		return "", fmt.Errorf("signoz login: decode: %w", err)
	}
	if lr.AccessJWT == "" {
		return "", errors.New("signoz login: empty access token")
	}
	c.jwt = lr.AccessJWT
	c.jwtExpiry = lr.AccessJWTExpiry
	return c.jwt, nil
}
