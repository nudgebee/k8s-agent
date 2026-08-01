package enrichers

import (
	"context"
	"fmt"
	"math"
	"time"
)

// Multi-window multi-burn-rate SLO evaluation, ported from coroot
// (model/alert.go + watchers/incidents.go on coroot main).
//
// The single-window evaluation in slo.go answers "is the error budget being
// burned faster than 14.4x right now?" over one hour. That fires on any single
// bad hour, including one that has already recovered, and never fires at all on
// a slow burn that still exhausts a 30-day budget.
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

// coveragePoints is how many samples each window is split into when measuring
// data coverage, and minCoverage the fraction that must be present for the
// window to count. Coroot requires at least half the window to contain valid
// data before it will compute a burn rate at all — without this, a low-traffic
// service whose exporter goes quiet for a few minutes reads as a total outage.
const (
	coveragePoints = 12
	minCoverage    = 0.5
	minStepSeconds = 15
)

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

// formatSLOStatus mirrors coroot's BurnRate.FormatSLOStatus. Note this reports
// the LONG WINDOW, not the burn rate divided by 3600 — the latter is always 0.
func (br burnRate) formatSLOStatus() string {
	hours := br.LongWindow / 3600
	unit := "hours"
	if hours == 1 {
		unit = "hour"
	}
	if hours == 0 {
		return fmt.Sprintf("error budget burn rate is %.1fx within %d minutes", br.LongWindowBurnRate, br.LongWindow/60)
	}
	return fmt.Sprintf("error budget burn rate is %.1fx within %d %s", br.LongWindowBurnRate, hours, unit)
}

// windowStats is one workload's good/bad/valid counts over one window, plus how
// much of that window actually reported data.
type windowStats struct {
	good     float64
	bad      float64
	valid    float64
	hasValid bool
	coverage float64
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

// calcBurnRates evaluates every alert rule against the per-window stats and
// returns one burnRate per rule, ported from coroot's calcBurnRates.
//
// A rule is skipped (not reported) when either window lacks the data to judge
// it. A rule is reported with severity OK when it has data but is under
// threshold — the caller needs those to render "how close are we".
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
		if long.coverage < minCoverage || short.coverage < minCoverage {
			continue
		}
		longTotal, shortTotal := long.total(), short.total()
		if longTotal <= 0 || shortTotal <= 0 {
			continue
		}
		br := burnRate{
			LongWindow:  r.LongWindow,
			ShortWindow: r.ShortWindow,
			Threshold:   r.Threshold,
			Severity:    severityOK,
		}
		lr := long.badCount() / longTotal
		sr := short.badCount() / shortTotal
		if math.IsNaN(lr) || math.IsNaN(sr) {
			continue
		}
		br.LongWindowPercentage = lr * 100
		br.ShortWindowPercentage = sr * 100
		br.LongWindowBurnRate = lr / objective
		br.ShortWindowBurnRate = sr / objective
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

// statsForWindow runs the config's SLI queries over one window and returns the
// per-workload counts keyed by "<name>/<namespace>".
//
// The query is evaluated at a step of window/coveragePoints rather than at the
// window itself. Every point is a full-window increase() so the LAST point is
// the value we want; the earlier points exist only so we can measure how much
// of the window reported data at all.
func (s *SLOEnricher) statsForWindow(ctx context.Context, cfg sloConfig, window int, endTime time.Time) (map[string]windowStats, error) {
	wcfg := cfg
	wcfg.Window = window
	queries, err := buildSLOQueries(wcfg)
	if err != nil {
		return nil, err
	}
	step := int64(window / coveragePoints)
	if step < minStepSeconds {
		step = minStepSeconds
	}
	startTime := endTime.Add(-time.Duration(window) * time.Second)

	a := &AppStatsEnricher{q: s.q}
	results := a.runQueries(ctx, queries, startTime, endTime, step, "", "", "")

	out := map[string]windowStats{}
	for _, app := range extractMetricStats(results) {
		ws := windowStats{
			good:     lastOrZero(app.GoodData),
			bad:      lastOrZero(app.BadData),
			coverage: definedFraction(app.ValidData, app.GoodData, app.BadData),
		}
		if app.ValidData != nil {
			if v := app.ValidData.last(); !math.IsNaN(v) && !math.IsInf(v, 0) {
				ws.valid = v
				ws.hasValid = true
			}
		}
		out[app.Name+"/"+app.Namespace] = ws
	}
	return out, nil
}

// burnRatesByApp evaluates every window the alert rules need and returns the
// burn-rate vector per workload key.
//
// Best-effort by design: a window whose query fails is simply absent from
// byWindow, and calcBurnRates skips any rule missing either of its windows.
func (s *SLOEnricher) burnRatesByApp(ctx context.Context, cfg sloConfig, endTime time.Time) map[string][]burnRate {
	byWindow := map[int]map[string]windowStats{}
	for _, w := range requiredWindows() {
		stats, err := s.statsForWindow(ctx, cfg, w, endTime)
		if err != nil {
			continue
		}
		byWindow[w] = stats
	}
	// Collect every workload seen in any window.
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
	return out
}

// attachBurnRates writes the burn-rate vector onto a report and, when the
// vector is non-empty, lets it decide `alert`/`alert_message` instead of the
// legacy single-window comparison.
//
// The legacy fields are always left in place so a backend that predates
// `burn_rates` keeps working against a newer agent.
func attachBurnRates(report map[string]any, rates []burnRate) {
	if len(rates) == 0 {
		return
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

func lastOrZero(ts *timeSeries) float64 {
	if ts == nil {
		return 0
	}
	v := ts.last()
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// definedFraction reports the share of grid points for which at least one of
// the series has a sample. A workload with zero errors legitimately has no
// filter_bad series at all, so coverage is measured across all of them rather
// than on any single one.
func definedFraction(series ...*timeSeries) float64 {
	points, defined := 0, 0
	for _, ts := range series {
		if ts == nil || len(ts.data) == 0 {
			continue
		}
		if len(ts.data) > points {
			points = len(ts.data)
		}
	}
	if points == 0 {
		return 0
	}
	for i := 0; i < points; i++ {
		for _, ts := range series {
			if ts == nil || i >= len(ts.data) {
				continue
			}
			if !math.IsNaN(ts.data[i]) {
				defined++
				break
			}
		}
	}
	return float64(defined) / float64(points)
}
