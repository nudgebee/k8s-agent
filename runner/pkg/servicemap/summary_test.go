package servicemap

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Tables come before the MVs that write into them, every statement is safe
// to rerun, and the MVs keep only delta rows: a cumulative row would add up
// running totals into the summary for good.
func TestSummaryDDL(t *testing.T) {
	ddl := summaryDDL("default")
	if len(ddl) != 8 {
		t.Fatalf("statements = %d; want 4 tables then 4 views", len(ddl))
	}
	for i, stmt := range ddl {
		isView := strings.HasPrefix(stmt, "CREATE MATERIALIZED VIEW IF NOT EXISTS")
		if isView != (i >= 4) || (!isView && !strings.HasPrefix(stmt, "CREATE TABLE IF NOT EXISTS")) {
			t.Errorf("statement %d is out of order or not idempotent:\n%s", i, stmt)
		}
		if !strings.Contains(stmt, "_v1") {
			t.Errorf("statement %d is not versioned:\n%s", i, stmt)
		}
		if !isView && !strings.Contains(stmt, "+ "+summaryTTL) {
			t.Errorf("table %d has no TTL:\n%s", i, stmt)
		}
	}
	for name, tc := range map[string]struct{ stmt, filter string }{
		"edges":   {ddl[4], deltaCounters},
		"latency": {ddl[5], deltaHistograms},
		"agents":  {ddl[6], deltaCounters},
		"pods":    {ddl[7], deltaCounters},
	} {
		if !strings.Contains(tc.stmt, tc.filter) {
			t.Errorf("%s view does not filter on %q:\n%s", name, tc.filter, tc.stmt)
		}
	}
	// One row per series, so the read can take the slowest series per edge.
	if !strings.Contains(ddl[5], "cityHash64(Attributes, ResourceAttributes['service.instance.id']) AS series") {
		t.Errorf("latency view does not key on the raw series:\n%s", ddl[5])
	}
	for _, m := range counterMetrics() {
		if !strings.Contains(ddl[4], sqlString(m)) {
			t.Errorf("edges view does not keep %s", m)
		}
	}
}

func TestStartSummary(t *testing.T) {
	t.Run("prometheus creates nothing", func(t *testing.T) {
		ch := &fakeClickHouse{}
		s, _ := newTestService(t, ch, SourcePrometheus)
		s.summaryReady.Store(false)
		s.StartSummary(context.Background())
		time.Sleep(50 * time.Millisecond)
		ch.mu.Lock()
		n := len(ch.sqls)
		ch.mu.Unlock()
		if n != 0 || s.summaryReady.Load() {
			t.Errorf("prometheus source sent %d queries to ClickHouse", n)
		}
	})

	t.Run("waits for the raw tables, then creates", func(t *testing.T) {
		defer func(r time.Duration) { summaryRetry = r }(summaryRetry)
		summaryRetry = 10 * time.Millisecond
		ch := &fakeClickHouse{noRawTables: true}
		s, _ := newTestService(t, ch, SourceShadow)
		s.summaryReady.Store(false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s.StartSummary(ctx)

		time.Sleep(50 * time.Millisecond)
		if ch.ddlCalls.Load() != 0 || s.summaryReady.Load() {
			t.Fatal("created the summary before the raw tables exist")
		}
		ch.mu.Lock()
		ch.noRawTables = false
		ch.mu.Unlock()
		deadline := time.Now().Add(2 * time.Second)
		for !s.summaryReady.Load() && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if !s.summaryReady.Load() {
			t.Fatal("summary not ready after the raw tables appeared")
		}
		if got := ch.ddlCalls.Load(); got != 8 {
			t.Errorf("DDL statements = %d; want 8", got)
		}
	})

	// ClickHouse restarted on an empty volume: the setup must notice and
	// create everything again, not trust the first run for good.
	t.Run("keeps re-running after it succeeds", func(t *testing.T) {
		defer func(r, c time.Duration) { summaryRetry, summaryRecheck = r, c }(summaryRetry, summaryRecheck)
		summaryRetry, summaryRecheck = 10*time.Millisecond, 10*time.Millisecond
		ch := &fakeClickHouse{}
		s, _ := newTestService(t, ch, SourceShadow)
		s.summaryReady.Store(false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s.StartSummary(ctx)

		waitFor := func(what string, ok func() bool) {
			t.Helper()
			deadline := time.Now().Add(2 * time.Second)
			for !ok() && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if !ok() {
				t.Fatal(what)
			}
		}
		waitFor("DDL not re-run after the first success", func() bool { return ch.ddlCalls.Load() >= 16 })
		ch.mu.Lock()
		ch.noRawTables = true
		ch.mu.Unlock()
		waitFor("still ready after the raw tables disappeared", func() bool { return !s.summaryReady.Load() })
		ch.mu.Lock()
		ch.noRawTables = false
		ch.mu.Unlock()
		waitFor("not ready again after the raw tables came back", s.summaryReady.Load)
	})
}
