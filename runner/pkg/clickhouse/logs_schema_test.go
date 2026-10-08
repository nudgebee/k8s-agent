package clickhouse

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// logsStub plays ClickHouse for one otel_logs table. Tests preset its
// columns (nil: the table does not exist), TTL hours and row count. CREATE
// creates the table when absent, RENAME moves it away, ALTER ... MODIFY TTL
// changes its TTL.
// mu guards every field: the HTTP handler runs on server goroutines while
// tests flip and read them.
type logsStub struct {
	mu         sync.Mutex
	columns    []string
	ttlHours   string
	rows       int
	failCreate bool
	failCount  bool
	failRename bool
	creates    int
	renames    int
	alters     []string
	colsTable  string
}

var ttlHoursRe = regexp.MustCompile(`toIntervalHour\((\d+)\)`)

// engineFull renders system.tables.engine_full the way ClickHouse 24.12 does
// for our DDL (see the integration test).
func engineFull(ttlHours string) string {
	return "MergeTree PARTITION BY toDate(Timestamp) ORDER BY (namespace, workload, toStartOfFiveMinutes(Timestamp), pod, container, Timestamp) TTL toDateTime(Timestamp) + toIntervalHour(" + ttlHours + ") SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1"
}

func (s *logsStub) setFailCreate(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failCreate = v
}

func (s *logsStub) createCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates
}

func (s *logsStub) renameCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renames
}

func (s *logsStub) alterStatements() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.alters...)
}

func (s *logsStub) columnsTable() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.colsTable
}

