package enrichers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Multi-window multi-burn-rate SLO evaluation, ported from coroot
// (model/alert.go + watchers/incidents.go on coroot main).
//
// The single-window evaluation in slo.go answers "is the error budget being
// burned faster than 14.4x right now?" over one window. That fires on any
// single bad hour, including one that has already recovered.
//
// The workbook algorithm instead pairs each burn-rate threshold with a long and
// a short window and requires BOTH to exceed the threshold: the long window
// establishes the burn is significant, the short window confirms it is still
// happening. See https://sre.google/workbook/alerting-on-slos/.

const (
	severityOK       = "OK"
	severityWarning  = "WARNING"
	severityCritical = "CRITICAL"
)

// alertRule mirrors coroot's model.AlertRule. Windows are in seconds.
//
// Kept in sync with coroot main model/alert.go:
//
//	{Hour,   5*Minute,  14.4, CRITICAL}
//	{6*Hour, 15*Minute, 6,    CRITICAL}
type alertRule struct {
	LongWindow  int
	ShortWindow int
	Threshold   float64
	Severity    string
}

var alertRules = []alertRule{
	{LongWindow: 3600, ShortWindow: 300, Threshold: 14.4, Severity: severityCritical},
	{LongWindow: 21600, ShortWindow: 900, Threshold: 6, Severity: severityCritical},
}

// Coroot only computes a burn rate over a window when at least half of it
// reported data (totalSum in watchers/incidents.go). Without that, a series
// that reported for a few minutes of an hour is judged as if it covered all of
// it. Coverage is measured by splitting the window into coveragePoints
// sub-steps and counting the ones in which the total series had a sample.
//
// minCoverageStep keeps a sub-step at least a typical scrape interval wide: a
// sub-step shorter than the scrape interval can hold no sample while the series
// is reporting normally, and would read as a gap.
const (
	coveragePoints  = 12
	minCoverage     = 0.5
	minCoverageStep = 60
)

// coverageKey is the query key under which statsForWindow runs the coverage
// query, next to the filter_* keys from buildSLOQueries.
const coverageKey = "coverage"

// burnRate is the wire shape emitted per alert rule. Field names mirror
// coroot's model.BurnRate so the two stay comparable.
type burnRate struct {
	LongWindow            int     `json:"long_window"`
	ShortWindow           int     `json:"short_window"`
	Threshold             float64 `json:"threshold"`
	LongWindowPercentage  float64 `json:"long_window_percentage"`
	ShortWindowPercentage float64 `json:"short_window_percentage"`
	LongWindowBurnRate    float64 `json:"long_window_burn_rate"`
	ShortWindowBurnRate   float64 `json:"short_window_burn_rate"`
	Severity              string  `json:"severity"`
}

// formatSLOStatus mirrors coroot's BurnRate.FormatSLOStatus: it reports the
// rate over the LONG WINDOW.
func (br burnRate) formatSLOStatus() string {
	return formatBurnStatus(br.LongWindowBurnRate, br.LongWindow)
}

