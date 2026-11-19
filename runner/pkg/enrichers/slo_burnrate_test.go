package enrichers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// goal 0.99 => objective 0.01, so a bad-request ratio of 0.20 is a 20x burn.
const testGoal = 0.99

func ws(good, bad, coverage float64) windowStats {
	return windowStats{good: good, bad: bad, hasGood: true, hasBad: true, coverage: coverage}
}

// Only the 1h/5m rule can be evaluated when byWindow holds just those two
// windows; the 6h/15m rule is skipped for lack of data. That keeps these tests
// pinned to one rule.
func oneRule(t *testing.T, rates []burnRate) burnRate {
	t.Helper()
	if len(rates) != 1 {
		t.Fatalf("rates = %d; want 1: %+v", len(rates), rates)
	}
	return rates[0]
}

func TestCalcBurnRates_BothWindowsOverThresholdFires(t *testing.T) {
	br := oneRule(t, calcBurnRates(map[int]windowStats{
		3600: ws(80, 20, 1), // 20% bad => 20x
		300:  ws(70, 30, 1), // 30% bad => 30x
	}, testGoal))

	if br.Severity != severityCritical {
		t.Errorf("severity = %q; want %q", br.Severity, severityCritical)
	}
	if br.LongWindowBurnRate != 20 || br.ShortWindowBurnRate != 30 {
		t.Errorf("burn rates = %v/%v; want 20/30", br.LongWindowBurnRate, br.ShortWindowBurnRate)
	}
	if br.LongWindowPercentage != 20 || br.ShortWindowPercentage != 30 {
		t.Errorf("percentages = %v/%v; want 20/30", br.LongWindowPercentage, br.ShortWindowPercentage)
	}
}

// The reason the port exists: a bad hour that has already recovered must not
// fire. Single-window evaluation alerts here; multi-window does not.
func TestCalcBurnRates_ShortWindowRecoveredDoesNotFire(t *testing.T) {
	br := oneRule(t, calcBurnRates(map[int]windowStats{
		3600: ws(80, 20, 1), // 20x over the hour
		300:  ws(999, 1, 1), // 0.1x right now — already recovered
	}, testGoal))

	if br.Severity != severityOK {
		t.Errorf("severity = %q; want %q (short window recovered)", br.Severity, severityOK)
	}
	if br.LongWindowBurnRate != 20 {
		t.Errorf("long burn rate = %v; want 20 (still reported)", br.LongWindowBurnRate)
	}
}

// A short window with no events cannot be judged, so its rule is not reported
// (coroot skips it the same way).
func TestCalcBurnRates_NoTrafficInShortWindowSkipsRule(t *testing.T) {
	rates := calcBurnRates(map[int]windowStats{
		3600: ws(80, 20, 1),
		300:  ws(0, 0, 1),
	}, testGoal)
	if len(rates) != 0 {
		t.Errorf("rates = %+v; want none (no events in the 5m window)", rates)
	}
}

// Coroot refuses to compute a burn rate unless at least half the window
// reported data.
func TestCalcBurnRates_LowCoverageSkipsRule(t *testing.T) {
	rates := calcBurnRates(map[int]windowStats{
		3600: ws(80, 20, 0.4),
		300:  ws(70, 30, 1),
	}, testGoal)
	if len(rates) != 0 {
		t.Errorf("rates = %+v; want none (long window coverage 0.4 < 0.5)", rates)
	}
}

func TestCalcBurnRates_MissingWindowSkipsRule(t *testing.T) {
	rates := calcBurnRates(map[int]windowStats{
		3600: ws(80, 20, 1),
	}, testGoal)
	if len(rates) != 0 {
		t.Errorf("rates = %+v; want none (no 5m window)", rates)
	}
}

