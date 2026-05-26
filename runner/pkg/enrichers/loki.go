package enrichers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/loki"
)

// LokiCompat implements `query_loki` / `query_loki_labels` /
// `query_grafana_loki_label_values` / `query_grafana_loki_series` actions.
//
// These differ from the agent's existing thin loki_* primitives in two ways:
//
//  1. The action_params shape is flat:
//     query_loki           → params.query is a raw URL query string
//     appended verbatim after `?`
//     query_loki_labels    → same pattern
//     query_grafana_loki_label_values → params.{label, query}
//     query_grafana_loki_series       → params is a dict of GET params
//
//  2. The wire response is `{success, data: <raw JSON or string>}`, NOT a
//     Finding.
type LokiCompat struct {
	c *loki.Client
}

func NewLokiCompat(c *loki.Client) *LokiCompat { return &LokiCompat{c: c} }

// Handlers returns the handler map.
func (l *LokiCompat) Handlers() map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"query_loki":                      l.queryLoki,
		"query_loki_labels":               l.queryLokiLabels,
		"query_grafana_loki_label_values": l.queryLokiLabelValues,
		"query_grafana_loki_series":       l.queryLokiSeries,
	}
}

// queryLoki mirrors the backend — `params.query` is the
// already-formed URL query string (e.g. "query={...}&start=...&end=..."), and
// we GET /loki/api/v1/query_range?<that string>.
func (l *LokiCompat) queryLoki(ctx context.Context, params map[string]any) (any, error) {
	q, _ := params["query"].(string)
	body, err := l.rawGet(ctx, "/loki/api/v1/query_range?"+strings.TrimPrefix(q, "?"))
	return wrapData(body, err), nil
}

func (l *LokiCompat) queryLokiLabels(ctx context.Context, params map[string]any) (any, error) {
	q, _ := params["query"].(string)
	path := "/loki/api/v1/labels"
	if q != "" {
		path += "?" + strings.TrimPrefix(q, "?")
	}
	body, err := l.rawGet(ctx, path)
	return wrapData(body, err), nil
}

func (l *LokiCompat) queryLokiLabelValues(ctx context.Context, params map[string]any) (any, error) {
	label, _ := params["label"].(string)
	q, _ := params["query"].(string)
	if label == "" {
		return map[string]any{
			"success": false,
			"data":    map[string]any{"error": "label is required"},
		}, nil
	}
	path := "/loki/api/v1/label/" + url.PathEscape(label) + "/values"
	if q != "" {
		path += "?" + strings.TrimPrefix(q, "?")
	}
	body, err := l.rawGet(ctx, path)
	return wrapData(body, err), nil
}

func (l *LokiCompat) queryLokiSeries(ctx context.Context, params map[string]any) (any, error) {
	v := url.Values{}
	for k, val := range params {
		switch x := val.(type) {
		case string:
			v.Set(k, x)
		case []any:
			for _, item := range x {
				if s, ok := item.(string); ok {
					v.Add(k, s)
				}
			}
		case []string:
			for _, s := range x {
				v.Add(k, s)
			}
		}
	}
	path := "/loki/api/v1/series"
	if enc := v.Encode(); enc != "" {
		path += "?" + enc
	}
	body, err := l.rawGet(ctx, path)
	return wrapData(body, err), nil
}

// rawGet talks to Loki via the existing client's BaseURL + ExtraHeaders, but
// without the `loki.Client` wrappers' typed-param handling. We need raw bytes
// because Loki returns either JSON (200) or text (non-200), and the
// api-server caller passes either through as-is.
func (l *LokiCompat) rawGet(ctx context.Context, path string) ([]byte, error) {
	if l.c == nil || l.c.BaseURL == "" {
		return nil, fmt.Errorf("loki: not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	for k, vv := range l.c.ExtraHeaders {
		for _, v := range vv {
			req.Header.Add(k, v)
		}
	}
	resp, err := l.c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		// Loki returns the body as text on non-200. We surface as
		// `data: "<raw text>"` rather than an error so the wire shape stays
		// uniform.
		return body, nil
	}
	return body, nil
}

// wrapData mirrors the backend's response shape — `data` is the parsed JSON
// when the body is valid JSON, or a string when Loki returned plain text.
func wrapData(body []byte, err error) map[string]any {
	if err != nil {
		return map[string]any{
			"success": false,
			"data":    map[string]any{"error": err.Error()},
		}
	}
	if len(body) == 0 {
		return map[string]any{"success": true, "data": nil}
	}
	var parsed any
	if json.Unmarshal(body, &parsed) == nil {
		return map[string]any{"success": true, "data": parsed}
	}
	return map[string]any{"success": true, "data": string(body)}
}
