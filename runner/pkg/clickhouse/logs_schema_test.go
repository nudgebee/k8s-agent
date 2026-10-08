package clickhouse

import (
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
		90 * time.Minute: "toIntervalHour(2)", // round up, never shorter than asked
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