// distribution_cut supplies valid (total) + good (fast); bad is the remainder,
// mirroring coroot's slowF = total - fast.
func TestCalcBurnRates_ValidSeriesDerivesBad(t *testing.T) {
	withValid := func(valid, good, coverage float64) windowStats {
		return windowStats{valid: valid, good: good, hasValid: true, hasGood: true, coverage: coverage}
	}
	br := oneRule(t, calcBurnRates(map[int]windowStats{
		3600: withValid(100, 80, 1), // 20 slow => 20x
		300:  withValid(100, 70, 1), // 30 slow => 30x
	}, testGoal))

	if br.Severity != severityCritical {
		t.Errorf("severity = %q; want %q", br.Severity, severityCritical)
	}
	if br.LongWindowBurnRate != 20 {
		t.Errorf("long burn rate = %v; want 20", br.LongWindowBurnRate)
	}
}

// Burn rates are rounded to one decimal, and the threshold is compared against
// the rounded value, so a reported 14.4x never fires a 14.4x rule.
func TestCalcBurnRates_RoundsBeforeComparing(t *testing.T) {
	// 14.44% bad at goal 0.99 is a 14.44x burn, reported as 14.4x.
	br := oneRule(t, calcBurnRates(map[int]windowStats{
		3600: ws(8556, 1444, 1),
		300:  ws(8556, 1444, 1),
	}, testGoal))
	if br.LongWindowBurnRate != 14.4 {
		t.Errorf("long burn rate = %v; want 14.4", br.LongWindowBurnRate)
	}
	if br.Severity != severityOK {
		t.Errorf("severity = %q; want %q (14.4x is not over 14.4x)", br.Severity, severityOK)
	}
}

func TestCalcBurnRates_GoalOfOneYieldsNothing(t *testing.T) {
	// objective = 0 would divide by zero.
	if rates := calcBurnRates(map[int]windowStats{3600: ws(80, 20, 1), 300: ws(70, 30, 1)}, 1); rates != nil {
		t.Errorf("rates = %+v; want nil for goal=1", rates)
	}
}

// Regression guard: an earlier port divided the burn RATE by 3600, so every
// message read "within 0 hours". The window is what gets formatted, in minutes
// when it is not a whole number of hours.
func TestFormatBurnStatus(t *testing.T) {
	for _, tc := range []struct {
		window int
		want   string
	}{
		{3600, "error budget burn rate is 20.0x within 1 hour"},
		{21600, "error budget burn rate is 20.0x within 6 hours"},
		{300, "error budget burn rate is 20.0x within 5 minutes"},
		{5400, "error budget burn rate is 20.0x within 90 minutes"},
		{60, "error budget burn rate is 20.0x within 1 minute"},
		{45, "error budget burn rate is 20.0x within 45 seconds"},
	} {
		if got := formatBurnStatus(20, tc.window); got != tc.want {
			t.Errorf("window=%d: got %q; want %q", tc.window, got, tc.want)
		}
	}
	if got := (burnRate{LongWindow: 21600, ShortWindow: 900, LongWindowBurnRate: 7}).formatSLOStatus(); got != "error budget burn rate is 7.0x within 6 hours" {
		t.Errorf("formatSLOStatus = %q; want the long window", got)
	}
}

func TestCoverageQuery(t *testing.T) {
	gbr := sloConfig{
		Method:     "good_bad_ratio",
		GroupBy:    "destination_workload_name, destination_workload_namespace",
		FilterGood: `reqs{status="200"}`,
		FilterBad:  `reqs{status="500"}`,
	}
	q, points, err := coverageQuery(gbr, 3600)
	if err != nil {
		t.Fatal(err)
	}
	want := `count_over_time((` +
		`sum by (destination_workload_name, destination_workload_namespace)(count_over_time(reqs{status="200"}[300s]))` +
		` or ` +
		`sum by (destination_workload_name, destination_workload_namespace)(count_over_time(reqs{status="500"}[300s]))` +
		`)[3600s:300s])`
	if q != want {
		t.Errorf("good_bad_ratio coverage query:\n got  %s\n want %s", q, want)
	}
	if points != 12 {
		t.Errorf("points = %d; want 12", points)
	}

	// The 5m window is split into 60s sub-steps, not 25s ones a 30s or 60s
	// scrape interval would leave empty.
	if _, points, _ := coverageQuery(gbr, 300); points != 5 {
		t.Errorf("5m window points = %d; want 5 (60s sub-steps)", points)
	}

	// With a valid series, coverage is measured on it alone — it is the total.
	dc := sloConfig{
		Method:          "distribution_cut",
		GroupBy:         "service",
		Expression:      `lat_bucket{job="x"}`,
		ThresholdBucket: 0.5,
	}
	q, _, err = coverageQuery(dc, 900)
	if err != nil {
		t.Fatal(err)
	}
	if want := `count_over_time((sum by (service)(count_over_time(lat_count{job="x"}[75s])))[900s:75s])`; q != want {
		t.Errorf("distribution_cut coverage query:\n got  %s\n want %s", q, want)
	}
}

