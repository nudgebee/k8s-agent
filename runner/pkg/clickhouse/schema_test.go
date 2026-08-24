package clickhouse

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// chStub is a ClickHouse stand-in that answers system.columns reads from a
// column list and records the DDL it is handed.
type chStub struct {
	columns      []string
	traceSrcExpr string
	alters       []string
	failAlter    bool
	// columnsAfterAlter, when non-nil, is served by system.columns reads that
	// follow an ALTER — the re-read EnsureMaterializedColumns does to confirm
	// the statement actually landed.
	columnsAfterAlter []string
	altered           bool
}

func (s *chStub) server(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		q := string(body)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(q, "ALTER TABLE"):
			s.alters = append(s.alters, q)
			s.altered = true
			if s.failAlter {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte("Code: 47. DB::Exception: Unknown identifier"))
				return
			}
			_, _ = w.Write([]byte(`{"meta":[],"data":[]}`))

		case strings.Contains(q, "default_expression"):
			rows := [][]any{}
			if s.traceSrcExpr != "" {
				rows = append(rows, []any{s.traceSrcExpr})
			}
			writeRows(w, "default_expression", rows)

		case strings.Contains(q, "system.columns"):
			cols := s.columns
			if s.altered && s.columnsAfterAlter != nil {
				cols = s.columnsAfterAlter
			}
			rows := make([][]any, 0, len(cols))
			for _, c := range cols {
				rows = append(rows, []any{c})
			}
			writeRows(w, "name", rows)

		default:
			t.Errorf("unexpected query: %s", q)
			writeRows(w, "x", nil)
		}
	}))
	t.Cleanup(srv.Close)
	return New(Config{Host: strings.TrimPrefix(srv.URL, "http://"), Database: "default"})
}

