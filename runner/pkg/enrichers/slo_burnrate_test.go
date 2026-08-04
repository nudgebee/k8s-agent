package enrichers

import (
	"math"
	"testing"
)

// goal 0.99 => objective 0.01, so a bad-request ratio of 0.20 is a 20x burn.
const testGoal = 0.99

func ws(good, bad, coverage float64) windowStats {
	return windowStats{good: good, bad: bad, coverage: coverage}
}

// 1-0.99 is not exactly 0.01 in float64, so burn rates land a few ulps off.
func nearly(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v; want %v", label, got, want)
	}
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
	nearly(t, "long burn rate", br.LongWindowBurnRate, 20)
	nearly(t, "short burn rate", br.ShortWindowBurnRate, 30)
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
	nearly(t, "long burn rate (still reported)", br.LongWindowBurnRate, 20)
}

// Coroot refuses to compute a burn rate unless at least half the window
// reported data — without this a low-traffic service whose exporter goes quiet
// reads as a total outage.
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
		return windowStats{valid: valid, good: good, hasValid: true, coverage: coverage}
	}
	br := oneRule(t, calcBurnRates(map[int]windowStats{
		3600: withValid(100, 80, 1), // 20 slow => 20x
		300:  withValid(100, 70, 1), // 30 slow => 30x
	}, testGoal))

	if br.Severity != severityCritical {
		t.Errorf("severity = %q; want %q", br.Severity, severityCritical)
	}
	nearly(t, "long burn rate", br.LongWindowBurnRate, 20)
}

func TestCalcBurnRates_GoalOfOneYieldsNothing(t *testing.T) {
	// objective = 0 would divide by zero.
	if rates := calcBurnRates(map[int]windowStats{3600: ws(80, 20, 1), 300: ws(70, 30, 1)}, 1); rates != nil {
		t.Errorf("rates = %+v; want nil for goal=1", rates)
	}
}

// Regression guard: an earlier port divided the burn RATE by 3600, so every
// message read "within 0 hours". The window is what gets formatted.
func TestFormatSLOStatus_UsesLongWindow(t *testing.T) {
	for _, tc := range []struct {
		long int
		want string
	}{
		{3600, "error budget burn rate is 20.0x within 1 hour"},
		{21600, "error budget burn rate is 20.0x within 6 hours"},
		{300, "error budget burn rate is 20.0x within 5 minutes"},
	} {
		got := burnRate{LongWindow: tc.long, LongWindowBurnRate: 20}.formatSLOStatus()
		if got != tc.want {
			t.Errorf("long=%d: got %q; want %q", tc.long, got, tc.want)
		}
	}
}

func TestDefinedFraction(t *testing.T) {
	ts := newTimeSeries(0, 4, 1)
	ts.set(1, 1)
	ts.set(3, 2)

	if got := definedFraction(ts, nil); got != 0.5 {
		t.Errorf("definedFraction = %v; want 0.5", got)
	}
	if got := definedFraction(nil, nil); got != 0 {
		t.Errorf("definedFraction(nil) = %v; want 0", got)
	}

	// A second series covering the gaps lifts coverage to full — a workload
	// with zero errors has no filter_bad series and must not be penalised.
	other := newTimeSeries(0, 4, 1)
	other.set(0, 1)
	other.set(2, 1)
	if got := definedFraction(ts, other); got != 1 {
		t.Errorf("definedFraction(both) = %v; want 1", got)
	}
}

func TestAttachBurnRates_OverridesLegacyAlert(t *testing.T) {
	// Legacy single-window evaluation said "alert"; the burn-rate vector says
	// the short window has recovered, so the report must not fire.
	report := map[string]any{"alert": true, "alert_message": "stale"}
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

func TestAttachBurnRates_EmptyKeepsLegacyFields(t *testing.T) {
	// No burn rates (e.g. Prometheus hiccup) must leave the legacy decision
	// alone rather than blanking the SLO.
	report := map[string]any{"alert": true, "alert_message": "legacy"}
	attachBurnRates(report, nil)

	if report["alert"] != true || report["alert_message"] != "legacy" {
		t.Errorf("legacy fields modified: %+v", report)
	}
	if _, ok := report["burn_rates"]; ok {
		t.Error("burn_rates should be absent")
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