func TestLegacyStats(t *testing.T) {
	got := windowStats{name: "web", namespace: "shop", good: 80, valid: 100, hasGood: true, hasValid: true}.legacyStats()
	if got["good_data_count"] != 80.0 || got["valid_data_count"] != 100.0 || got["bad_data_count"] != 20.0 {
		t.Errorf("valid series: %+v; want good 80, valid 100, bad 20", got)
	}
	got = windowStats{name: "web", namespace: "shop", good: 5, hasGood: true}.legacyStats()
	if _, ok := got["bad_data_count"]; ok {
		t.Errorf("bad_data_count present without a bad series: %+v", got)
	}
	if got["name"] != "web" || got["namespace"] != "shop" {
		t.Errorf("identity = %v/%v; want web/shop", got["name"], got["namespace"])
	}
}

func TestAttachBurnRates_OverridesLegacyAlert(t *testing.T) {
	// Legacy single-window evaluation said "alert"; the burn-rate vector says
	// the short window has recovered, so the report must not fire.
	report := map[string]any{"valid": true, "alert": true, "alert_message": "stale"}
	attachBurnRates(report, []burnRate{{LongWindow: 3600, ShortWindow: 300, Threshold: 14.4, Severity: severityOK}})

	if report["alert"] != false {
		t.Errorf("alert = %v; want false", report["alert"])
	}
	if report["severity"] != severityOK {
		t.Errorf("severity = %v; want %q", report["severity"], severityOK)
	}
	if report["alert_message"] != "" {
		t.Errorf("alert_message = %q; want empty", report["alert_message"])
	}
	if _, ok := report["burn_rates"]; !ok {
		t.Error("burn_rates missing from report")
	}
}

// An evaluation in which no rule could be judged is still authoritative: it
// reports OK rather than falling back to the single-window alert.
func TestAttachBurnRates_NoJudgedRuleIsOK(t *testing.T) {
	report := map[string]any{"valid": true, "alert": true, "alert_message": "legacy"}
	attachBurnRates(report, nil)

	if report["alert"] != false || report["severity"] != severityOK {
		t.Errorf("alert/severity = %v/%v; want false/OK", report["alert"], report["severity"])
	}
	rates, ok := report["burn_rates"].([]burnRate)
	if !ok || rates == nil || len(rates) != 0 {
		t.Errorf("burn_rates = %#v; want an empty, non-nil slice", report["burn_rates"])
	}
}

func TestAttachBurnRates_InvalidReportUntouched(t *testing.T) {
	report := map[string]any{"valid": false, "alert": false}
	attachBurnRates(report, []burnRate{{LongWindow: 3600, ShortWindow: 300, Threshold: 14.4, Severity: severityCritical}})

	if report["alert"] != false {
		t.Errorf("alert = %v; want false on an invalid report", report["alert"])
	}
	for _, k := range []string{"severity", "burn_rates"} {
		if _, ok := report[k]; ok {
			t.Errorf("%s set on an invalid report: %+v", k, report)
		}
	}
}

