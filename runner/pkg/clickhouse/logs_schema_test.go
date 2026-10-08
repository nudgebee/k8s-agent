package clickhouse

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func columnDefined(ddl, col string) bool {
	return regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(col) + `\s`).MatchString(ddl)
}

// The exporter inserts these by name with create_schema: false; one missing
// column fails every batch, so this list must match logs_insert.sql (v0.157.0).
func TestLogsTableDDL_HasEveryExporterInsertColumn(t *testing.T) {
	ddl := LogsTableDDL("default", DefaultLogsRetention)
	for _, col := range exporterInsertColumns {
		if !columnDefined(ddl, col) {
			t.Errorf("DDL missing exporter column %s", col)
		}
	}
}

func TestLogsTableDDL_KubernetesColumnsMaterializedLowCardinality(t *testing.T) {
	ddl := LogsTableDDL("default", DefaultLogsRetention)
	for _, col := range []string{"namespace", "workload", "pod", "container"} {
		if !regexp.MustCompile(`(?m)^\s+` + col + ` LowCardinality\(String\) MATERIALIZED `).MatchString(ddl) {
			t.Errorf("%s must be LowCardinality(String) MATERIALIZED", col)
		}
	}
	for _, col := range nudgebeeLogColumns {
		if !columnDefined(ddl, col) {
			t.Errorf("DDL missing Nudgebee column %s", col)
		}
	}
}

func TestLogsTableDDL_SortKeyLeadsWithKubernetesLabels(t *testing.T) {
	ddl := LogsTableDDL("default", DefaultLogsRetention)
	want := "ORDER BY (namespace, workload, toStartOfFiveMinutes(Timestamp), pod, container, Timestamp)"
	if !strings.Contains(ddl, want) {
		t.Errorf("want %q in DDL:\n%s", want, ddl)
	}
}

func TestLogsTableDDL_TokenIndexOnBody(t *testing.T) {
	ddl := LogsTableDDL("default", DefaultLogsRetention)
	if !strings.Contains(ddl, "INDEX idx_body lower(Body) TYPE tokenbf_v1(32768, 3, 0)") {
		t.Errorf("missing tokenbf_v1 index on lower(Body):\n%s", ddl)
	}
}

func TestLogsTableDDL_TTLFromRetention(t *testing.T) {
	cases := map[time.Duration]string{
		72 * time.Hour:   "toIntervalHour(72)",
		90 * time.Minute: "toIntervalHour(2)",  // round up, never shorter than asked
		0:                "toIntervalHour(72)", // unset falls back to the default
		-time.Hour:       "toIntervalHour(72)",
	}
	for in, want := range cases {
		if ddl := LogsTableDDL("default", in); !strings.Contains(ddl, "TTL toDateTime(Timestamp) + "+want) {
			t.Errorf("retention %v: want %s", in, want)
		}
	}
}

// CLICKHOUSE_DB is operator-supplied and may contain a hyphen.
func TestLogsTableDDL_QuotesDatabase(t *testing.T) {
	ddl := LogsTableDDL("logs-db", DefaultLogsRetention)
	if !strings.HasPrefix(ddl, "CREATE TABLE IF NOT EXISTS `logs-db`.`otel_logs`") {
		t.Errorf("database not quoted:\n%s", ddl[:80])
	}
}

// The level regexes reach ClickHouse through a Go raw string, so \b must be
// written \b; a single backslash is a backspace inside a ClickHouse literal.
func TestLogsTableDDL_LevelRegexesEscapeWordBoundary(t *testing.T) {
	ddl := LogsTableDDL("default", DefaultLogsRetention)
	for _, want := range []string{
		`match(Body, '(?i)\\b(fatal|panic|critical)\\b')`,
		`match(Body, '(?i)\\b(error|err|exception)\\b')`,
		`match(Body, '(?i)\\bwarn(ing)?\\b')`,
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("want %s in DDL", want)
		}
	}
}

// logsStub answers CREATE with an empty 200 (as ClickHouse does) and
// system.columns reads from a fixed column list.
type logsStub struct {
	columns    []string
	failCreate bool
	creates    int
	colsTable  string
}

func (s *logsStub) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		q := string(b)
		switch {
		case strings.HasPrefix(q, "CREATE TABLE"):
			s.creates++
			if s.failCreate {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("Code: 999. DB::Exception: not ready"))
			}
		case strings.Contains(q, "system.columns"):
			if m := regexp.MustCompile(`table = '([^']+)'`).FindStringSubmatch(q); m != nil {
				s.colsTable = m[1]
			}
			rows := make([][]any, 0, len(s.columns))
			for _, c := range s.columns {
				rows = append(rows, []any{c})
			}
			writeRows(w, "name", rows)
		default:
			t.Errorf("unexpected query: %s", q)
		}
	}))
	t.Cleanup(srv.Close)
	return New(Config{Host: strings.TrimPrefix(srv.URL, "http://"), Database: "default"})
}

func fullLogsColumns() []string {
	return append(append([]string{"EventName"}, exporterInsertColumns...), nudgebeeLogColumns...)
}

func TestEnsureLogsTable_CreatesAndReportsReady(t *testing.T) {
	s := &logsStub{columns: fullLogsColumns()}
	state, err := EnsureLogsTable(context.Background(), s.client(t), DefaultLogsRetention)
	if state != LogsTableReady || err != nil {
		t.Fatalf("want ready, got %v (%v)", state, err)
	}
	if s.creates != 1 {
		t.Errorf("want 1 CREATE, got %d", s.creates)
	}
	if s.colsTable != LogsTable {
		t.Errorf("columns read from %q, want %q", s.colsTable, LogsTable)
	}
}

func TestEnsureLogsTable_CreateFailureIsUnavailable(t *testing.T) {
	s := &logsStub{failCreate: true}
	state, err := EnsureLogsTable(context.Background(), s.client(t), DefaultLogsRetention)
	if state != LogsTableUnavailable || err == nil {
		t.Fatalf("want unavailable with error, got %v (%v)", state, err)
	}
}

// Review Focus 1: a table created by the exporter's own schema has the OTel
// columns but none of ours. CREATE IF NOT EXISTS is a no-op there, so the
// runner must say so instead of claiming ready.
func TestEnsureLogsTable_LegacyShape(t *testing.T) {
	s := &logsStub{columns: exporterInsertColumns}
	state, err := EnsureLogsTable(context.Background(), s.client(t), DefaultLogsRetention)
	if state != LogsTableLegacyShape {
		t.Fatalf("want legacy shape, got %v", state)
	}
	if err == nil || !strings.Contains(err.Error(), "namespace") || !strings.Contains(err.Error(), "RENAME TABLE") {
		t.Errorf("error must name missing columns and the remedy, got %v", err)
	}
}

func TestEnsureLogsTable_NilClient(t *testing.T) {
	if state, err := EnsureLogsTable(context.Background(), nil, DefaultLogsRetention); state != LogsTableUnavailable || err == nil {
		t.Fatalf("want unavailable, got %v (%v)", state, err)
	}
}