func writeRows(w http.ResponseWriter, col string, rows [][]any) {
	if rows == nil {
		rows = [][]any{}
	}
	payload := map[string]any{
		"meta": []map[string]string{{"name": col, "type": "String"}},
		"data": rows,
	}
	b, _ := json.Marshal(payload)
	_, _ = w.Write(b)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// allMaterialized is every column this package manages.
func allMaterialized() []string {
	out := make([]string, 0, len(materializedColumns))
	for name := range materializedColumns {
		out = append(out, name)
	}
	return out
}

// A pre-0.156 table has no ScopeName column, so the trace_source definition
// must not reference it — doing so fails the whole ALTER at name resolution,
// which is the exact bug this schema work exists to stop.
func TestTraceSourceDefinition_OmitsScopeNameWhenColumnAbsent(t *testing.T) {
	def := traceSourceDefinition(map[string]struct{}{"SpanAttributes": {}})
	if strings.Contains(def, scopeNameColumn) {
		t.Errorf("definition references %s on a table without it:\n%s", scopeNameColumn, def)
	}
	if !strings.Contains(def, "otel.scope.name") {
		t.Errorf("definition dropped the attribute fallback:\n%s", def)
	}
}

// On a 0.157 table the attribute is empty on every span, so the column arm has
// to be present or every trace is classified 'otel'.
func TestTraceSourceDefinition_UsesScopeNameWhenColumnPresent(t *testing.T) {
	def := traceSourceDefinition(map[string]struct{}{scopeNameColumn: {}})
	if !strings.Contains(def, scopeNameColumn+" = '"+ebpfScopeName+"'") {
		t.Errorf("definition missing the %s arm:\n%s", scopeNameColumn, def)
	}
	// Spans written before the upgrade are still in the table.
	if !strings.Contains(def, "otel.scope.name") {
		t.Errorf("definition dropped the attribute arm:\n%s", def)
	}
}

// A bare OTel exporter table gets every managed column added.
func TestEnsure_AddsAllColumnsToFreshTable(t *testing.T) {
	s := &chStub{
		columns:           []string{"Timestamp", "TraceId", "SpanAttributes", "ResourceAttributes", scopeNameColumn},
		columnsAfterAlter: append([]string{"Timestamp", "TraceId", "SpanAttributes", "ResourceAttributes", scopeNameColumn}, allMaterialized()...),
	}
	if got := EnsureMaterializedColumns(context.Background(), s.server(t), discardLogger()); !got {
		t.Fatal("EnsureMaterializedColumns = false; want true")
	}
	if len(s.alters) != 1 {
		t.Fatalf("issued %d ALTERs; want 1", len(s.alters))
	}
	for name := range materializedColumns {
		if !strings.Contains(s.alters[0], "ADD COLUMN "+name+" ") {
			t.Errorf("ALTER missing column %q:\n%s", name, s.alters[0])
		}
	}
}

// Rackspace's shape: the legacy agent already created all 14. Nothing to do,
// and the flag must come back true — reporting false is what forced installs
// with working columns onto the recompute query shape.
func TestEnsure_NoDDLWhenAlreadyComplete(t *testing.T) {
	cols := append([]string{"Timestamp", "SpanAttributes"}, allMaterialized()...)
	s := &chStub{columns: cols}
	if got := EnsureMaterializedColumns(context.Background(), s.server(t), discardLogger()); !got {
		t.Fatal("EnsureMaterializedColumns = false; want true")
	}
	if len(s.alters) != 0 {
		t.Errorf("issued %d ALTERs on a complete table; want 0:\n%v", len(s.alters), s.alters)
	}
}

// An install carried across the collector upgrade keeps a trace_source built
// only from the span attribute, which the new collector no longer populates.
// Name-presence alone can't see that, so the stored expression is compared.
func TestEnsure_RewritesStaleTraceSource(t *testing.T) {
	cols := append([]string{"Timestamp", "SpanAttributes", scopeNameColumn}, allMaterialized()...)
	s := &chStub{
		columns:      cols,
		traceSrcExpr: "CASE WHEN toString(SpanAttributes['otel.scope.name']) = 'nudgebee-node-agent' THEN 'ebpf' ELSE 'otel' END",
	}
	if got := EnsureMaterializedColumns(context.Background(), s.server(t), discardLogger()); !got {
		t.Fatal("EnsureMaterializedColumns = false; want true")
	}
	if len(s.alters) != 1 || !strings.Contains(s.alters[0], "MODIFY COLUMN trace_source") {
		t.Fatalf("expected a MODIFY COLUMN trace_source; got %v", s.alters)
	}
	if !strings.Contains(s.alters[0], scopeNameColumn+" = '"+ebpfScopeName+"'") {
		t.Errorf("rewritten definition missing the %s arm:\n%s", scopeNameColumn, s.alters[0])
	}
}

// The same stale-looking definition on a table that has no ScopeName is
// correct, not stale — rewriting it would break the install.
func TestEnsure_LeavesTraceSourceAloneWithoutScopeName(t *testing.T) {
	cols := append([]string{"Timestamp", "SpanAttributes"}, allMaterialized()...)
	s := &chStub{
		columns:      cols,
		traceSrcExpr: "CASE WHEN toString(SpanAttributes['otel.scope.name']) = 'nudgebee-node-agent' THEN 'ebpf' ELSE 'otel' END",
	}
	if got := EnsureMaterializedColumns(context.Background(), s.server(t), discardLogger()); !got {
		t.Fatal("EnsureMaterializedColumns = false; want true")
	}
	if len(s.alters) != 0 {
		t.Errorf("rewrote a correct definition: %v", s.alters)
	}
}

// The exporter creates otel_traces lazily on its first span batch, so an agent
// that starts first sees no table. That is not an error, and it must not be
// reported as "columns present".
func TestEnsure_AbsentTableReportsFalseWithoutDDL(t *testing.T) {
	s := &chStub{columns: nil}
	if got := EnsureMaterializedColumns(context.Background(), s.server(t), discardLogger()); got {
		t.Error("EnsureMaterializedColumns = true for a missing table; want false")
	}
	if len(s.alters) != 0 {
		t.Errorf("issued DDL against a missing table: %v", s.alters)
	}
}

// A rejected ALTER must report false: claiming the columns exist makes the
// backend SELECT columns that aren't there, turning a slow query into a failed
// one.
func TestEnsure_FailedAlterReportsFalse(t *testing.T) {
	s := &chStub{
		columns:   []string{"Timestamp", "SpanAttributes"},
		failAlter: true,
	}
	if got := EnsureMaterializedColumns(context.Background(), s.server(t), discardLogger()); got {
		t.Error("EnsureMaterializedColumns = true after a failed ALTER; want false")
	}
}

// A partially applied ALTER must not report success either — hence the re-read
// rather than trusting the statement's exit status.
func TestEnsure_PartialAlterReportsFalse(t *testing.T) {
	s := &chStub{
		columns:           []string{"Timestamp", "SpanAttributes"},
		columnsAfterAlter: []string{"Timestamp", "SpanAttributes", "workload_name"},
	}
	if got := EnsureMaterializedColumns(context.Background(), s.server(t), discardLogger()); got {
		t.Error("EnsureMaterializedColumns = true after a partial ALTER; want false")
	}
}

func TestEnsure_NilClientReportsFalse(t *testing.T) {
	if EnsureMaterializedColumns(context.Background(), nil, discardLogger()) {
		t.Error("EnsureMaterializedColumns = true for a nil client; want false")
	}
}

// hasAll is a set check, not a count: the legacy probe asserted `count() = 14`,
// so adding a 15th column would have flipped every existing install to the slow
// path until it had been altered.
func TestHasAll_IgnoresExtraColumns(t *testing.T) {
	required := map[string]string{"a": "", "b": ""}
	cols := map[string]struct{}{"a": {}, "b": {}, "unrelated": {}}
	if !hasAll(cols, required) {
		t.Error("hasAll = false when every required column is present")
	}
	if hasAll(map[string]struct{}{"a": {}}, required) {
		t.Error("hasAll = true with a required column missing")
	}
}

// The two schemas actually in the fleet, captured from live clusters. They are
// mutually exclusive — one has ScopeName and no materialized columns, the other
// has the materialized columns and no ScopeName — which is why the definition
// has to be chosen from the real column set rather than written as one static
// expression with an OR fallback.
var (
	// nudgebee-agent-dev, otel-collector 0.157: the flattened exporter schema.
	// SpanAttributes['otel.scope.name'] is empty on every span here.
	devSchema = []string{
		"Timestamp", "TraceId", "SpanId", "ParentSpanId", "TraceState", "SpanName", "SpanKind",
		"ServiceName", "ResourceAttributes", "ScopeName", "ScopeVersion", "SpanAttributes",
		"Duration", "StatusCode", "StatusMessage",
		"Events.Timestamp", "Events.Name", "Events.Attributes",
		"Links.TraceId", "Links.SpanId", "Links.TraceState", "Links.Attributes",
	}
	// nudgebee-rackspace, pre-0.156 exporter plus the 14 columns the legacy
	// Python runner added before the Go rewrite. No ScopeName column at all.
	rackspaceSchema = []string{
		"Timestamp", "TraceId", "SpanId", "ParentSpanId", "TraceState", "SpanName", "SpanKind",
		"ServiceName", "ResourceAttributes", "SpanAttributes", "Duration", "StatusCode", "StatusMessage",
		"Events.Timestamp", "Events.Name", "Events.Attributes",
		"Links.TraceId", "Links.SpanId", "Links.TraceState", "Links.Attributes",
		"workload_namespace", "workload_name", "resource", "destination_workload_name",
		"destination_workload_namespace", "destination_name", "headers", "http_status_code",
		"request_payload", "http_response", "trace_source", "workload_zone",
		"destination_workload_zone", "cloud_availability_zone",
	}
)

// A fresh 0.157 install has none of the materialized columns; it must get all
// of them, with trace_source reading the ScopeName column.
func TestEnsure_RealDevSchema(t *testing.T) {
	s := &chStub{columns: devSchema, columnsAfterAlter: append(append([]string{}, devSchema...), allMaterialized()...)}
	if got := EnsureMaterializedColumns(context.Background(), s.server(t), discardLogger()); !got {
		t.Fatal("EnsureMaterializedColumns = false; want true")
	}
	if len(s.alters) != 1 {
		t.Fatalf("issued %d ALTERs; want 1", len(s.alters))
	}
	if !strings.Contains(s.alters[0], "ADD COLUMN trace_source") ||
		!strings.Contains(s.alters[0], scopeNameColumn+" = '"+ebpfScopeName+"'") {
		t.Errorf("trace_source must read the ScopeName column on this schema:\n%s", s.alters[0])
	}
}

// Rackspace already has all 14 and no ScopeName. The correct outcome is no DDL
// at all and a true flag — the mis-reported false is what pushed it onto the
// recompute query shape, which then referenced a column its table lacks.
func TestEnsure_RealRackspaceSchema(t *testing.T) {
	s := &chStub{
		columns:      rackspaceSchema,
		traceSrcExpr: "CASE WHEN toString(SpanAttributes['otel.scope.name']) = 'nudgebee-node-agent' THEN 'ebpf' ELSE 'otel' END",
	}
	if got := EnsureMaterializedColumns(context.Background(), s.server(t), discardLogger()); !got {
		t.Fatal("EnsureMaterializedColumns = false; want true — the columns are all present")
	}
	if len(s.alters) != 0 {
		t.Errorf("issued DDL against a table that needs none: %v", s.alters)
	}
}
