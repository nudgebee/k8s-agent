// Package httpproxy is a generic in-cluster HTTP proxy primitive.
//
// Action surface:
//   - http_proxy_request : forward an arbitrary request to a configured base URL
//
// The action takes:
//
//	{target: "<name>" | url, method, path, headers, body, query}
//
// `target` resolves against a per-Client name → base URL map (configured by
// main from values.yaml). This replaces the legacy single-purpose Grafana
// proxy + generic APIProxy with one named-target primitive that handles both.
package httpproxy

import (
	"bytes"
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
	Targets map[string]string // name → base URL
	HTTP    *http.Client
}

func New(targets map[string]string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &Client{Targets: targets, HTTP: httpClient}
}

type Request struct {
	Target  string
	Method  string
	Path    string
	Query   url.Values
	Headers map[string]string
	Body    json.RawMessage
}

type Response struct {
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

func (c *Client) Do(ctx context.Context, r *Request) (*Response, error) {
	baseURL, err := c.resolveTarget(r.Target)
	if err != nil {
		return nil, err
	}
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	u := strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(r.Path, "/")
	if len(r.Query) > 0 {
		u += "?" + r.Query.Encode()
	}

	var body io.Reader
	if len(r.Body) > 0 {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	for k, v := range r.Headers {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("httpproxy: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	for k, v := range resp.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}
	return &Response{StatusCode: resp.StatusCode, Headers: headers, Body: string(respBody)}, nil
}

// resolveTarget allows two forms:
//
//	target: "grafana"            -> looks up Client.Targets["grafana"]
//	target: "https://grafana..."  -> used directly (operator-explicit)
//
// Operator-explicit URLs require Client.Targets to contain "*" or to be empty
// (i.e., explicit-only mode). Without this guard, a misauthenticated request
// could hit any URL the agent can reach.
func (c *Client) resolveTarget(target string) (string, error) {
	if target == "" {
		return "", errors.New("httpproxy: target required")
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		// Allowed only if explicitly enabled via the wildcard target.
		if _, ok := c.Targets["*"]; ok {
			return target, nil
		}
		return "", fmt.Errorf("httpproxy: explicit URL targets are disabled")
	}
	base, ok := c.Targets[target]
	if !ok {
		return "", fmt.Errorf("httpproxy: target %q not configured", target)
	}
	return base, nil
}

// Now also wired below as a higher-timeout retry-allowed default. Note the
// http.Client uses one connection pool across all targets; concurrent calls
// are safe.
var _ = time.Second // keep import for future per-request timeouts
