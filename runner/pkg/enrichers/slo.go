package enrichers

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

// SLOEnricher implements `slo_generator` — runs the SLO config's underlying
// queries against Prometheus, computes good/bad event counts per workload,
// derives SLI / error budget / burn rate, and emits a list of SLOReport
// dicts.
//
// Wire shape:
//
//	{
//	  "success": true,
//	  "data":    [<list of SLOReport dicts>]
//	}
//
// Caller deserializes each dict into an SLOReport struct on the backend.
type SLOEnricher struct {
	q PromQuerier
}

// NewSLOEnricher returns an SLO generator wired to the given Prometheus client.
func NewSLOEnricher(c *prometheus.Client) *SLOEnricher {
	return &SLOEnricher{q: promClientAdapter{c: c}}
}

// Handler returns a dispatch.Handler for slo_generator.
func (s *SLOEnricher) Handler() dispatch.Handler {
	return func(ctx context.Context, params map[string]any) (any, error) {
		reports, err := s.compute(ctx, params)
		if err != nil {
			return map[string]any{
				"success": false,
				"data":    []any{},
				"msg":     err.Error(),
			}, nil
		}
		return map[string]any{
			"success": true,
			"data":    reports,
		}, nil
	}
}

// sloConfig is the SLOConfig dataclass mirror. All fields read from the
// action_params.slo_config map.
type sloConfig struct {
	Name                         string  `json:"name"`
	Goal                         float64 `json:"goal"`
	Method                       string  `json:"method"`
	GroupBy                      string  `json:"group_by"`
	Expression                   string  `json:"expression"`
	FilterGood                   string  `json:"filter_good"`
	FilterBad                    string  `json:"filter_bad"`
	FilterValid                  string  `json:"filter_valid"`
	ErrorBudgetBurnRateThreshold float64 `json:"error_budget_burn_rate_threshold"`
	Window                       int     `json:"window"`
	ThresholdBucket              float64 `json:"threshold_bucket"`
}

func (s *SLOEnricher) compute(ctx context.Context, params map[string]any) ([]map[string]any, error) {
	rawCfg, _ := params["slo_config"].(map[string]any)
	if len(rawCfg) == 0 {
		return nil, fmt.Errorf("slo_generator: slo_config is required")
	}
	cfg := parseSLOConfig(rawCfg)
	if cfg.Window <= 0 {
		cfg.Window = 3600
	}
	if cfg.GroupBy == "" || strings.Contains(cfg.GroupBy, "actual_destination_workload_name") {
		cfg.GroupBy = "destination_workload_name, destination_workload_namespace"
	}
	if cfg.ErrorBudgetBurnRateThreshold == 0 {
		cfg.ErrorBudgetBurnRateThreshold = 14.4
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-time.Duration(cfg.Window) * time.Second)

	queries, err := buildSLOQueries(cfg)
	if err != nil {
		return nil, err
	}
	step := int64(cfg.Window) // SLO uses window as step (one bucket).

	// Reuse the application_stats Prometheus runner so the parsing path is
	// identical to get_application_stats.
	a := &AppStatsEnricher{q: s.q}
	results := a.runQueries(ctx, queries, startTime, endTime, step, "", "", "")
	apps := extractMetricStats(results)

	minValidEvents := envIntDefault("MIN_VALID_EVENTS", 1)
	reports := make([]map[string]any, 0, len(apps))
	for _, app := range apps {
		stats := app.toResponse()
		report := buildSLOReport(cfg, stats, startTime, endTime, minValidEvents)
		reports = append(reports, report)
	}
	return reports, nil
}

// parseSLOConfig accepts the JSON-decoded action_params.slo_config map.
func parseSLOConfig(m map[string]any) sloConfig {
	out := sloConfig{}
	out.Name, _ = m["name"].(string)
	out.Goal = toFloat(m["goal"])
	out.Method, _ = m["method"].(string)
	out.GroupBy, _ = m["group_by"].(string)
	out.Expression, _ = m["expression"].(string)
	out.FilterGood, _ = m["filter_good"].(string)
	out.FilterBad, _ = m["filter_bad"].(string)
	out.FilterValid, _ = m["filter_valid"].(string)
	out.ErrorBudgetBurnRateThreshold = toFloat(m["error_budget_burn_rate_threshold"])
	if v, err := toInt(m["window"]); err == nil {
		out.Window = v
	}
	out.ThresholdBucket = toFloat(m["threshold_bucket"])
	return out
}

