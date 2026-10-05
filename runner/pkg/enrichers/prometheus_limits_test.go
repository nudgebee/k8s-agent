package enrichers

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := parseTimestamp(s)
	if err != nil {
		t.Fatalf("parseTimestamp(%q): %v", s, err)
	}
	return ts
}

// TestClampStep_WidensOnlyBeyondCap pins the rule that a range query never
// asks for more than maxRangePoints samples per series, and that requests
// already under the cap are passed through untouched.
func TestClampStep_WidensOnlyBeyondCap(t *testing.T) {
	cases := []struct {
		name       string
		start, end string
		step       string
		wantSame   bool
	}{
		{"1h at 60s", "2026-09-14 08:00:00 UTC", "2026-09-14 09:00:00 UTC", "60", true},
		{"24h at 60s", "2026-09-14 08:00:00 UTC", "2026-09-15 08:00:00 UTC", "60", true},
		// 7d at 60s is 10080 points — just under the cap.
		{"7d at 60s", "2026-09-08 08:00:00 UTC", "2026-09-15 08:00:00 UTC", "60", true},
		// 30d at 60s is 43200 points — must widen.
		{"30d at 60s", "2026-08-16 08:00:00 UTC", "2026-09-15 08:00:00 UTC", "60", false},
		{"1y at 60s", "2025-09-15 08:00:00 UTC", "2026-09-15 08:00:00 UTC", "60", false},
		// Duration-form steps are understood, not silently replaced.
		{"30d at 1h", "2026-08-16 08:00:00 UTC", "2026-09-15 08:00:00 UTC", "1h", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end := mustTime(t, tc.start), mustTime(t, tc.end)
			got := clampStep(start, end, tc.step)

			if tc.wantSame {
				if got != tc.step {
					t.Fatalf("clampStep = %q; want unchanged %q", got, tc.step)
				}
				return
			}
			if got == tc.step {
				t.Fatalf("clampStep left %q unchanged; expected widening", tc.step)
			}
			secs, err := strconv.ParseFloat(got, 64)
			if err != nil {
				t.Fatalf("widened step %q is not numeric: %v", got, err)
			}
			points := end.Sub(start).Seconds() / secs
			if points > maxRangePoints {
				t.Errorf("widened to %q, still %.0f points (max %d)", got, points, maxRangePoints)
			}
			// Widening should be minimal — not an order of magnitude past the cap.
			if points < maxRangePoints/2 {
				t.Errorf("widened to %q = %.0f points; over-coarsened (cap %d)", got, points, maxRangePoints)
			}
		})
	}
}

// TestClampStep_HandlesDegenerateInput checks the paths that used to be able
// to reach Prometheus as a malformed or unbounded request.
func TestClampStep_HandlesDegenerateInput(t *testing.T) {
	start := mustTime(t, "2026-09-14 08:00:00 UTC")
	end := mustTime(t, "2026-09-15 08:00:00 UTC")

	// Zero and negative spans are left alone — runOne rejects them earlier.
	if got := clampStep(end, start, "60"); got != "60" {
		t.Errorf("reversed window: clampStep = %q; want 60", got)
	}
	if got := clampStep(start, start, "60"); got != "60" {
		t.Errorf("zero window: clampStep = %q; want 60", got)
	}

	// A zero / unparseable step must not divide by zero, and must produce
	// something that keeps the request under the cap.
	for _, step := range []string{"0", "-5", "", "abc"} {
		got := clampStep(start, end, step)
		if secs, err := strconv.ParseFloat(got, 64); err == nil && secs > 0 {
			if end.Sub(start).Seconds()/secs > maxRangePoints {
				t.Errorf("step %q -> %q still exceeds the cap", step, got)
			}
		}
	}
}

// countingProm records how many QueryRange calls are in flight at once.
type countingProm struct {
	body    []byte
	inFlite atomic.Int32
	maxSeen atomic.Int32
	release chan struct{}
}

