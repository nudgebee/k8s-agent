package enrichers

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// failingRangeProm rejects any query containing "bad", the way Prometheus or
// VictoriaMetrics rejects a query it cannot parse.
type failingRangeProm struct{ cannedRangeProm }

func (f failingRangeProm) QueryRange(ctx context.Context, query, start, end, step, timeout string) ([]byte, error) {
	if strings.Contains(query, "bad") {
		return nil, errors.New("cannot parse string literal: invalid syntax")
	}
	return f.cannedRangeProm.QueryRange(ctx, query, start, end, step, timeout)
}

// A failed query degrades the reply instead of failing it, but it must be
// logged: dropping it silently hid an unparseable SLO matcher (#39800).
func TestRunQueries_LogsFailedQueries(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := &AppStatsEnricher{q: failingRangeProm{cannedRangeProm{body: []byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`)}}}
	end := time.Unix(1704067200, 0)
	out := a.runQueries(context.Background(), map[string]string{"good": "up", "broken": "bad_query"}, end.Add(-time.Hour), end, 60, "", "", "")

	if _, ok := out["good"]; !ok {
		t.Errorf("the good query's result is missing: %v", out)
	}
	if _, ok := out["broken"]; ok {
		t.Errorf("the failed query should not appear in the result")
	}
	logged := buf.String()
	if !strings.Contains(logged, "enrichers: prometheus query failed") || !strings.Contains(logged, "key=broken") {
		t.Errorf("failed query was not logged; log:\n%s", logged)
	}
}