// buildSLOQueries assembles the per-method query map. Methods supported:
//
//	good_bad_ratio    — needs filter_good + (filter_bad OR filter_valid)
//	distribution_cut  — needs expression + threshold_bucket
//
// Ports the backend verbatim, including the _bucket→_count rewrite
// and the `le=~"<bucket>(\\.0+)?"` regex match.
func buildSLOQueries(cfg sloConfig) (map[string]string, error) {
	groupOp := fmt.Sprintf("sum by (%s)", cfg.GroupBy)
	switch cfg.Method {
	case "good_bad_ratio":
		if cfg.FilterGood == "" {
			return nil, fmt.Errorf("slo_generator: good_bad_ratio requires filter_good")
		}
		queries := map[string]string{
			"filter_good": fmtSLOQuery(cfg.FilterGood, cfg.Window, []string{"increase", groupOp}, nil),
		}
		switch {
		case cfg.FilterBad != "":
			queries["filter_bad"] = fmtSLOQuery(cfg.FilterBad, cfg.Window, []string{"increase", groupOp}, nil)
		case cfg.FilterValid != "":
			queries["filter_valid"] = fmtSLOQuery(cfg.FilterValid, cfg.Window, []string{"increase", groupOp}, nil)
		default:
			return nil, fmt.Errorf("slo_generator: good_bad_ratio requires filter_bad or filter_valid")
		}
		return queries, nil
	case "distribution_cut":
		if cfg.Expression == "" || cfg.ThresholdBucket == 0 {
			return nil, fmt.Errorf("slo_generator: distribution_cut requires expression and threshold_bucket")
		}
		bucket := strconv.FormatFloat(cfg.ThresholdBucket, 'f', -1, 64)
		queries := map[string]string{
			"filter_good": fmtSLOQuery(cfg.Expression, cfg.Window, []string{"increase", groupOp}, map[string]string{"le": bucket}),
		}
		exprCount := strings.ReplaceAll(cfg.Expression, "_bucket", "_count")
		queries["filter_valid"] = fmtSLOQuery(exprCount, cfg.Window, []string{"increase", groupOp}, nil)
		return queries, nil
	}
	return nil, fmt.Errorf("slo_generator: unknown method %q", cfg.Method)
}

// fmtSLOQuery is the line-for-line port of SLOGenerator._fmt_query
// . Replaces `[window` if present, otherwise appends
// `[<window>s]`; wraps with each operator; for `le` labels uses the regex
// match needed to handle `0.5` vs `0.500` etc.
func fmtSLOQuery(query string, window int, operators []string, labels map[string]string) string {
	q := strings.TrimSpace(query)
	if strings.Contains(q, "[window") {
		q = strings.ReplaceAll(q, "[window", fmt.Sprintf("[%ds", window))
	} else {
		q += fmt.Sprintf("[%ds]", window)
	}
	for _, op := range operators {
		q = fmt.Sprintf("%s(%s)", op, q)
	}
	for k, v := range labels {
		if k == "le" {
			// Escape: ', le=~"^<v>(\\.0+)?$"}'  — note the doubled
			// backslash in the Python source becomes a single \ on the wire.
			q = strings.Replace(q, "}", fmt.Sprintf(`, %s=~"^%s(\.0+)?$"}`, k, v), 1)
		} else {
			q = strings.Replace(q, "}", fmt.Sprintf(`, %s="%s"}`, k, v), 1)
		}
	}
	return q
}

