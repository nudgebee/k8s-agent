package mutate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Loki rules HTTP API (https://grafana.com/docs/loki/latest/operations/recording-rules/):
//
//	POST   /loki/api/v1/rules/{namespace}     — replace a namespace's rule groups
//	GET    /loki/api/v1/rules/{namespace}/{group}
//	DELETE /loki/api/v1/rules/{namespace}/{group}
//
// These are called via HTTP from the agent. Body for POST is YAML;
// the action_params is forwarded raw.

// CreateOrReplaceLokiAlertRule POSTs the rule group YAML to Loki.
//
//	namespace : Loki tenant/namespace (path param)
//	body      : raw YAML body (string)
func (m *Mutator) CreateOrReplaceLokiAlertRule(ctx context.Context, namespace, body string) ([]byte, error) {
	if m.LokiRulesURL == "" {
		return nil, errors.New("mutate: LokiRulesURL not configured")
	}
	if namespace == "" || body == "" {
		return nil, errors.New("mutate: namespace and body required")
	}
	u := strings.TrimRight(m.LokiRulesURL, "/") + "/loki/api/v1/rules/" + url.PathEscape(namespace)
	return m.lokiDo(ctx, http.MethodPost, u, "application/yaml", []byte(body))
}

// DeleteLokiAlertRule removes one rule group within a namespace.
func (m *Mutator) DeleteLokiAlertRule(ctx context.Context, namespace, group string) ([]byte, error) {
	if m.LokiRulesURL == "" {
		return nil, errors.New("mutate: LokiRulesURL not configured")
	}
	if namespace == "" || group == "" {
		return nil, errors.New("mutate: namespace and group required")
	}
	u := strings.TrimRight(m.LokiRulesURL, "/") + "/loki/api/v1/rules/" +
		url.PathEscape(namespace) + "/" + url.PathEscape(group)
	return m.lokiDo(ctx, http.MethodDelete, u, "", nil)
}

func (m *Mutator) lokiDo(ctx context.Context, method, u, contentType string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range m.LokiRulesHeaders {
		req.Header.Set(k, v)
	}
	cli := &http.Client{Timeout: 30 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loki rules: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("loki rules HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}