func (s *logsStub) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		q := string(b)
		s.mu.Lock()
		defer s.mu.Unlock()
		fail := func(status int, msg string) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(msg))
		}
		switch {
		case strings.HasPrefix(q, "CREATE TABLE"):
			s.creates++
			if s.failCreate {
				fail(http.StatusServiceUnavailable, "Code: 999. DB::Exception: not ready")
				return
			}
			if s.columns == nil {
				s.columns = fullLogsColumns()
				s.ttlHours = ttlHoursRe.FindStringSubmatch(q)[1]
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
		case strings.Contains(q, "engine_full") && strings.Contains(q, "system.tables"):
			var rows [][]any
			if s.columns != nil {
				rows = [][]any{{engineFull(s.ttlHours)}}
			}
			writeRows(w, "engine_full", rows)
		case strings.HasPrefix(q, "SELECT count() FROM"):
			if s.failCount {
				fail(http.StatusInternalServerError, "Code: 999. DB::Exception: count failed")
				return
			}
			// JSONCompact quotes UInt64 values.
			writeRows(w, "count()", [][]any{{strconv.Itoa(s.rows)}})
		case strings.HasPrefix(q, "RENAME TABLE"):
			s.renames++
			if s.failRename {
				fail(http.StatusInternalServerError, "Code: 57. DB::Exception: Table default.otel_logs_legacy already exists. (TABLE_ALREADY_EXISTS)")
				return
			}
			s.columns = nil
		case strings.HasPrefix(q, "ALTER TABLE") && strings.Contains(q, "MODIFY TTL"):
			s.alters = append(s.alters, q)
			s.ttlHours = ttlHoursRe.FindStringSubmatch(q)[1]
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

// bufLogger records log output as JSON, the runner's format (where a
// time.Duration attribute prints as integer nanoseconds). EnsureLogsTable and
// KeepEnsuringLogsTable log on the caller's goroutine, so the buffer needs no
// lock.
func bufLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

func TestEnsureLogsTable_CreatesAndReportsReady(t *testing.T) {
	s := &logsStub{} // no table yet
	state, err := EnsureLogsTable(context.Background(), s.client(t), DefaultLogsRetention, discardLogger())
	if state != LogsTableReady || err != nil {
		t.Fatalf("want ready, got %v (%v)", state, err)
	}
	if n := s.createCount(); n != 1 {
		t.Errorf("want 1 CREATE, got %d", n)
	}
	if got := s.columnsTable(); got != LogsTable {
		t.Errorf("columns read from %q, want %q", got, LogsTable)
	}
	if alters := s.alterStatements(); len(alters) != 0 {
		t.Errorf("fresh table got a TTL ALTER: %v", alters)
	}
}

func TestEnsureLogsTable_CreateFailureIsUnavailable(t *testing.T) {
	s := &logsStub{failCreate: true}
	state, err := EnsureLogsTable(context.Background(), s.client(t), DefaultLogsRetention, discardLogger())
	if state != LogsTableUnavailable || err == nil {
		t.Fatalf("want unavailable with error, got %v (%v)", state, err)
	}
}

// CREATE IF NOT EXISTS never changes an existing table, so a new
// logs.retention only reaches it through ALTER ... MODIFY TTL.
func TestEnsureLogsTable_MatchingTTLNoAlter(t *testing.T) {
	s := &logsStub{columns: fullLogsColumns(), ttlHours: "72"}
	if state, err := EnsureLogsTable(context.Background(), s.client(t), 72*time.Hour, discardLogger()); state != LogsTableReady {
		t.Fatalf("want ready, got %v (%v)", state, err)
	}
	if alters := s.alterStatements(); len(alters) != 0 {
		t.Errorf("TTL already matches, want no ALTER, got %v", alters)
	}
}

func TestEnsureLogsTable_DifferentTTLAltersOnce(t *testing.T) {
	s := &logsStub{columns: fullLogsColumns(), ttlHours: "72"}
	c := s.client(t)
	logger, logs := bufLogger()
	if state, err := EnsureLogsTable(context.Background(), c, 24*time.Hour, logger); state != LogsTableReady {
		t.Fatalf("want ready, got %v (%v)", state, err)
	}
	want := "ALTER TABLE `default`.`otel_logs` MODIFY TTL toDateTime(Timestamp) + toIntervalHour(24)"
	if alters := s.alterStatements(); len(alters) != 1 || alters[0] != want {
		t.Fatalf("want exactly [%s], got %q", want, alters)
	}
	for _, w := range []string{`"level":"INFO"`, `"old_retention":"72h0m0s"`, `"new_retention":"24h0m0s"`} {
		if !strings.Contains(logs.String(), w) {
			t.Errorf("log missing %q:\n%s", w, logs.String())
		}
	}

	// The TTL now matches: a second pass changes nothing.
	if state, err := EnsureLogsTable(context.Background(), c, 24*time.Hour, discardLogger()); state != LogsTableReady {
		t.Fatalf("second pass: want ready, got %v (%v)", state, err)
	}
	if n := len(s.alterStatements()); n != 1 {
		t.Errorf("second pass ran another ALTER (%d total)", n)
	}
}

// Review Focus 1: a table created by the exporter's own schema has the OTel
// columns but none of ours. CREATE IF NOT EXISTS is a no-op there, so the
// runner must say so instead of claiming ready.
func TestEnsureLogsTable_LegacyShape(t *testing.T) {
	s := &logsStub{columns: exporterInsertColumns}
	state, err := EnsureLogsTable(context.Background(), s.client(t), DefaultLogsRetention, discardLogger())
	if state != LogsTableLegacyShape {
		t.Fatalf("want legacy shape, got %v", state)
	}
	if err == nil || !strings.Contains(err.Error(), "namespace") || !strings.Contains(err.Error(), "RENAME TABLE") {
		t.Errorf("error must name missing columns and the remedy, got %v", err)
	}
}

func TestEnsureLogsTable_NilClient(t *testing.T) {
	if state, err := EnsureLogsTable(context.Background(), nil, DefaultLogsRetention, discardLogger()); state != LogsTableUnavailable || err == nil {
		t.Fatalf("want unavailable, got %v (%v)", state, err)
	}
}

func TestKeepEnsuringLogsTable_RetriesUntilReady(t *testing.T) {
	s := &logsStub{failCreate: true}
	c := s.client(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.setFailCreate(false) // ClickHouse comes up
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	logger, logs := bufLogger()
	state := KeepEnsuringLogsTable(ctx, c, DefaultLogsRetention, 10*time.Millisecond, logger)
	if state != LogsTableReady {
		t.Fatalf("want ready after retries, got %v", state)
	}
	if n := s.createCount(); n < 2 {
		t.Errorf("want retries, got %d CREATE attempts", n)
	}
	// Human-readable, not integer nanoseconds.
	if !strings.Contains(logs.String(), `"retention":"72h0m0s"`) {
		t.Errorf("ready log must carry retention as 72h0m0s:\n%s", logs.String())
	}
}

func TestKeepEnsuringLogsTable_StopsOnContextCancel(t *testing.T) {
	s := &logsStub{failCreate: true}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if state := KeepEnsuringLogsTable(ctx, s.client(t), DefaultLogsRetention, 10*time.Millisecond, discardLogger()); state != LogsTableUnavailable {
		t.Fatalf("want unavailable on cancel, got %v", state)
	}
}
