package enrichers

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestSLOGenerator_GoodBadRatio runs slo_generator end-to-end against a fake
// Prometheus that returns one good event and one bad event for the same
// workload. The expected SLO calculation:
//
//	sli = good / (good+bad) = 1/2 = 0.5
//	gap = sli - goal = 0.5 - 0.99 = -0.49
//	eb_target = 1 - 0.99 = 0.01
//	eb_value  = 1 - 0.5  = 0.5
//	eb_burn_rate = round(eb_value / eb_target, 1) = 50.0
//
// The shape is what the backend reads back via
// resp.data.data → list of SLOReport dicts.
func TestSLOGenerator_GoodBadRatio(t *testing.T) {
	// Sample timestamp must land inside the [now-3600s, now] window so the
	// TimeSeries grid actually picks it up. Using now-30s satisfies that
	// regardless of when the test runs.
	now := time.Now().UTC().Unix()
	matrix := func(value string) []byte {
		ts := strconv.FormatInt(now-30, 10)
		return []byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"destination_workload_name":"web","destination_workload_namespace":"shop"},"values":[[` + ts + `,"` + value + `"]]}]}}`)
	}
	// anyMatchProm responds based on whether the query has the bad-status
	// label so fmtSLOQuery's wrapper string stays opaque to the test.
	s := &SLOEnricher{q: anyMatchProm{good: matrix("1"), bad: matrix("1")}}

	resp, err := s.Handler()(context.Background(), map[string]any{
		"slo_config": map[string]any{
			"name":        "availability",
			"goal":        0.99,
			"method":      "good_bad_ratio",
			"filter_good": `container_http_requests_total{status="200"}`,
			"filter_bad":  `container_http_requests_total{status="500"}`,
			"window":      3600,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := resp.(map[string]any)
	if r["success"] != true {
		t.Fatalf("success = %v: %+v", r["success"], r)
	}
	reports := r["data"].([]map[string]any)
	if len(reports) != 1 {
		t.Fatalf("reports = %d; want 1: %+v", len(reports), reports)
	}
	rep := reports[0]
	if rep["sli_measurement"] != 0.5 {
		t.Errorf("sli_measurement = %v; want 0.5", rep["sli_measurement"])
	}
	if rep["alert"] != true {
		// burn_rate=50 is well above default threshold 14.4
		t.Errorf("alert = %v; want true (burn rate 50 > 14.4)", rep["alert"])
	}
}

// anyMatchProm returns a fixed response for any query — distinguishing the
// "good" branch (the first one substituted into the filter_good map).
// We set the same body for both ranges since the test only cares about counts.
type anyMatchProm struct {
	good []byte
	bad  []byte
}

func (a anyMatchProm) Query(_ context.Context, _, _, _ string) ([]byte, error) {
	return a.good, nil
}
func (a anyMatchProm) QueryRange(_ context.Context, q, _, _, _, _ string) ([]byte, error) {
	if contains(q, "status=\"500\"") {
		return a.bad, nil
	}
	return a.good, nil
}
func (a anyMatchProm) LabelValues(_ context.Context, _, _, _ string, _ []string) ([]byte, error) {
	return []byte(`{"status":"success","data":[]}`), nil
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestFmtSLOQuery_Defaults(t *testing.T) {
	q := fmtSLOQuery(`up`, 60, []string{"increase", "sum by (workload)"}, nil)
	want := `sum by (workload)(increase(up[60s]))`
	if q != want {
		t.Errorf("got %q; want %q", q, want)
	}
}

// The le matcher must be a valid PromQL/MetricsQL string literal: those use Go
// escape rules, so a lone `\.` fails to parse and the SLO gets no value (#39800).
func TestFmtSLOQuery_LeRegex(t *testing.T) {
	q := fmtSLOQuery(`http_buckets{job="x"}`, 60, []string{"increase"}, map[string]string{"le": "0.5"})
	want := `increase(http_buckets{job="x", le=~"^0\\.5(\\.0+)?$"}[60s])`
	if q != want {
		t.Fatalf("got  %s\nwant %s", q, want)
	}

	for le, cases := range map[string]struct{ match, noMatch []string }{
		"0.5": {match: []string{"0.5"}, noMatch: []string{"0x5", "0.55", "10.5"}},
		"1":   {match: []string{"1", "1.0", "1.00"}, noMatch: []string{"10", "1.5", "11"}},
	} {
		re := leMatcher(t, fmtSLOQuery(`b{job="x"}`, 60, nil, map[string]string{"le": le}))
		for _, v := range cases.match {
			if !re.MatchString(v) {
				t.Errorf("bucket %s: %q should match %q", le, v, re)
			}
		}
		for _, v := range cases.noMatch {
			if re.MatchString(v) {
				t.Errorf("bucket %s: %q should not match %q", le, v, re)
			}
		}
	}
}

// leMatcher reads the le=~"..." literal back the way a PromQL parser does (Go
// string-literal rules) and compiles it.
func leMatcher(t *testing.T, q string) *regexp.Regexp {
	t.Helper()
	start := strings.Index(q, `le=~`) + len(`le=~`)
	end := strings.Index(q[start:], `"}`) + start + 1
	pattern, err := strconv.Unquote(q[start:end])
	if err != nil {
		t.Fatalf("le matcher %s is not a valid string literal: %v", q[start:end], err)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("le matcher %q is not a valid regex: %v", pattern, err)
	}
	return re
}

func TestBuildSLOQueries_DistributionCutRequiresExpression(t *testing.T) {
	cfg := sloConfig{Method: "distribution_cut", Window: 60}
	if _, err := buildSLOQueries(cfg); err == nil {
		t.Error("expected error for missing expression+threshold_bucket")
	}
}