// formatBurnStatus names the window a burn rate was measured over. A window that
// is not a whole number of hours is given in minutes (or seconds), so a 5m or
// 90m window never reads "within 0 hours".
func formatBurnStatus(rate float64, window int) string {
	n, unit := window, "second"
	switch {
	case window >= 3600 && window%3600 == 0:
		n, unit = window/3600, "hour"
	case window >= 60 && window%60 == 0:
		n, unit = window/60, "minute"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("error budget burn rate is %.1fx within %d %s", rate, n, unit)
}

// windowStats is one workload's good/bad/valid counts over one window ending at
// the evaluation time, plus how much of that window reported data.
type windowStats struct {
	name, namespace string

	good, bad, valid          float64
	hasGood, hasBad, hasValid bool
	coverage                  float64
}

// total returns the denominator of the SLI. When the config supplies a valid
// series (distribution_cut always does; good_bad_ratio may) that series IS the
// total and "bad" is everything that is not good — matching coroot's
// slowF = total - fast. Otherwise the total is good+bad.
func (w windowStats) total() float64 {
	if w.hasValid {
		return w.valid
	}
	return w.good + w.bad
}

func (w windowStats) badCount() float64 {
	if w.hasValid {
		if b := w.valid - w.good; b > 0 {
			return b
		}
		return 0
	}
	return w.bad
}

// badRatio returns bad/total over the window. ok is false when the window
// cannot be judged: less than half of it reported data, or it saw no events.
func (w windowStats) badRatio() (ratio float64, ok bool) {
	if w.coverage < minCoverage {
		return 0, false
	}
	total := w.total()
	if total <= 0 {
		return 0, false
	}
	return w.badCount() / total, true
}

// legacyStats is the stats map buildSLOReport reads, in the shape
// applicationStats.toResponse gives it: a count is present only when its
// series returned a value, and a valid series turns bad into valid - good.
func (w windowStats) legacyStats() map[string]any {
	out := map[string]any{"name": w.name, "namespace": w.namespace}
	if w.hasGood {
		out["good_data_count"] = w.good
	}
	if w.hasBad {
		out["bad_data_count"] = w.bad
	}
	if w.hasValid {
		out["valid_data_count"] = w.valid
		if w.hasGood {
			out["bad_data_count"] = w.valid - w.good
		}
	}
	return out
}

// calcBurnRates evaluates every alert rule against the per-window stats and
// returns one burnRate per rule it could judge, ported from coroot's
// calcBurnRates.
//
// A rule is skipped (not reported) when either window cannot be judged — not
// enough coverage, or no events. A rule is reported with severity OK when it
// has data but is under threshold, so the caller can render "how close are we".
// Burn rates are rounded to one decimal like the single-window value, and the
// threshold is compared against the rounded figure so a reported 14.4x never
// fires a 14.4x rule.
func calcBurnRates(byWindow map[int]windowStats, goal float64) []burnRate {
	objective := 1 - goal
	if objective <= 0 {
		return nil
	}
	res := make([]burnRate, 0, len(alertRules))
	for _, r := range alertRules {
		long, okLong := byWindow[r.LongWindow]
		short, okShort := byWindow[r.ShortWindow]
		if !okLong || !okShort {
			continue
		}
		lr, okLong := long.badRatio()
		sr, okShort := short.badRatio()
		if !okLong || !okShort {
			continue
		}
		br := burnRate{
			LongWindow:            r.LongWindow,
			ShortWindow:           r.ShortWindow,
			Threshold:             r.Threshold,
			LongWindowPercentage:  round6(lr * 100),
			ShortWindowPercentage: round6(sr * 100),
			LongWindowBurnRate:    round1(lr / objective),
			ShortWindowBurnRate:   round1(sr / objective),
			Severity:              severityOK,
		}
		if br.LongWindowBurnRate > r.Threshold && br.ShortWindowBurnRate > r.Threshold {
			br.Severity = r.Severity
		}
		res = append(res, br)
	}
	return res
}

// worstSeverity returns the highest severity across the rules and the burn rate
// that produced it, so the caller can build the alert message from the window
// that actually fired.
func worstSeverity(rates []burnRate) (string, burnRate) {
	rank := map[string]int{severityOK: 0, severityWarning: 1, severityCritical: 2}
	worst := severityOK
	var firing burnRate
	for _, br := range rates {
		if rank[br.Severity] > rank[worst] {
			worst = br.Severity
			firing = br
		}
	}
	return worst, firing
}

// requiredWindows is the deduplicated set of windows the alert rules need.
func requiredWindows() []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(alertRules)*2)
	for _, r := range alertRules {
		for _, w := range []int{r.LongWindow, r.ShortWindow} {
			if !seen[w] {
				seen[w] = true
				out = append(out, w)
			}
		}
	}
	return out
}

func coverageStep(window int) int {
	return max(window/coveragePoints, minCoverageStep)
}

// coverageQuery counts, per workload, the sub-steps of the window in which the
// SLI's total series reported at least one sample. The total is the valid
// series when the config has one, otherwise good and bad together — a workload
// with zero errors has no bad series at all and must not be penalised for it.
// It returns the query and the number of sub-steps in the window.
func coverageQuery(cfg sloConfig, window int) (string, int, error) {
	step := coverageStep(window)
	presence, err := sloQueries(cfg, step, "count_over_time")
	if err != nil {
		return "", 0, err
	}
	keys := []string{"filter_good", "filter_bad"}
	if _, ok := presence["filter_valid"]; ok {
		keys = []string{"filter_valid"}
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if q, ok := presence[k]; ok {
			parts = append(parts, q)
		}
	}
	q := fmt.Sprintf("count_over_time((%s)[%ds:%ds])", strings.Join(parts, " or "), window, step)
	return q, window / step, nil
}

// statsForWindow runs the config's SLI queries over `window` as instant queries
// at endTime, so every count covers exactly (endTime-window, endTime], and
// returns the per-workload counts keyed by "<name>/<namespace>". With
// withCoverage it also measures each workload's data coverage of the window.
//
// The queries run concurrently. The stats of the queries that succeeded are
// returned even when another failed; err names every failure, and a caller
// must not judge a window from partial results — a missing bad series reads as
// zero errors, a missing good series as zero successes.
func (s *SLOEnricher) statsForWindow(ctx context.Context, cfg sloConfig, window int, endTime time.Time, withCoverage bool) (map[string]windowStats, error) {
	wcfg := cfg
	wcfg.Window = window
	queries, err := buildSLOQueries(wcfg)
	if err != nil {
		return nil, err
	}
	points := 0
	if withCoverage {
		q, n, err := coverageQuery(cfg, window)
		if err != nil {
			return nil, err
		}
		queries[coverageKey] = q
		points = n
	}

	type result struct {
		key     string
		samples map[string]workloadSample
		err     error
	}
	resCh := make(chan result, len(queries))
	var wg sync.WaitGroup
	for key, query := range queries {
		merge := rNanSum
		if key == coverageKey {
			// Series of one workload overlap in time; summing their sub-step
			// counts would report more coverage than the window has.
			merge = rMax
		}
		wg.Go(func() {
			samples, err := s.instantByWorkload(ctx, query, endTime, merge)
			resCh <- result{key: key, samples: samples, err: err}
		})
	}
	wg.Wait()
	close(resCh)

	out := map[string]windowStats{}
	var errs []error
	for r := range resCh {
		if r.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.key, r.err))
			continue
		}
		for k, smp := range r.samples {
			ws := out[k]
			ws.name, ws.namespace = smp.name, smp.namespace
			switch r.key {
			case "filter_good":
				ws.good, ws.hasGood = smp.value, true
			case "filter_bad":
				ws.bad, ws.hasBad = smp.value, true
			case "filter_valid":
				ws.valid, ws.hasValid = smp.value, true
			case coverageKey:
				if points > 0 {
					ws.coverage = math.Min(smp.value/float64(points), 1)
				}
			}
			out[k] = ws
		}
	}
	return out, errors.Join(errs...)
}

