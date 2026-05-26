package servicemap

import (
	"encoding/json"
	"strconv"
)

// promResult is one entry in a Prometheus query_range response. We parse
// only what we need — labels (a flat map[string]string) and the most
// recent value (.last in render_service_map).
type promResult struct {
	Metric map[string]string
	Last   float64 // value at the latest timestamp; NaN if no points
	HasVal bool
}

// promQueryRangeResponse mirrors the JSON shape Prometheus returns from
// /api/v1/query_range. We unmarshal then walk it; the agent's prometheus
// client (pkg/observability/prometheus) returns this verbatim as
// json.RawMessage.
type promQueryRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     []promResultRaw `json:"result"`
	} `json:"data"`
}

type promResultRaw struct {
	Metric map[string]string `json:"metric"`
	// values is [[ts, "string-value"], ...]
	Values [][]any `json:"values"`
}

// parsePromRangeResponse extracts the labels + last value from each result
// in a Prometheus query_range response. Empty/error responses become an
// empty slice with no error — callers can distinguish "no metric data" from
// "fetch failed" via a non-nil error path further up.
func parsePromRangeResponse(raw json.RawMessage) ([]promResult, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var resp promQueryRangeResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return nil, nil
	}
	out := make([]promResult, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		pr := promResult{Metric: r.Metric}
		if v, ok := lastValue(r.Values); ok {
			pr.Last = v
			pr.HasVal = true
		}
		out = append(out, pr)
	}
	return out, nil
}

// lastValue extracts the final sample's float from a Prometheus values pair
// list. Each sample is [timestamp, "<float as string>"]. Returns false if
// the list is empty or the value can't parse.
func lastValue(values [][]any) (float64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	last := values[len(values)-1]
	if len(last) != 2 {
		return 0, false
	}
	s, ok := last[1].(string)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// labelOr returns labels[key] or fallback if absent.
func labelOr(labels map[string]string, key, fallback string) string {
	if v, ok := labels[key]; ok && v != "" {
		return v
	}
	return fallback
}