func (c *countingProm) track() {
	n := c.inFlite.Add(1)
	for {
		m := c.maxSeen.Load()
		if n <= m || c.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	// Hold the slot until every goroutine that can start has started, so the
	// observed maximum reflects the real concurrency ceiling.
	<-c.release
	c.inFlite.Add(-1)
}

func (c *countingProm) Query(context.Context, string, string, string) ([]byte, error) {
	c.track()
	return c.body, nil
}

func (c *countingProm) QueryRange(context.Context, string, string, string, string, string) ([]byte, error) {
	c.track()
	return c.body, nil
}

func (c *countingProm) LabelValues(context.Context, string, string, string, []string) ([]byte, error) {
	return []byte(`{"status":"success","data":[]}`), nil
}

// TestHandleQueriesEnricher_BoundsFanOut verifies the per-request concurrency
// ceiling. Before this bound, peak memory scaled with however many queries a
// caller happened to batch, because every in-flight response and its decoded
// tree were live simultaneously.
func TestHandleQueriesEnricher_BoundsFanOut(t *testing.T) {
	const nQueries = 50

	fake := &countingProm{
		body:    []byte(matrixBody),
		release: make(chan struct{}),
	}
	p := &PrometheusEnricher{q: fake, accountID: "acct"}

	rawQueries := make([]any, 0, nQueries)
	for i := range nQueries {
		rawQueries = append(rawQueries, map[string]any{
			"key": fmt.Sprintf("Q%d", i), "query": fmt.Sprintf("q_%d", i),
		})
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := p.HandleQueriesEnricher(context.Background(), map[string]any{
			"promql_queries": rawQueries,
			"duration": map[string]any{
				"starts_at": "2026-09-14 08:00:00 UTC",
				"ends_at":   "2026-09-14 09:00:00 UTC",
			},
			"steps": "60",
		})
		if err != nil {
			t.Error(err)
		}
	}()

	// Give the fan-out time to saturate whatever ceiling exists, then let the
	// held calls drain.
	deadline := time.After(2 * time.Second)
	for fake.inFlite.Load() < int32(maxConcurrentQueries) {
		select {
		case <-deadline:
			t.Fatalf("only %d queries ever in flight; expected to reach %d",
				fake.inFlite.Load(), maxConcurrentQueries)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// Hold briefly so any unbounded fan-out would overshoot and be caught.
	time.Sleep(50 * time.Millisecond)
	close(fake.release)
	wg.Wait()

	if got := fake.maxSeen.Load(); got > int32(maxConcurrentQueries) {
		t.Errorf("peak in-flight queries = %d; want <= %d", got, maxConcurrentQueries)
	}
}

// TestParseMetricSeries_CoarsensRatherThanExploding covers the app_stats grid,
// which allocates (window/step) float64s per series and fills every cell. A
// caller asking for a year at a 60s step must get a coarser grid, not a
// multi-gigabyte allocation and not an error.
func TestParseMetricSeries_CoarsensRatherThanExploding(t *testing.T) {
	body := []byte(`{"status":"success","data":{"resultType":"matrix","result":[` +
		`{"metric":{"pod":"p"},"values":[[1700000000,"1"],[1700086400,"2"]]}]}}`)

	start := int64(1700000000)
	end := start + 365*24*3600 // one year
	const step = int64(60)     // would be 525600 points per series

	series, err := parseMetricSeries(body, start, end, step)
	if err != nil {
		t.Fatalf("parseMetricSeries: %v", err)
	}
	if len(series) != 1 {
		t.Fatalf("series count = %d; want 1", len(series))
	}
	if got := len(series[0].values.data); got > maxRangePoints+1 {
		t.Errorf("grid width = %d points; want <= %d", got, maxRangePoints+1)
	}
	// The real samples must still land somewhere in the grid.
	var seen int
	for _, v := range series[0].values.data {
		if !isNaN(v) {
			seen++
		}
	}
	if seen == 0 {
		t.Error("no samples landed in the coarsened grid")
	}
}

func isNaN(f float64) bool { return f != f }

// TestParsePromDuration covers the Prometheus duration grammar, including the
// d/w/y units Go's time.ParseDuration rejects and the compound forms a caller
// can legitimately send as a step.
func TestParsePromDuration(t *testing.T) {
	ok := map[string]float64{
		"30s":    30,
		"1m":     60,
		"90s":    90,
		"1h":     3600,
		"12h":    43200,
		"1d":     86400,
		"7d":     604800,
		"1w":     604800,
		"1y":     31536000,
		"1h30m":  5400,
		"2d12h":  216000,
		"1d1h1m": 90060,
		"500ms":  0.5,
		"1ms":    0.001,
	}
	for in, want := range ok {
		got, valid := parsePromDuration(in)
		if !valid {
			t.Errorf("parsePromDuration(%q) rejected; want %v", in, want)
			continue
		}
		if got != want {
			t.Errorf("parsePromDuration(%q) = %v; want %v", in, got, want)
		}
	}

	bad := []string{"", "d", "1", "1x", "abc", "-1d", "1.5h", "h1", "1d!"}
	for _, in := range bad {
		if got, valid := parsePromDuration(in); valid {
			t.Errorf("parsePromDuration(%q) = %v, accepted; want rejected", in, got)
		}
	}
}

// TestClampStep_RespectsPromDurationSteps is the regression guard for the bug
// this caught: a step in Prometheus duration units that clampStep could not
// read was treated as 60s, so an explicit coarse step ("1d") got *widened* to
// ~236s — 366x finer than asked for, and exactly the oversized query the clamp
// is meant to prevent.
func TestClampStep_RespectsPromDurationSteps(t *testing.T) {
	start := mustTime(t, "2026-08-16 08:00:00 UTC")
	end := mustTime(t, "2026-09-15 08:00:00 UTC") // 30 days

	// All of these are coarse enough to stay under the cap, so clampStep must
	// return them untouched.
	for _, step := range []string{"1d", "1w", "12h", "1h30m", "2d12h", "1y", "300s"} {
		if got := clampStep(start, end, step); got != step {
			t.Errorf("clampStep(30d window, %q) = %q; want unchanged", step, got)
		}
	}

	// A step genuinely too fine for the window still widens.
	for _, step := range []string{"60", "1m", "30s"} {
		got := clampStep(start, end, step)
		if got == step {
			t.Errorf("clampStep(30d window, %q) left it unchanged; expected widening", step)
		}
		secs, err := strconv.ParseFloat(got, 64)
		if err != nil {
			t.Fatalf("widened step %q not numeric: %v", got, err)
		}
		if points := end.Sub(start).Seconds() / secs; points > maxRangePoints {
			t.Errorf("step %q -> %q, still %.0f points", step, got, points)
		}
	}
}

// TestSampleScanner_MalformedInputNeverPanics walks the scanner over truncated
// and malformed input, with particular attention to a trailing backslash,
// which advances the cursor two bytes at a time.
func TestSampleScanner_MalformedInputNeverPanics(t *testing.T) {
	inputs := []string{
		`[[0,"\`,
		`[[0,"ab\`,
		`[["\`,
		`[[0,"\"]]`,
		`[`,
		`[[`,
		`[[0`,
		`[[0,`,
		`[[0,"`,
		`[[0,"1"`,
		`[[0,"1"]`,
		`"`,
		`\`,
		`[\`,
		`[[\\`,
		`null`,
		`nul`,
		``,
	}
	for _, in := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic scanning %q: %v", in, r)
				}
			}()
			var c sampleColumns
			_ = c.UnmarshalJSON([]byte(in))
			var s promSample
			_ = s.UnmarshalJSON([]byte(in))
		}()
	}
}