func TestRequiredWindows(t *testing.T) {
	got := requiredWindows()
	want := map[int]bool{}
	for _, r := range alertRules {
		want[r.LongWindow] = true
		want[r.ShortWindow] = true
	}
	if len(got) != len(want) {
		t.Fatalf("requiredWindows = %v; want %d distinct windows", got, len(want))
	}
	for _, w := range got {
		if !want[w] {
			t.Errorf("unexpected window %d", w)
		}
	}
}

// sloProm is a fake Prometheus for slo_generator. It answers instant queries
// for one workload, web/shop, from per-window event counts, and records every
// query with its evaluation time.
type sloProm struct {
	// good and bad hold the increase() result per window in seconds. A window
	// missing from the map returns no series, as when nothing reported.
	good, bad map[int]float64
	// coverage is the share of a window's sub-steps that had data. A window
	// missing from the map is fully covered.
	coverage map[int]float64
	// fail makes the query for "<good|bad|coverage>/<window>" return an error.
	fail map[string]bool

	mu    sync.Mutex
	calls []sloPromCall
}

type sloPromCall struct{ query, at string }

var (
	sloCoverageRe = regexp.MustCompile(`^count_over_time\(\(.*\)\[(\d+)s:(\d+)s\]\)$`)
	sloIncreaseRe = regexp.MustCompile(`increase\(.*\[(\d+)s\]\)`)
)

func (p *sloProm) Query(_ context.Context, q, at, _ string) ([]byte, error) {
	p.mu.Lock()
	p.calls = append(p.calls, sloPromCall{query: q, at: at})
	p.mu.Unlock()

	var (
		series string
		window int
		value  float64
		ok     bool
	)
	if m := sloCoverageRe.FindStringSubmatch(q); m != nil {
		window, _ = strconv.Atoi(m[1])
		step, _ := strconv.Atoi(m[2])
		share, set := p.coverage[window]
		if !set {
			share = 1
		}
		series, value, ok = "coverage", share*float64(window/step), share > 0
	} else if m := sloIncreaseRe.FindStringSubmatch(q); m != nil {
		window, _ = strconv.Atoi(m[1])
		series = "good"
		counts := p.good
		if strings.Contains(q, `status="500"`) {
			series, counts = "bad", p.bad
		}
		value, ok = counts[window]
	} else {
		return nil, fmt.Errorf("sloProm: unexpected query %s", q)
	}
	if p.fail[fmt.Sprintf("%s/%d", series, window)] {
		return nil, errors.New("query timed out")
	}
	result := ""
	if ok {
		result = fmt.Sprintf(`{"metric":{"destination_workload_name":"web","destination_workload_namespace":"shop"},"value":[%s,"%g"]}`, at, value)
	}
	return []byte(`{"status":"success","data":{"resultType":"vector","result":[` + result + `]}}`), nil
}

func (p *sloProm) QueryRange(_ context.Context, q, _, _, _, _ string) ([]byte, error) {
	return nil, fmt.Errorf("sloProm: slo_generator should not run range queries: %s", q)
}

func (p *sloProm) LabelValues(_ context.Context, _, _, _ string, _ []string) ([]byte, error) {
	return []byte(`{"status":"success","data":[]}`), nil
}

// everyWindow returns the same count for the report window and every window
// the alert rules need.
func everyWindow(v float64) map[int]float64 {
	return map[int]float64{3600: v, 300: v, 21600: v, 900: v}
}