// workloadSample is one workload's value from an instant query, merged across
// the series that resolve to it.
type workloadSample struct {
	name, namespace string
	value           float64
}

// instantByWorkload runs one instant query at `at` and returns its samples keyed
// by "<name>/<namespace>", resolved from the labels the same way
// application_stats resolves them. A response that is not a successful vector
// is an error.
func (s *SLOEnricher) instantByWorkload(ctx context.Context, query string, at time.Time, merge func(acc, v float64) float64) (map[string]workloadSample, error) {
	raw, err := s.q.Query(ctx, query, strconv.FormatInt(at.Unix(), 10), "")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string         `json:"resultType"`
			Result     []vectorSeries `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode prometheus response: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("prometheus status %q: %s", resp.Status, resp.Error)
	}
	if resp.Data.ResultType != "vector" {
		return nil, fmt.Errorf("prometheus result type %q, want vector", resp.Data.ResultType)
	}
	out := map[string]workloadSample{}
	for _, r := range resp.Data.Result {
		name, namespace, _, ok := workloadOf(r.Metric)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(r.Value.val, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		key := name + "/" + namespace
		if smp, seen := out[key]; seen {
			v = merge(smp.value, v)
		}
		out[key] = workloadSample{name: name, namespace: namespace, value: v}
	}
	return out, nil
}

// burnRatesByApp evaluates every alert rule per workload, querying all the
// windows the rules need concurrently.
//
// ok is false when the burn rates could not be evaluated — the goal leaves no
// error budget, or any query failed — and the caller must keep the
// single-window verdict. When ok is true the result is authoritative: a
// workload with no rates had no window that could be judged (e.g. no traffic
// in the last 5 minutes), which is not a burn.
func (s *SLOEnricher) burnRatesByApp(ctx context.Context, cfg sloConfig, endTime time.Time) (map[string][]burnRate, bool) {
	if cfg.Goal >= 1 {
		return nil, false
	}
	windows := requiredWindows()
	byWindow := make(map[int]map[string]windowStats, len(windows))
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []error
	)
	for _, w := range windows {
		wg.Go(func() {
			stats, err := s.statsForWindow(ctx, cfg, w, endTime, true)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("window %ds: %w", w, err))
				return
			}
			byWindow[w] = stats
		})
	}
	wg.Wait()
	if len(errs) > 0 {
		slog.Warn("slo_generator: burn-rate evaluation failed, keeping the single-window verdict",
			"slo", cfg.Name, "err", errors.Join(errs...))
		return nil, false
	}

	keys := map[string]bool{}
	for _, stats := range byWindow {
		for k := range stats {
			keys[k] = true
		}
	}
	out := make(map[string][]burnRate, len(keys))
	for k := range keys {
		perWindow := map[int]windowStats{}
		for w, stats := range byWindow {
			if ws, ok := stats[k]; ok {
				perWindow[w] = ws
			}
		}
		if rates := calcBurnRates(perWindow, cfg.Goal); len(rates) > 0 {
			out[k] = rates
		}
	}
	return out, true
}

// attachBurnRates writes an authoritative burn-rate evaluation onto a report:
// `burn_rates` and `severity`, and `alert`/`alert_message` derived from them
// instead of the single-window comparison. Call it only when burnRatesByApp
// returned ok. An empty `rates` means no rule could be judged, which reports OK.
//
// A report already marked invalid (no data in its window) is left alone: its
// alert stays off and it carries no severity that would contradict NO_DATA.
func attachBurnRates(report map[string]any, rates []burnRate) {
	if valid, _ := report["valid"].(bool); !valid {
		return
	}
	if rates == nil {
		rates = []burnRate{}
	}
	report["burn_rates"] = rates
	severity, firing := worstSeverity(rates)
	report["severity"] = severity
	report["alert"] = severity != severityOK
	if severity != severityOK {
		report["alert_message"] = firing.formatSLOStatus()
	} else {
		report["alert_message"] = ""
	}
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}
