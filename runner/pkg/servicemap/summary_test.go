package servicemap

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Tables come before the MVs that write into them, every statement is safe
// to rerun, and the MVs keep only delta rows: a cumulative row would add up
// running totals into the summary for good.
func TestSummaryDDL(t *testing.T) {
	versioned := regexp.MustCompile(`_v[0-9]+\b`)
	ddl := summaryDDL("default")
	if len(ddl) != 8 {
		t.Fatalf("statements = %d; want 4 tables then 4 views", len(ddl))
	}
	for i, stmt := range ddl {
		isView := strings.HasPrefix(stmt, "CREATE MATERIALIZED VIEW IF NOT EXISTS")
		if isView != (i >= 4) || (!isView && !strings.HasPrefix(stmt, "CREATE TABLE IF NOT EXISTS")) {
			t.Errorf("statement %d is out of order or not idempotent:\n%s", i, stmt)
		}
		if !versioned.MatchString(stmt) {
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
	// One agent per pod, so a pod that reuses a gone pod's IP is a new agent
	// rather than the old one with a gap.
	if !strings.Contains(ddl[6], "ResourceAttributes['k8s.pod.uid']") {
		t.Errorf("agents view does not key on the pod:\n%s", ddl[6])
	}
	for _, m := range counterMetrics() {
		if !strings.Contains(ddl[4], sqlString(m)) {
			t.Errorf("edges view does not keep %s", m)
		}
	}
}

// Retired versions are dropped MV first, and never one still in use.
func TestRetiredSummaryDDL(t *testing.T) {
	ddl := retiredSummaryDDL("default")
	if len(ddl) != 2*len(retiredSummaryTables) {
		t.Fatalf("statements = %d; want a view and a table per retired version", len(ddl))
	}
	live := append(summaryTables(), summaryViews()...)
	for i, stmt := range ddl {
		want := "DROP TABLE IF EXISTS "
		if i%2 == 0 {
			want = "DROP VIEW IF EXISTS "
		}
		if !strings.HasPrefix(stmt, want) {
			t.Errorf("statement %d = %q; want prefix %q", i, stmt, want)
		}
		for _, name := range live {
			if strings.HasSuffix(stmt, "`"+name+"`") {
				t.Errorf("statement %d drops %s, which is still in use", i, name)
			}
		}
	}
}

// ddlSent returns the CREATE and DROP statements sent so far, in order.
func ddlSent(ch *fakeClickHouse) []string {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	var out []string
	for _, s := range ch.sqls {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "CREATE ") || strings.HasPrefix(s, "DROP ") {
			out = append(out, s)
		}
	}
	return out
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
		// The CREATEs, then the retired drops, MV first, each exactly once.
		want := append(summaryDDL(s.CH.Database), retiredSummaryDDL(s.CH.Database)...)
		var got []string
		deadline = time.Now().Add(2 * time.Second)
		for len(got) < len(want) && time.Now().Before(deadline) {
			got = ddlSent(ch)
			time.Sleep(5 * time.Millisecond)
		}
		if len(got) != len(want) {
			t.Fatalf("DDL sent = %d statements; want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
		}
		for i := range want {
			if !strings.HasPrefix(got[i], want[i]) {
				t.Errorf("DDL %d = %q; want %q", i, got[i], want[i])
			}
		}
	})

	// A user that may CREATE but not DROP: cleanup fails, the map still
	// reads from ClickHouse.
	t.Run("a failing drop does not block readiness", func(t *testing.T) {
		defer func(c time.Duration) { summaryRecheck = c }(summaryRecheck)
		summaryRecheck = 10 * time.Millisecond
		ch := &fakeClickHouse{dropFails: true}
		s, _ := newTestService(t, ch, SourceShadow)
		s.summaryReady.Store(false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s.StartSummary(ctx)

		deadline := time.Now().Add(2 * time.Second)
		for ch.ddlCalls.Load() < 24 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if !s.summaryReady.Load() {
			t.Fatal("summary not ready after a DROP failed")
		}
		drops := 0
		for _, stmt := range ddlSent(ch) {
			if strings.HasPrefix(stmt, "DROP ") {
				drops++
			}
		}
		if drops != 1 {
			t.Errorf("DROP statements = %d; want 1: stop at the first failure and do not retry", drops)
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
		// Rechecks re-create but never drop again: an older runner in a
		// rolling update re-creates the retired version, and they would fight.
		drops := 0
		for _, stmt := range ddlSent(ch) {
			if strings.HasPrefix(stmt, "DROP ") {
				drops++
			}
		}
		if drops != len(retiredSummaryDDL(s.CH.Database)) {
			t.Errorf("DROP statements = %d; want %d, from the first setup only", drops, len(retiredSummaryDDL(s.CH.Database)))
		}
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
