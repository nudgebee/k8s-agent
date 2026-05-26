package enrichers

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// PrometheusQueryResultDict converts a raw Prometheus query response (the
// `data` field returned by `/api/v1/query` / `query_range`) into the
// PrometheusQueryResult wire shape consumed by downstream services.
//
// The shape is:
//
//	{
//	  "result_type": "vector"|"matrix"|"scalar"|"string"|"error",
//	  "vector_result":      [{metric: {..}, value: {timestamp, value}}] | null,
//	  "series_list_result": [{metric: {..}, timestamps: [..], values: [..]}] | null,
//	  "scalar_result":      {timestamp, value} | null,
//	  "string_result":      "..." | null
//	}
//
// Only one of the four *_result fields is populated; the others are explicit
// nulls (json: omitempty would drop them, but consumers expect nulls).
// prometheusResponseEnvelope mirrors the top-level shape Prometheus emits at
// /api/v1/query and /api/v1/query_range. Shared between
// PrometheusQueryResultDict and runOneInstantRaw so the two stay aligned.
type prometheusResponseEnvelope struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
}

func PrometheusQueryResultDict(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty Prometheus response")
	}
	var top prometheusResponseEnvelope
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("decode prometheus response: %w", err)
	}
	if top.Status != "success" {
		return map[string]any{
			"result_type":        "error",
			"vector_result":      nil,
			"series_list_result": nil,
			"scalar_result":      nil,
			"string_result":      top.Error,
		}, nil
	}

	out := map[string]any{
		"result_type":        top.Data.ResultType,
		"vector_result":      nil,
		"series_list_result": nil,
		"scalar_result":      nil,
		"string_result":      nil,
	}

	switch top.Data.ResultType {
	case "vector":
		var raws []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		}
		if err := json.Unmarshal(top.Data.Result, &raws); err != nil {
			return nil, fmt.Errorf("decode vector result: %w", err)
		}
		out["vector_result"] = vectorToWire(raws)
	case "matrix":
		var raws []struct {
			Metric map[string]string `json:"metric"`
			Values [][]any           `json:"values"`
		}
		if err := json.Unmarshal(top.Data.Result, &raws); err != nil {
			return nil, fmt.Errorf("decode matrix result: %w", err)
		}
		out["series_list_result"] = matrixToWire(raws)
	case "scalar":
		var raw []any
		if err := json.Unmarshal(top.Data.Result, &raw); err != nil {
			return nil, fmt.Errorf("decode scalar result: %w", err)
		}
		out["scalar_result"] = scalarToWire(raw)
	case "string":
		var raw any
		_ = json.Unmarshal(top.Data.Result, &raw)
		out["string_result"] = fmt.Sprintf("%v", raw)
	default:
		return nil, fmt.Errorf("unknown result_type %q", top.Data.ResultType)
	}
	return out, nil
}

// vectorToWire turns Prometheus instant samples into the
// `[{metric, value: {timestamp, value}}]` wire shape. value is always a
// string, timestamp always a float.
func vectorToWire(raws []struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}) []map[string]any {
	out := make([]map[string]any, 0, len(raws))
	for _, r := range raws {
		ts, val := unpackScalar(r.Value)
		out = append(out, map[string]any{
			"metric": r.Metric,
			"value":  map[string]any{"timestamp": ts, "value": val},
		})
	}
	return out
}

// matrixToWire turns Prometheus range samples into the
// `[{metric, timestamps: [..floats..], values: [..strings..]}]` wire shape —
// [ts, val] pairs split into parallel arrays.
func matrixToWire(raws []struct {
	Metric map[string]string `json:"metric"`
	Values [][]any           `json:"values"`
}) []map[string]any {
	out := make([]map[string]any, 0, len(raws))
	for _, r := range raws {
		timestamps := make([]float64, 0, len(r.Values))
		values := make([]string, 0, len(r.Values))
		for _, v := range r.Values {
			ts, val := unpackScalar(v)
			timestamps = append(timestamps, ts)
			values = append(values, val)
		}
		out = append(out, map[string]any{
			"metric":     r.Metric,
			"timestamps": timestamps,
			"values":     values,
		})
	}
	return out
}

func scalarToWire(raw []any) map[string]any {
	ts, val := unpackScalar(raw)
	return map[string]any{"timestamp": ts, "value": val}
}

// unpackScalar coerces the 2-element [ts, value] Prometheus sample to
// (float64, string).
func unpackScalar(raw []any) (float64, string) {
	if len(raw) < 2 {
		return 0, ""
	}
	var ts float64
	switch t := raw[0].(type) {
	case float64:
		ts = t
	case string:
		ts, _ = strconv.ParseFloat(t, 64)
	}
	return ts, fmt.Sprintf("%v", raw[1])
}
