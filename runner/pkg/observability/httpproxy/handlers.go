package httpproxy

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Handlers wires the http_proxy_request action.
func Handlers(c *Client) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"http_proxy_request": func(ctx context.Context, p map[string]any) (any, error) {
			return c.Do(ctx, parseRequest(p))
		},
	}
}

func parseRequest(p map[string]any) *Request {
	r := &Request{
		Target: str(p, "target"),
		Method: str(p, "method"),
		Path:   str(p, "path"),
	}
	// Headers: map[string]string from action_params.
	if h, ok := p["headers"].(map[string]any); ok {
		r.Headers = make(map[string]string, len(h))
		for k, v := range h {
			if s, ok := v.(string); ok {
				r.Headers[k] = s
			}
		}
	}
	// Query: map[string]string|[]string.
	if q, ok := p["query"].(map[string]any); ok {
		r.Query = url.Values{}
		for k, v := range q {
			switch x := v.(type) {
			case string:
				r.Query.Set(k, x)
			case []any:
				for _, s := range x {
					if str, ok := s.(string); ok {
						r.Query.Add(k, str)
					}
				}
			}
		}
	}
	// Body: any JSON value, re-marshalled.
	if b, ok := p["body"]; ok && b != nil {
		marshaled, err := json.Marshal(b)
		if err == nil {
			r.Body = marshaled
		}
	}
	return r
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}
