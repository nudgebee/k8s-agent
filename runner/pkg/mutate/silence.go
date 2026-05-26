package mutate

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

// AlertManager silence CRUD. The agent only proxies HTTP requests; it does
// not parse silence payloads. Caller passes through arbitrary JSON.
//
// Endpoints (AlertManager API v2):
//   GET    /api/v2/silences              — list
//   POST   /api/v2/silences              — create or update
//   DELETE /api/v2/silence/{id}          — delete

// GetSilences returns the raw AlertManager silences list as JSON bytes.
// Optional filter strings get passed through as `filter=...` query params.
func (m *Mutator) GetSilences(ctx context.Context, filters []string) ([]byte, error) {
	if m.AlertManagerURL == "" {
		return nil, errors.New("mutate: AlertManager URL not configured")
	}
	q := url.Values{}
	for _, f := range filters {
		q.Add("filter", f)
	}
	u := strings.TrimRight(m.AlertManagerURL, "/") + "/api/v2/silences"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return m.amDo(ctx, http.MethodGet, u, nil)
}

// AddSilence creates a new silence. Body is the AlertManager Silence JSON
// object — we don't validate the schema, just forward.
func (m *Mutator) AddSilence(ctx context.Context, body []byte) ([]byte, error) {
	if m.AlertManagerURL == "" {
		return nil, errors.New("mutate: AlertManager URL not configured")
	}
	if len(body) == 0 {
		return nil, errors.New("mutate: silence body required")
	}
	u := strings.TrimRight(m.AlertManagerURL, "/") + "/api/v2/silences"
	return m.amDo(ctx, http.MethodPost, u, body)
}

// DeleteSilence cancels a silence by ID.
func (m *Mutator) DeleteSilence(ctx context.Context, id string) ([]byte, error) {
	if m.AlertManagerURL == "" {
		return nil, errors.New("mutate: AlertManager URL not configured")
	}
	if id == "" {
		return nil, errors.New("mutate: silence id required")
	}
	u := strings.TrimRight(m.AlertManagerURL, "/") + "/api/v2/silence/" + url.PathEscape(id)
	return m.amDo(ctx, http.MethodDelete, u, nil)
}

func (m *Mutator) amDo(ctx context.Context, method, u string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range m.AlertManagerHeaders {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("alertmanager %s %s: %w", method, u, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("alertmanager HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// ParseSilenceBody is a small helper for handlers that receive the silence
// payload as a generic map (action_params). It re-marshals to JSON so the
// proxy stays format-stable.
func ParseSilenceBody(p map[string]any) ([]byte, error) {
	if body, ok := p["body"]; ok {
		return json.Marshal(body)
	}
	// Some callers pass the silence fields directly at the top level.
	return json.Marshal(p)
}
