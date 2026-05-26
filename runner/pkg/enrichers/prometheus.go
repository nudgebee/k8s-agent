package enrichers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

// PromQuerier is the subset of *prometheus.Client this package needs. We type
// it as an interface so tests can drop in a fake without spinning up an HTTP
// server.
type PromQuerier interface {
	Query(ctx context.Context, query, atTime, timeout string) (raw []byte, err error)
	QueryRange(ctx context.Context, query, start, end, step, timeout string) (raw []byte, err error)
	LabelValues(ctx context.Context, label, start, end string, match []string) (raw []byte, err error)
}

// promClientAdapter adapts *prometheus.Client (which returns json.RawMessage)
// to the []byte signature above. Trivial; just here so we don't put []byte
// into the package public API of the underlying client.
type promClientAdapter struct{ c *prometheus.Client }

func (a promClientAdapter) Query(ctx context.Context, q, t, to string) ([]byte, error) {
	r, err := a.c.Query(ctx, q, t, to)
	return []byte(r), err
}

func (a promClientAdapter) QueryRange(ctx context.Context, q, s, e, st, to string) ([]byte, error) {
	r, err := a.c.QueryRange(ctx, q, s, e, st, to)
	return []byte(r), err
}

func (a promClientAdapter) LabelValues(ctx context.Context, label, start, end string, match []string) ([]byte, error) {
	r, err := a.c.LabelValues(ctx, label, start, end, match)
	return []byte(r), err
}

// PrometheusEnricher implements the `prometheus_enricher` and
// `prometheus_queries_enricher` actions. The api-server callers
// talk to these by name and parse the wire shape via
// relay.ExecuteAndExtractResponse.
type PrometheusEnricher struct {
	q         PromQuerier
	accountID string
}

// NewPrometheusEnricher wires the enricher to a *prometheus.Client. accountID
// is stamped into the Finding's evidence.
func NewPrometheusEnricher(c *prometheus.Client, accountID string) *PrometheusEnricher {
	return &PrometheusEnricher{q: promClientAdapter{c: c}, accountID: accountID}
}

// HandleEnricher implements `prometheus_enricher`. Single-query path —
// the result is wrapped in a PrometheusBlock.
//
// Action params (PrometheusQueryParams):
//
//	promql_query   string  (required, unless promql_queries is set)
//	promql_queries [{key, query}]  (alt; routes through HandleQueriesEnricher)
//	instant        bool    (default false → range query)
//	duration       supports {duration_minutes} OR {starts_at, ends_at}
//	step           string  (range queries only, defaults to "60")
//
// If `promql_queries` is set, we delegate.
func (p *PrometheusEnricher) HandleEnricher(ctx context.Context, params map[string]any) (any, error) {
	queries, _ := params["promql_queries"].([]any)
	if len(queries) > 0 {
		return p.HandleQueriesEnricher(ctx, params)
	}
	query, _ := params["promql_query"].(string)
	if query == "" {
		return ErrorResponse("Invalid request, prometheus_enricher requires a promql query.", 400), nil
	}
	startsAt, endsAt, err := parseDuration(params["duration"])
	if err != nil {
		return ErrorResponse(err.Error(), 400), nil
	}
	instant, _ := params["instant"].(bool)

	resultDict, err := p.runOne(ctx, query, instant, startsAt, endsAt, stringStep(params))
	if err != nil {
		// Still emit a PrometheusBlock with result_type="error" so callers
		// don't have to special-case.
		resultDict = map[string]any{
			"result_type":        "error",
			"vector_result":      nil,
			"series_list_result": nil,
			"scalar_result":      nil,
			"string_result":      err.Error(),
		}
	}
	block := PrometheusBlock(resultDict, query)
	return FindingResponse(p.accountID, uuid.Nil, block)
}

