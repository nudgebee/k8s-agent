package jaeger

import (
	"context"
	"encoding/json"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// Handlers wires Jaeger primitive actions.
func Handlers(c *Client) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"jaeger_query_traces":      wrap(c, handleTraces),
		"jaeger_query_services":    wrap(c, handleServices),
		"jaeger_query_trace_by_id": wrap(c, handleTraceByID),
		"jaeger_query_operations":  wrap(c, handleOperations),
		"jaeger_query_metrics":     wrap(c, handleMetrics),
	}
}

func wrap(c *Client, fn func(context.Context, *Client, map[string]any) (json.RawMessage, error)) dispatch.Handler {
	return func(ctx context.Context, params map[string]any) (any, error) {
		raw, err := fn(ctx, c, params)
		if err != nil {
			return nil, err
		}
		return raw, nil
	}
}

func handleTraces(ctx context.Context, c *Client, p map[string]any) (json.RawMessage, error) {
	return c.Traces(ctx, p)
}

func handleServices(ctx context.Context, c *Client, _ map[string]any) (json.RawMessage, error) {
	return c.Services(ctx)
}

func handleTraceByID(ctx context.Context, c *Client, p map[string]any) (json.RawMessage, error) {
	id, _ := p["trace_id"].(string)
	return c.TraceByID(ctx, id)
}

func handleOperations(ctx context.Context, c *Client, p map[string]any) (json.RawMessage, error) {
	svc, _ := p["service"].(string)
	return c.Operations(ctx, svc)
}

func handleMetrics(ctx context.Context, c *Client, p map[string]any) (json.RawMessage, error) {
	metricType, _ := p["metric_type"].(string)
	rest := map[string]any{}
	for k, v := range p {
		if k == "metric_type" {
			continue
		}
		rest[k] = v
	}
	return c.Metrics(ctx, metricType, rest)
}