// buildSLOReport mirrors SLOReport.build plus the
// validate() pre-check.
func buildSLOReport(cfg sloConfig, stats map[string]any, startTime, endTime time.Time, minValidEvents int) map[string]any {
	const noData = -1
	report := map[string]any{
		"workload":                         stringFromMap(stats, "name"),
		"namespace":                        stringFromMap(stats, "namespace"),
		"name":                             cfg.Name,
		"goal":                             cfg.Goal,
		"window":                           cfg.Window,
		"start_time":                       float64(startTime.Unix()),
		"end_time":                         float64(endTime.Unix()),
		"valid":                            true,
		"alert":                            false,
		"sli_measurement":                  0.0,
		"events_count":                     0,
		"good_events_count":                0,
		"bad_events_count":                 0,
		"alert_message":                    "",
		"error_budget_burn_rate_threshold": cfg.ErrorBudgetBurnRateThreshold,
	}

	good := numFromMap(stats, "good_data_count", noData)
	bad := numFromMap(stats, "bad_data_count", noData)
	valid := numFromMap(stats, "valid_data_count", noData)

	// Validate: bad/good/valid must have enough events. If both are NO_DATA we
	// treat as invalid.
	if good == float64(noData) || math.IsNaN(good) {
		good = 0
	}
	if bad == float64(noData) || math.IsNaN(bad) {
		bad = 0
	}
	if good == 0 && bad == 0 && valid == float64(noData) {
		report["valid"] = false
		return report
	}
	if good+bad < float64(minValidEvents) && valid == float64(noData) {
		report["valid"] = false
		return report
	}

	// SLI: good/(good+bad) for tuple-form, or valid_data_count directly when
	// the SLO config supplies a precomputed SLI value (the ratio is already
	// in the metric).
	var sli float64
	if good+bad > 0 {
		sli = round6(good / (good + bad))
	} else if valid != float64(noData) {
		sli = round6(valid)
	}

	gap := sli - cfg.Goal
	ebTarget := 1 - cfg.Goal
	ebValue := 1 - sli
	ebRemainingMinutes := float64(cfg.Window) * gap / 60
	ebTargetMinutes := float64(cfg.Window) * ebTarget / 60
	ebMinutes := float64(cfg.Window) * ebValue / 60
	var ebRatio, ebBurnRate float64
	if ebTarget > 0 {
		ebRatio = ebValue * 100 / ebTarget
		ebBurnRate = math.Round(ebValue/ebTarget*10) / 10 // round(x, 1)
	}
	hours := int(ebBurnRate / (60 * 60))
	hourLabel := "hours"
	if hours == 1 {
		hourLabel = "hour"
	}
	alertMessage := fmt.Sprintf("error budget burn rate is %.1fx within %d %s", ebBurnRate, hours, hourLabel)
	alert := false
	if cfg.ErrorBudgetBurnRateThreshold > 0 {
		alert = ebBurnRate > cfg.ErrorBudgetBurnRateThreshold
	}

	report["gap"] = gap
	report["error_budget_target"] = ebTarget
	report["error_budget_measurement"] = ebValue
	report["error_budget_burn_rate"] = ebBurnRate
	report["error_budget_remaining_minutes"] = ebRemainingMinutes
	report["error_budget_minutes"] = ebTargetMinutes
	report["error_minutes"] = ebMinutes
	report["error_budget_consumed_ratio"] = ebRatio
	report["sli_measurement"] = sli
	report["events_count"] = int(good + bad)
	report["good_events_count"] = int(good)
	report["bad_events_count"] = int(bad)
	report["alert_message"] = alertMessage
	report["alert"] = alert

	if sli < 0 || sli > 1 {
		// _post_validate: SLI must be in [0,1].
		report["valid"] = false
	}
	return report
}

func stringFromMap(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func numFromMap(m map[string]any, key string, def float64) float64 {
	v, ok := m[key]
	if !ok {
		return def
	}
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err == nil {
			return f
		}
	}
	return def
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case string:
		f, err := strconv.ParseFloat(x, 64)
		if err == nil {
			return f
		}
	}
	return 0
}

func round6(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return v
	}
	return math.Round(v*1e6) / 1e6
}

func envIntDefault(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
