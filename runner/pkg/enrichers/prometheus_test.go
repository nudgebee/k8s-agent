package enrichers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// fakeProm satisfies PromQuerier with canned responses keyed by query string.
// Concurrent-safe (HandleQueriesEnricher fans queries out in parallel).
type fakeProm struct {
	instant   map[string][]byte
	rng       map[string][]byte
	labels    map[string][]byte
	labelsErr error
}

func (f *fakeProm) Query(_ context.Context, q, _, _ string) ([]byte, error) {
	return f.instant[q], nil
}
func (f *fakeProm) QueryRange(_ context.Context, q, _, _, _, _ string) ([]byte, error) {
	return f.rng[q], nil
}
func (f *fakeProm) LabelValues(_ context.Context, label, _, _ string, _ []string) ([]byte, error) {
	if f.labelsErr != nil {
		return nil, f.labelsErr
	}
	if v, ok := f.labels[label]; ok {
		return v, nil
	}
	return []byte(`{"status":"success","data":[]}`), nil
}

const vectorBody = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"x":"y"},"value":[1700,"1"]}]}}`
const matrixBody = `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"x":"y"},"values":[[1700,"1"],[1760,"2"]]}]}}`

// TestHandleEnricher_InstantWrapsResultInPrometheusBlock locks the
// prometheus_enricher response to the api-server caller's expected shape:
// findings[0].evidence[0].data is a JSON-encoded array containing one
// "prometheus" block whose `data` is a vector_result.
func TestHandleEnricher_InstantWrapsResultInPrometheusBlock(t *testing.T) {
	pe := &PrometheusEnricher{
		q:         &fakeProm{instant: map[string][]byte{"up": []byte(vectorBody)}},
		accountID: "acc-1",
	}
	resp, err := pe.HandleEnricher(context.Background(), map[string]any{
		"promql_query": "up",
		"instant":      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.(map[string]any)
	if r["success"] != true {
		t.Fatalf("success = %v", r["success"])
	}
	evidence := r["findings"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(evidence["data"].(string)), &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "prometheus" {
		t.Fatalf("expected one prometheus block, got %+v", blocks)
	}
	data := blocks[0]["data"].(map[string]any)
	if data["result_type"] != "vector" {
		t.Errorf("result_type = %v", data["result_type"])
	}
}

// TestHandleQueriesEnricher_KeyedJSONBlock locks the prometheus_queries_enricher
// response: findings[0].evidence[0].data → array → json block whose `data`
// is a JSON-encoded `{key: PrometheusQueryResult}` map. This is what the
// backend's response extractor walks.
func TestHandleQueriesEnricher_KeyedJSONBlock(t *testing.T) {
	pe := &PrometheusEnricher{
		q: &fakeProm{rng: map[string][]byte{
			"up":     []byte(matrixBody),
			"errors": []byte(matrixBody),
		}},
		accountID: "acc-1",
	}
	resp, err := pe.HandleQueriesEnricher(context.Background(), map[string]any{
		"promql_queries": []any{
			map[string]any{"key": "A", "query": "up"},
			map[string]any{"key": "B", "query": "errors"},
		},
		"duration": map[string]any{"duration_minutes": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence := resp.(map[string]any)["findings"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(evidence["data"].(string)), &blocks); err != nil {
		t.Fatal(err)
	}
	if blocks[0]["type"] != "json" {
		t.Fatalf("expected json block, got %+v", blocks[0])
	}
	var inner map[string]map[string]any
	if err := json.Unmarshal([]byte(blocks[0]["data"].(string)), &inner); err != nil {
		t.Fatalf("inner JSON: %v", err)
	}
	if _, ok := inner["A"]; !ok {
		t.Errorf("A missing: %+v", inner)
	}
	if _, ok := inner["B"]; !ok {
		t.Errorf("B missing: %+v", inner)
	}
}

// TestHandleQueriesEnricher_InstantReturnsBareResultArray locks the legacy
// Python contract for instant queries: each key's value is the raw
// Prometheus `data.result` array ([{metric, value:[ts,"val"]}, ...]),
// not the PrometheusQueryResult envelope. Backend pod-metric enrichers
// and the relay's /prometheus proxy both walk this shape directly;
// handing them the envelope silently yields empty results and fabricates
// false "missing resource request" insights.
func TestHandleQueriesEnricher_InstantReturnsBareResultArray(t *testing.T) {
	pe := &PrometheusEnricher{
		q: &fakeProm{instant: map[string][]byte{
			"kube_pod_container_resource_requests": []byte(
				`{"status":"success","data":{"resultType":"vector","result":[` +
					`{"metric":{"container":"app","resource":"memory"},"value":[1700,"209715200"]},` +
					`{"metric":{"container":"app","resource":"cpu"},"value":[1700,"0.237"]}` +
					`]}}`),
		}},
		accountID: "acc-1",
	}
	resp, err := pe.HandleQueriesEnricher(context.Background(), map[string]any{
		"promql_queries": []any{
			map[string]any{"key": "requests", "query": "kube_pod_container_resource_requests"},
		},
		"instant":  true,
		"duration": map[string]any{"duration_minutes": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence := resp.(map[string]any)["findings"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(evidence["data"].(string)), &blocks); err != nil {
		t.Fatal(err)
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blocks[0]["data"].(string)), &inner); err != nil {
		t.Fatalf("inner JSON: %v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(inner["requests"], &items); err != nil {
		t.Fatalf("requests is not a bare result array: %v\nbody: %s", err, inner["requests"])
	}
	if len(items) != 2 {
		t.Fatalf("got %d items; want 2", len(items))
	}
	val, ok := items[0]["value"].([]any)
	if !ok {
		t.Fatalf("value is not a 2-element array (envelope leaked through): %T %v", items[0]["value"], items[0]["value"])
	}
	if len(val) != 2 || val[1] != "209715200" {
		t.Errorf("value = %v; want [ts, \"209715200\"]", val)
	}
}

// TestHandleQueriesEnricher_InstantErrorReturnsEnvelope locks the legacy
// error path: when the Prometheus call fails for an instant query, the
// key's value is the PrometheusQueryResult error envelope
// ({result_type:"error", string_result:"..."}), not a partial array — so
// callers that check result_type can distinguish "Prom unreachable" from
// "no series matched".
func TestHandleQueriesEnricher_InstantErrorReturnsEnvelope(t *testing.T) {
	pe := &PrometheusEnricher{
		q: &fakeProm{instant: map[string][]byte{
			"up": []byte(`{"status":"error","errorType":"bad_data","error":"connection refused"}`),
		}},
		accountID: "acc-1",
	}
	resp, err := pe.HandleQueriesEnricher(context.Background(), map[string]any{
		"promql_queries": []any{map[string]any{"key": "A", "query": "up"}},
		"instant":        true,
		"duration":       map[string]any{"duration_minutes": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence := resp.(map[string]any)["findings"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(evidence["data"].(string)), &blocks)
	var inner map[string]map[string]any
	if err := json.Unmarshal([]byte(blocks[0]["data"].(string)), &inner); err != nil {
		t.Fatalf("inner JSON: %v\nbody: %s", err, blocks[0]["data"])
	}
	got := inner["A"]
	if got["result_type"] != "error" {
		t.Errorf("result_type = %v; want error", got["result_type"])
	}
	if !contains(got["string_result"].(string), "connection refused") {
		t.Errorf("string_result = %q; want wrapped 'connection refused'", got["string_result"])
	}
}

// TestHandleEnricher_RoutesToQueriesWhenMultiQuery covers the
// "promql_queries set → delegate" path in prometheus_enricher.
func TestHandleEnricher_RoutesToQueriesWhenMultiQuery(t *testing.T) {
	pe := &PrometheusEnricher{
		q: &fakeProm{rng: map[string][]byte{
			"only-this": []byte(matrixBody),
		}},
		accountID: "acc-1",
	}
	resp, err := pe.HandleEnricher(context.Background(), map[string]any{
		"promql_queries": []any{
			map[string]any{"key": "Z", "query": "only-this"},
		},
		"duration": map[string]any{"duration_minutes": 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence := resp.(map[string]any)["findings"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(evidence["data"].(string)), &blocks)
	if blocks[0]["type"] != "json" {
		t.Errorf("expected json block (queries route), got %v", blocks[0]["type"])
	}
}

// TestHandleEnricher_MissingQueryReturnsErrorShape covers the validation
// guard for a missing required query.
func TestHandleEnricher_MissingQueryReturnsErrorShape(t *testing.T) {
	pe := &PrometheusEnricher{q: &fakeProm{}, accountID: "acc-1"}
	resp, _ := pe.HandleEnricher(context.Background(), map[string]any{})
	r := resp.(map[string]any)
	if r["success"] != false {
		t.Errorf("expected success=false, got %+v", r)
	}
	if r["error_code"] != 400 {
		t.Errorf("error_code = %v; want 400", r["error_code"])
	}
}

// ---------- prometheus_labels ----------

// TestHandleLabels_WrapsLabelValuesInResultTypeJsonBlock locks the response
// shape api-server's parseRelayData → decodeInnerData chain expects
// (services/observability/prometheus.go:243-321): a Finding whose first
// evidence block is `type: json` carrying `{result_type, data, error}`.
func TestHandleLabels_WrapsLabelValuesInResultTypeJsonBlock(t *testing.T) {
	pe := &PrometheusEnricher{
		q: &fakeProm{labels: map[string][]byte{
			"__name__": []byte(`{"status":"success","data":["up","node_load1","apiserver_request_total"]}`),
		}},
		accountID: "acc-1",
	}
	resp, err := pe.HandleLabels(context.Background(), map[string]any{"label_name": "__name__"})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.(map[string]any)
	if r["success"] != true {
		t.Fatalf("success = %v; want true", r["success"])
	}
	evidence := r["findings"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(evidence["data"].(string)), &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "json" {
		t.Fatalf("evidence blocks = %#v; want one json block", blocks)
	}
	// blocks[0].data is itself a JSON string (the JsonBlock pattern).
	// Unwrap and assert the payload.
	var payload struct {
		ResultType string   `json:"result_type"`
		Data       []string `json:"data"`
		Error      string   `json:"error"`
	}
	if err := json.Unmarshal([]byte(blocks[0]["data"].(string)), &payload); err != nil {
		t.Fatalf("inner data not JSON: %v\n%s", err, blocks[0]["data"])
	}
	if payload.ResultType != "success" {
		t.Errorf("result_type = %q; want success", payload.ResultType)
	}
	want := []string{"up", "node_load1", "apiserver_request_total"}
	if len(payload.Data) != len(want) {
		t.Fatalf("data = %v; want %v", payload.Data, want)
	}
	for i, v := range want {
		if payload.Data[i] != v {
			t.Errorf("data[%d] = %q; want %q", i, payload.Data[i], v)
		}
	}
	if payload.Error != "" {
		t.Errorf("error = %q; want empty", payload.Error)
	}
}

// TestHandleLabels_MissingLabelNameReturnsErrorJsonBlock —
// PrometheusLabelsValueParams requires `label_name`. The shape on the
// except-branch is success at the dispatch level, error at the inner
// JsonBlock level.
func TestHandleLabels_MissingLabelNameReturnsErrorJsonBlock(t *testing.T) {
	pe := &PrometheusEnricher{q: &fakeProm{}, accountID: "acc-1"}
	resp, err := pe.HandleLabels(context.Background(), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.(map[string]any)
	if r["success"] != true {
		t.Fatalf("dispatch-level success = %v; want true (error is per-block)", r["success"])
	}
	evidence := r["findings"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(evidence["data"].(string)), &blocks)
	var payload struct {
		ResultType string `json:"result_type"`
		Error      string `json:"error"`
	}
	_ = json.Unmarshal([]byte(blocks[0]["data"].(string)), &payload)
	if payload.ResultType != "error" {
		t.Errorf("result_type = %q; want error", payload.ResultType)
	}
	if payload.Error == "" {
		t.Error("error message should describe the missing label_name")
	}
}

// TestHandleLabels_PrometheusErrorPropagatesToBlockError — when
// prom.LabelValues fails (network, 5xx, …) the dispatch still returns a
// Finding so the api-server caller's parseRelayData chain doesn't blow up,
// but the inner block carries result_type=error with the wrapped message.
func TestHandleLabels_PrometheusErrorPropagatesToBlockError(t *testing.T) {
	pe := &PrometheusEnricher{
		q:         &fakeProm{labelsErr: errors.New("prometheus 500: connection refused")},
		accountID: "acc-1",
	}
	resp, _ := pe.HandleLabels(context.Background(), map[string]any{"label_name": "namespace"})
	evidence := resp.(map[string]any)["findings"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(evidence["data"].(string)), &blocks)
	var payload struct {
		ResultType string   `json:"result_type"`
		Data       []string `json:"data"`
		Error      string   `json:"error"`
	}
	_ = json.Unmarshal([]byte(blocks[0]["data"].(string)), &payload)
	if payload.ResultType != "error" {
		t.Errorf("result_type = %q; want error", payload.ResultType)
	}
	if !contains(payload.Error, "connection refused") {
		t.Errorf("error = %q; want wrapped 'connection refused'", payload.Error)
	}
	if len(payload.Data) != 0 {
		t.Errorf("data = %v; want empty on error", payload.Data)
	}
}

// TestHandleLabels_NonSuccessPrometheusStatusBecomesBlockError covers the
// case where Prometheus returns 200 with body {"status":"error","error":"..."}.
// extractPrometheusDataArray translates that into a wrapped error.
func TestHandleLabels_NonSuccessPrometheusStatusBecomesBlockError(t *testing.T) {
	pe := &PrometheusEnricher{
		q: &fakeProm{labels: map[string][]byte{
			"namespace": []byte(`{"status":"error","errorType":"bad_data","error":"invalid match[]"}`),
		}},
		accountID: "acc-1",
	}
	resp, _ := pe.HandleLabels(context.Background(), map[string]any{"label_name": "namespace"})
	evidence := resp.(map[string]any)["findings"].([]any)[0].(map[string]any)["evidence"].([]any)[0].(map[string]any)
	var blocks []map[string]any
	_ = json.Unmarshal([]byte(evidence["data"].(string)), &blocks)
	var payload struct {
		ResultType string `json:"result_type"`
		Error      string `json:"error"`
	}
	_ = json.Unmarshal([]byte(blocks[0]["data"].(string)), &payload)
	if payload.ResultType != "error" {
		t.Errorf("result_type = %q; want error for non-success Prometheus status", payload.ResultType)
	}
	if !contains(payload.Error, "invalid match[]") {
		t.Errorf("error = %q; want Prometheus error preserved", payload.Error)
	}
}

func TestExtractPrometheusDataArray_NullDataIsEmpty(t *testing.T) {
	// Prometheus emits {"status":"success","data":null} for label values
	// with zero matches. get_label_values returns []; we must too.
	got, err := extractPrometheusDataArray([]byte(`{"status":"success","data":null}`))
	if err != nil {
		t.Fatalf("extract null data: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("got %v; want empty []any", got)
	}
}

func TestParamStringSlice(t *testing.T) {
	cases := []struct {
		in   any
		want []string
	}{
		{nil, nil},
		{`x`, []string{"x"}},
		{[]string{"a", "b"}, []string{"a", "b"}},
		{[]any{"a", "b"}, []string{"a", "b"}},
		{[]any{"a", 42}, []string{"a"}},
		{42, nil},
	}
	for _, c := range cases {
		got := paramStringSlice(map[string]any{"k": c.in}, "k")
		if len(got) != len(c.want) {
			t.Errorf("paramStringSlice(%v) = %v; want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("paramStringSlice(%v)[%d] = %q; want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