// HandleQueriesEnricher implements `prometheus_queries_enricher`. Multi-query
// path — packs `{key: <result>}` into one JsonBlock.
//
// Per-key wire shape matches the legacy Python prometheus_queries_enricher
// contract that downstream consumers (api-server pod_metric_enricher,
// workflow engine, LLM server, relay-server's /prometheus proxy) were
// written against:
//
//	instant + success → bare Prometheus result array, exactly as Prom returns
//	                    under data.result: [{metric, value:[ts,"val"]}, ...]
//	range   + success → PrometheusQueryResult.dict() envelope with
//	                    series_list_result populated
//	any     + error   → envelope with result_type="error", string_result=<msg>
//
// action params:
//
//	promql_queries [{key, query}]   (required)
//	instant        bool
//	duration       same as enricher (object form: {ends_at, starts_at})
//	steps          string           (note: "steps" here, not "step")
func (p *PrometheusEnricher) HandleQueriesEnricher(ctx context.Context, params map[string]any) (any, error) {
	rawQueries, _ := params["promql_queries"].([]any)
	if len(rawQueries) == 0 {
		// Single-query callers may have routed here by mistake; degrade gracefully.
		if q, _ := params["promql_query"].(string); q != "" {
			rawQueries = []any{map[string]any{"key": "A", "query": q}}
		} else {
			return ErrorResponse("Invalid request, prometheus_enricher requires a promql query.", 400), nil
		}
	}
	startsAt, endsAt, err := parseDuration(params["duration"])
	if err != nil {
		return ErrorResponse(err.Error(), 400), nil
	}
	instant, _ := params["instant"].(bool)
	step := stringStep(params)

	type kv struct {
		key, query string
	}
	queries := make([]kv, 0, len(rawQueries))
	for _, q := range rawQueries {
		m, ok := q.(map[string]any)
		if !ok {
			continue
		}
		key, _ := m["key"].(string)
		qstr, _ := m["query"].(string)
		if qstr == "" {
			continue
		}
		queries = append(queries, kv{key: key, query: qstr})
	}

	type slot struct {
		key string
		// res is []any for instant+success (bare Prometheus result array),
		// map[string]any otherwise (range envelope, or error envelope).
		res any
	}
	results := make([]slot, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		i, q := i, q
		wg.Add(1)
		go func() {
			defer wg.Done()
			if instant {
				list, runErr := p.runOneInstantRaw(ctx, q.query, endsAt)
				if runErr != nil {
					results[i] = slot{key: q.key, res: errorEnvelope(runErr)}
					return
				}
				results[i] = slot{key: q.key, res: list}
				return
			}
			r, runErr := p.runOne(ctx, q.query, false, startsAt, endsAt, step)
			if runErr != nil {
				r = errorEnvelope(runErr)
			}
			results[i] = slot{key: q.key, res: r}
		}()
	}
	wg.Wait()

	out := make(map[string]any, len(results))
	for _, s := range results {
		out[s.key] = s.res
	}

	block, err := JSONBlock(out)
	if err != nil {
		return nil, err
	}
	return FindingResponse(p.accountID, uuid.Nil, block)
}

func errorEnvelope(err error) map[string]any {
	return map[string]any{
		"result_type":        "error",
		"vector_result":      nil,
		"series_list_result": nil,
		"scalar_result":      nil,
		"string_result":      err.Error(),
	}
}

// HandleLabels implements `prometheus_labels`. Despite the name, this action
// returns the *values* for a single label (Prometheus
// /api/v1/label/<name>/values), not the list of all label names — it passes
// through to `prom.get_label_values`. The two production callers
// + chronosphere.go:131
// + llm/rag-server) all send `{label_name: "..."}` and unwrap a JsonBlock
// of {result_type, data, error} via parseRelayData → decodeInnerData.
//
// Output shape: a Finding with one JsonBlock whose payload is
//
//	{"result_type": "success", "data": [...], "error": ""}     (success)
//	{"result_type": "error",   "data": [],    "error": "..."}  (failure)
//
// Action params (PrometheusLabelsValueParams):
//
//	label_name string  (required; "__name__" returns the metric list)
//	start      string  (optional; unix seconds)
//	end        string  (optional; unix seconds)
//	match[]    []string (optional; series selector filter)
func (p *PrometheusEnricher) HandleLabels(ctx context.Context, params map[string]any) (any, error) {
	labelName, _ := params["label_name"].(string)
	if labelName == "" {
		return p.labelsErrorFinding("prometheus_labels: label_name is required")
	}
	start, _ := params["start"].(string)
	end, _ := params["end"].(string)
	match := paramStringSlice(params, "match[]")

	raw, err := p.q.LabelValues(ctx, labelName, start, end, match)
	if err != nil {
		return p.labelsErrorFinding(fmt.Sprintf("prometheus_labels: %v", err))
	}
	values, err := extractPrometheusDataArray(raw)
	if err != nil {
		return p.labelsErrorFinding(fmt.Sprintf("prometheus_labels: parse response: %v", err))
	}
	payload := map[string]any{
		"result_type": "success",
		"data":        values,
		"error":       "",
	}
	block, err := JSONBlock(payload)
	if err != nil {
		return nil, err
	}
	return FindingResponse(p.accountID, uuid.Nil, block)
}