func runSLO(t *testing.T, p *sloProm, window int) map[string]any {
	t.Helper()
	resp, err := (&SLOEnricher{q: p}).Handler()(context.Background(), map[string]any{
		"slo_config": map[string]any{
			"name":        "availability",
			"goal":        testGoal,
			"method":      "good_bad_ratio",
			"filter_good": `container_http_requests_total{status="200"}`,
			"filter_bad":  `container_http_requests_total{status="500"}`,
			"window":      window,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp.(map[string]any)
}

func onlyReport(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	if resp["success"] != true {
		t.Fatalf("success = %v: %+v", resp["success"], resp)
	}
	reports := resp["data"].([]map[string]any)
	if len(reports) != 1 {
		t.Fatalf("reports = %d; want 1: %+v", len(reports), reports)
	}
	return reports[0]
}

// Review case: the last hour burned at 20x, but nothing reported in the last
// 5 minutes. The burn has stopped, so the SLO must not fire — it used to fall
// back to the single-window alert, which did.
func TestSLOGenerator_NoTrafficInShortWindowDoesNotFire(t *testing.T) {
	for name, tc := range map[string]struct {
		short    map[int]float64
		coverage map[int]float64
	}{
		// The series kept reporting; the counters just did not move.
		"zero increase": {short: map[int]float64{300: 0, 900: 0}},
		// The series stopped reporting altogether.
		"no series": {coverage: map[int]float64{300: 0, 900: 0}},
	} {
		t.Run(name, func(t *testing.T) {
			good := map[int]float64{3600: 80, 21600: 4980}
			bad := map[int]float64{3600: 20, 21600: 20}
			for w, v := range tc.short {
				good[w], bad[w] = v, v
			}
			rep := onlyReport(t, runSLO(t, &sloProm{good: good, bad: bad, coverage: tc.coverage}, 3600))

			if rep["error_budget_burn_rate"] != 20.0 {
				t.Fatalf("single-window burn rate = %v; want 20 (the case under test)", rep["error_budget_burn_rate"])
			}
			if rep["alert"] != false || rep["severity"] != severityOK {
				t.Errorf("alert/severity = %v/%v; want false/OK", rep["alert"], rep["severity"])
			}
			if rates, _ := rep["burn_rates"].([]burnRate); rates == nil || len(rates) != 0 {
				t.Errorf("burn_rates = %#v; want empty (no rule could be judged)", rep["burn_rates"])
			}
		})
	}
}

// Review case: a query failure inside the burn pass must not produce a
// verdict, in either direction; the report keeps the single-window result.
func TestSLOGenerator_BurnQueryFailureKeepsSingleWindowVerdict(t *testing.T) {
	for name, tc := range map[string]struct {
		good, bad map[int]float64
		fail      string
		wantAlert bool
	}{
		// A healthy service whose 5m good query fails would read as 100% errors.
		"healthy, good query fails": {everyWindow(999), everyWindow(1), "good/300", false},
		// A real 30x burn whose 5m bad query fails would read as no errors.
		"burning, bad query fails": {everyWindow(70), everyWindow(30), "bad/300", true},
		// Coverage is part of the verdict too.
		"burning, coverage fails": {everyWindow(70), everyWindow(30), "coverage/21600", true},
	} {
		t.Run(name, func(t *testing.T) {
			p := &sloProm{good: tc.good, bad: tc.bad, fail: map[string]bool{tc.fail: true}}
			rep := onlyReport(t, runSLO(t, p, 3600))

			if rep["alert"] != tc.wantAlert {
				t.Errorf("alert = %v; want %v (single-window verdict)", rep["alert"], tc.wantAlert)
			}
			for _, k := range []string{"burn_rates", "severity"} {
				if _, ok := rep[k]; ok {
					t.Errorf("%s = %v; want absent when the burn pass failed", k, rep[k])
				}
			}
		})
	}
}

// A failed query for the report's own window fails the run instead of
// reporting an SLI computed without that series.
func TestSLOGenerator_ReportQueryFailureFailsRun(t *testing.T) {
	p := &sloProm{good: everyWindow(999), bad: everyWindow(1), fail: map[string]bool{"bad/3600": true}}
	resp := runSLO(t, p, 3600)
	if resp["success"] != false {
		t.Fatalf("success = %v; want false: %+v", resp["success"], resp)
	}
	if msg, _ := resp["msg"].(string); !strings.Contains(msg, "filter_bad") {
		t.Errorf("msg = %q; want it to name the failed query", msg)
	}
}

// Review case: every count must cover the window ending at the report's
// end_time. Range queries laid on a grid that drops the end point read the
// window one step earlier — the previous hour for the single-window fields.
func TestSLOGenerator_EvaluatesEveryWindowAtEndTime(t *testing.T) {
	p := &sloProm{good: everyWindow(70), bad: everyWindow(30)}
	rep := onlyReport(t, runSLO(t, p, 3600))

	endTime := strconv.FormatInt(int64(rep["end_time"].(float64)), 10)
	windows := map[string]bool{}
	for _, c := range p.calls {
		if c.at != endTime {
			t.Errorf("query evaluated at %s; want end_time %s: %s", c.at, endTime, c.query)
		}
		if m := sloIncreaseRe.FindStringSubmatch(c.query); m != nil && !sloCoverageRe.MatchString(c.query) {
			windows[m[1]] = true
		}
	}
	got := slices.Sorted(maps.Keys(windows))
	if want := []string{"21600", "300", "3600", "900"}; !slices.Equal(got, want) {
		t.Errorf("increase windows = %v; want %v", got, want)
	}
	if rep["start_time"].(float64) != rep["end_time"].(float64)-3600 {
		t.Errorf("start_time = %v; want end_time-3600", rep["start_time"])
	}
}

// Review case: a report with no data in its own window is NO_DATA on the
// backend, so the burn pass must not stamp it with a severity or alert.
func TestSLOGenerator_InvalidReportIgnoresBurnRates(t *testing.T) {
	// The report window is 5m with no events; the 6h/15m rule burns at 30x.
	good := map[int]float64{300: 0, 900: 70, 3600: 70, 21600: 70}
	bad := map[int]float64{300: 0, 900: 30, 3600: 30, 21600: 30}
	rep := onlyReport(t, runSLO(t, &sloProm{good: good, bad: bad}, 300))

	if rep["valid"] != false {
		t.Fatalf("valid = %v; want false (no events in the 5m report window)", rep["valid"])
	}
	if rep["alert"] != false {
		t.Errorf("alert = %v; want false", rep["alert"])
	}
	for _, k := range []string{"severity", "burn_rates"} {
		if _, ok := rep[k]; ok {
			t.Errorf("%s = %v; want absent on an invalid report", k, rep[k])
		}
	}
}

// Coverage reaches the verdict: with only 2 of the 5m window's 5 sub-steps
// reporting, the 1h/5m rule is not judged, and the 6h/15m rule decides.
func TestSLOGenerator_LowCoverageSkipsRule(t *testing.T) {
	p := &sloProm{good: everyWindow(70), bad: everyWindow(30), coverage: map[int]float64{300: 0.4}}
	rep := onlyReport(t, runSLO(t, p, 3600))

	rates := rep["burn_rates"].([]burnRate)
	if len(rates) != 1 || rates[0].LongWindow != 21600 {
		t.Fatalf("burn_rates = %+v; want only the 6h/15m rule", rates)
	}
	if rep["severity"] != severityCritical || rep["alert"] != true {
		t.Errorf("severity/alert = %v/%v; want CRITICAL/true", rep["severity"], rep["alert"])
	}
	if want := "error budget burn rate is 30.0x within 6 hours"; rep["alert_message"] != want {
		t.Errorf("alert_message = %q; want %q", rep["alert_message"], want)
	}
}

// With no error budget there is nothing to burn: the pass runs no queries and
// the single-window verdict stands.
func TestSLOGenerator_GoalOfOneSkipsBurnPass(t *testing.T) {
	p := &sloProm{good: everyWindow(70), bad: everyWindow(30)}
	resp, err := (&SLOEnricher{q: p}).Handler()(context.Background(), map[string]any{
		"slo_config": map[string]any{
			"name":        "availability",
			"goal":        1.0,
			"method":      "good_bad_ratio",
			"filter_good": `container_http_requests_total{status="200"}`,
			"filter_bad":  `container_http_requests_total{status="500"}`,
			"window":      3600,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep := onlyReport(t, resp.(map[string]any))
	if len(p.calls) != 2 {
		t.Errorf("queries = %d; want 2 (report window only)", len(p.calls))
	}
	if _, ok := rep["burn_rates"]; ok {
		t.Errorf("burn_rates = %v; want absent", rep["burn_rates"])
	}
}
