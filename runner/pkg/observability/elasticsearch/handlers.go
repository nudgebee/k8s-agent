package elasticsearch

import (
	"context"
	"encoding/json"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Handlers wires the ES primitive actions.
func Handlers(c *Client) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"query_es":                    wrap(c, handleQuery),
		"query_es_indices":            wrap(c, handleIndices),
		"query_es_index_field":        wrap(c, handleIndexFields),
		"query_es_field_index_values": wrap(c, handleFieldValues),
	}
}

func wrap(c *Client, fn func(ctx context.Context, c *Client, p map[string]any) (json.RawMessage, error)) dispatch.Handler {
	return func(ctx context.Context, params map[string]any) (any, error) {
		raw, err := fn(ctx, c, params)
		if err != nil {
			return nil, err
		}
		return raw, nil
	}
}

func handleQuery(ctx context.Context, c *Client, p map[string]any) (json.RawMessage, error) {
	index := str(p, "index")
	queryType := str(p, "query_type")
	if queryType == "" {
		queryType = "dsl"
	}
	if queryType == "ppl" {
		// PPL params arrive as a single string under "query".
		return c.Search(ctx, index, queryType, str(p, "query"))
	}
	// DSL: full search body lives under "query" or merged at the top level.
	if q, ok := p["query"]; ok {
		// Some callers pass the raw search body; others pass {query: {...}}.
		// The legacy receiver strips one wrapping level — mirror that.
		if m, ok := q.(map[string]any); ok {
			if inner, ok := m["query"]; ok {
				m["query"] = inner
			}
			return c.Search(ctx, index, "dsl", m)
		}
	}
	rest := map[string]any{}
	for k, v := range p {
		if k == "index" || k == "query_type" {
			continue
		}
		rest[k] = v
	}
	return c.Search(ctx, index, "dsl", rest)
}

func handleIndices(ctx context.Context, c *Client, _ map[string]any) (json.RawMessage, error) {
	return c.Indices(ctx)
}

func handleIndexFields(ctx context.Context, c *Client, p map[string]any) (json.RawMessage, error) {
	return c.IndexFields(ctx, str(p, "index"))
}

func handleFieldValues(ctx context.Context, c *Client, p map[string]any) (json.RawMessage, error) {
	limit := 0
	if v, ok := p["limit"].(float64); ok {
		limit = int(v)
	}
	return c.FieldValues(ctx, str(p, "index"), str(p, "field_name"), limit)
}

func str(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}