// labelsErrorFinding emits the failure branch: the Finding still succeeds
// (success=true at the dispatcher level) but the inner JsonBlock carries
// result_type=error so callers can distinguish a network/Prometheus
// failure from "label has no values".
func (p *PrometheusEnricher) labelsErrorFinding(msg string) (any, error) {
	payload := map[string]any{
		"result_type": "error",
		"data":        []any{},
		"error":       msg,
	}
	block, err := JSONBlock(payload)
	if err != nil {
		return nil, err
	}
	return FindingResponse(p.accountID, uuid.Nil, block)
}

// extractPrometheusDataArray pulls `.data` out of a Prometheus
// `{status,data,...}` envelope as a []any. Returns an empty slice when
// status is success but data is null (Prometheus serves empty as null);
// returns an error when status is non-success or the shape is malformed.
func extractPrometheusDataArray(raw []byte) ([]any, error) {
	var env struct {
		Status string `json:"status"`
		Data   []any  `json:"data"`
		Error  string `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.Status != "success" {
		if env.Error == "" {
			env.Error = "non-success status: " + env.Status
		}
		return nil, errors.New(env.Error)
	}
	if env.Data == nil {
		return []any{}, nil
	}
	return env.Data, nil
}

// paramStringSlice extracts a []string from a param that could be a Go
// []string, []any of strings, or a single string. Mirrors observability/
// prometheus.strSlice — duplicated here to keep enrichers free of an import
// cycle. (We don't import the observability package's helpers; the seven-line
// duplication is cheaper than another internal helper package.)
func paramStringSlice(p map[string]any, key string) []string {
	if p == nil {
		return nil
	}
	switch v := p[key].(type) {
	case nil:
		return nil
	case string:
		return []string{v}
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// runOneInstantRaw executes an instant query and returns the bare
// `data.result` array from the Prometheus response — the same shape the
// legacy Python prometheus_queries_enricher emitted from
// prom.custom_query() (each element is `{metric: {...}, value: [ts, "val"]}`).
//
// Downstream consumers in api-server (pod_metric_enricher, workflow
// engine, LLM server) and relay-server's /prometheus proxy parse this
// shape directly and silently produce empty/wrong results when handed
// the PrometheusQueryResult envelope instead.
//
// On HTTP / parse error or non-success Prometheus status, returns the
// error so HandleQueriesEnricher can wrap it into the legacy error
// envelope (result_type="error").
func (p *PrometheusEnricher) runOneInstantRaw(ctx context.Context, query string, endsAt time.Time) ([]any, error) {
	t := ""
	if !endsAt.IsZero() {
		t = strconv.FormatInt(endsAt.Unix(), 10)
	}
	raw, err := p.q.Query(ctx, query, t, "")
	if err != nil {
		return nil, err
	}
	var env prometheusResponseEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("decode prometheus response: %w", err)
	}
	if env.Status != "success" {
		msg := env.Error
		if msg == "" {
			msg = "non-success status: " + env.Status
		}
		return nil, errors.New(msg)
	}
	// `result` is required in a successful Prom response, but guard the
	// missing-field case explicitly: json.Unmarshal(nil, &result) returns
	// "unexpected end of JSON input", not a no-op.
	if len(env.Data.Result) == 0 {
		return []any{}, nil
	}
	var result []any
	if err := json.Unmarshal(env.Data.Result, &result); err != nil {
		return nil, fmt.Errorf("decode result array: %w", err)
	}
	if result == nil {
		return []any{}, nil
	}
	return result, nil
}

// runOne executes a single query and returns the PrometheusQueryResult.dict()
// shape. instant=true → /api/v1/query at endsAt; instant=false → query_range.
func (p *PrometheusEnricher) runOne(ctx context.Context, query string, instant bool, startsAt, endsAt time.Time, step string) (map[string]any, error) {
	var raw []byte
	var err error
	if instant {
		t := ""
		if !endsAt.IsZero() {
			t = strconv.FormatInt(endsAt.Unix(), 10)
		}
		raw, err = p.q.Query(ctx, query, t, "")
	} else {
		if startsAt.IsZero() || endsAt.IsZero() {
			return nil, errors.New("prometheus_enricher: range query requires duration with starts_at and ends_at")
		}
		s := strconv.FormatInt(startsAt.Unix(), 10)
		e := strconv.FormatInt(endsAt.Unix(), 10)
		if step == "" {
			step = "60"
		}
		raw, err = p.q.QueryRange(ctx, query, s, e, step, "")
	}
	if err != nil {
		return nil, err
	}
	return PrometheusQueryResultDict(raw)
}

// parseDuration parses the PrometheusDuration / PrometheusDateRange union.
// Accepts:
//
//	{"duration_minutes": 5}                        → starts_at = now - 5m, ends_at = now
//	{"starts_at":"2024-... UTC", "ends_at":"..."}  → fixed window
//	[{...}]                                        → list-wrapped variants
//	nil                                            → both zero (caller must handle for instant)
//
// We accept "%Y-%m-%d %H:%M:%S UTC" and RFC3339 transparently.
func parseDuration(raw any) (time.Time, time.Time, error) {
	if raw == nil {
		return time.Time{}, time.Time{}, nil
	}
	if list, ok := raw.([]any); ok && len(list) > 0 {
		raw = list[0]
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("duration: unexpected type %T", raw)
	}
	if dm, ok := m["duration_minutes"]; ok {
		mins, err := toInt(dm)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("duration_minutes: %w", err)
		}
		now := time.Now().UTC().Truncate(time.Second)
		return now.Add(-time.Duration(mins) * time.Minute), now, nil
	}
	starts, _ := m["starts_at"].(string)
	ends, _ := m["ends_at"].(string)
	if starts == "" || ends == "" {
		return time.Time{}, time.Time{}, errors.New("duration: starts_at and ends_at are required")
	}
	s, err := parseTimestamp(starts)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("starts_at: %w", err)
	}
	e, err := parseTimestamp(ends)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("ends_at: %w", err)
	}
	return s, e, nil
}

// parseTimestamp accepts the formats callers send (including the
// Postgres-style "2006-01-02 15:04:05 UTC" some callers emit).
func parseTimestamp(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{
		"2006-01-02 15:04:05 UTC",
		"2006-01-02 15:04:05 MST",
		time.RFC3339,
		time.RFC3339Nano,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", s)
}

func toInt(v any) (int, error) {
	switch x := v.(type) {
	case int:
		return x, nil
	case int64:
		return int(x), nil
	case float64:
		return int(x), nil
	case string:
		i, err := strconv.Atoi(x)
		return i, err
	}
	return 0, fmt.Errorf("expected number, got %T", v)
}

func stringStep(params map[string]any) string {
	// "step" is used in prometheus_enricher and "steps" in
	// prometheus_queries_enricher. Accept both.
	for _, key := range []string{"step", "steps"} {
		if s, ok := params[key].(string); ok && s != "" {
			return s
		}
		if n, ok := params[key].(float64); ok && n > 0 {
			return strconv.FormatFloat(n, 'f', -1, 64)
		}
	}
	return ""
}
