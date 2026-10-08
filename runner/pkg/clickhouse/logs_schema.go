package clickhouse

// otel_logs: the table container logs land in (#40144).
//
// The gateway collector's clickhouse exporter runs with create_schema: false,
// so this package owns the table. The exporter's own schema sorts by
// (ServiceName, Timestamp), which is a full scan for Kubernetes logs that have
// no meaningful service.name, so ours materializes the Kubernetes labels and
// sorts by them. The row contract the collectors must meet is in
// runner/docs/logs-row-contract.md.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// LogsTable is the table the exporter writes logs to (logs_table_name).
const LogsTable = "otel_logs"

// DefaultLogsRetention applies when LOGS_RETENTION is unset or not positive.
const DefaultLogsRetention = 72 * time.Hour

// exporterInsertColumns are the columns the v0.157.0 exporter names in its
// INSERT (internal/sqltemplates/logs_insert.sql). All must exist.
var exporterInsertColumns = []string{
	"Timestamp", "TraceId", "SpanId", "TraceFlags", "SeverityText", "SeverityNumber",
	"ServiceName", "Body", "ResourceSchemaUrl", "ResourceAttributes", "ScopeSchemaUrl",
	"ScopeName", "ScopeVersion", "ScopeAttributes", "LogAttributes",
}

// nudgebeeLogColumns are computed at insert time from the OTel maps.
var nudgebeeLogColumns = []string{"namespace", "workload", "pod", "container", "node", "stream", "level"}

const logsTableTemplate = `CREATE TABLE IF NOT EXISTS %s.%s
(
    Timestamp DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TraceId String CODEC(ZSTD(1)),
    SpanId String CODEC(ZSTD(1)),
    TraceFlags UInt8,
    SeverityText LowCardinality(String) CODEC(ZSTD(1)),
    SeverityNumber UInt8,
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    Body String CODEC(ZSTD(1)),
    ResourceSchemaUrl LowCardinality(String) CODEC(ZSTD(1)),
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeSchemaUrl LowCardinality(String) CODEC(ZSTD(1)),
    ScopeName String CODEC(ZSTD(1)),
    ScopeVersion LowCardinality(String) CODEC(ZSTD(1)),
    ScopeAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    LogAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    EventName String CODEC(ZSTD(1)),
    namespace LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.namespace.name'],
    workload LowCardinality(String) MATERIALIZED multiIf(
        ResourceAttributes['k8s.deployment.name'] != '', ResourceAttributes['k8s.deployment.name'],
        ResourceAttributes['k8s.statefulset.name'] != '', ResourceAttributes['k8s.statefulset.name'],
        ResourceAttributes['k8s.daemonset.name'] != '', ResourceAttributes['k8s.daemonset.name'],
        ResourceAttributes['k8s.cronjob.name'] != '', ResourceAttributes['k8s.cronjob.name'],
        ResourceAttributes['k8s.job.name'] != '', ResourceAttributes['k8s.job.name'],
        ResourceAttributes['k8s.pod.name']),
    pod LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.pod.name'],
    container LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.container.name'],
    node LowCardinality(String) MATERIALIZED ResourceAttributes['k8s.node.name'],
    stream LowCardinality(String) MATERIALIZED LogAttributes['log.iostream'],
    level LowCardinality(String) MATERIALIZED multiIf(
        SeverityNumber >= 21, 'critical',
        SeverityNumber >= 17, 'error',
        SeverityNumber >= 13, 'warning',
        SeverityNumber >= 9, 'info',
        SeverityNumber >= 1, 'debug',
        match(Body, '(?i)\\b(fatal|panic|critical)\\b'), 'critical',
        match(Body, '(?i)\\b(error|err|exception)\\b'), 'error',
        match(Body, '(?i)\\bwarn(ing)?\\b'), 'warning',
        'unknown'),
    INDEX idx_trace_id TraceId TYPE bloom_filter(0.001) GRANULARITY 1,
    INDEX idx_body lower(Body) TYPE tokenbf_v1(32768, 3, 0) GRANULARITY 8,
    INDEX idx_level level TYPE set(16) GRANULARITY 4,
    INDEX idx_res_attr_key mapKeys(ResourceAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_log_attr_key mapKeys(LogAttributes) TYPE bloom_filter(0.01) GRANULARITY 1,
    INDEX idx_log_attr_value mapValues(LogAttributes) TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = MergeTree
PARTITION BY toDate(Timestamp)
ORDER BY (namespace, workload, toStartOfFiveMinutes(Timestamp), pod, container, Timestamp)
TTL toDateTime(Timestamp) + toIntervalHour(%d)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1`

// LogsTableDDL returns the CREATE TABLE IF NOT EXISTS statement for otel_logs.
func LogsTableDDL(database string, retention time.Duration) string {
	return fmt.Sprintf(logsTableTemplate, quoteIdent(database), quoteIdent(LogsTable), retentionHours(retention))
}

// retentionHours rounds up so the table never keeps less than asked.
func retentionHours(d time.Duration) int64 {
	if d <= 0 {
		d = DefaultLogsRetention
	}
	h := int64(d / time.Hour)
	if d%time.Hour != 0 {
		h++
	}
	return h
}

// LogsTableState is what EnsureLogsTable found.
type LogsTableState int

const (
	// LogsTableUnavailable: ClickHouse unreachable or the DDL failed. Retry.
	LogsTableUnavailable LogsTableState = iota
	// LogsTableReady: otel_logs has every exporter and Nudgebee column.
	LogsTableReady
	// LogsTableLegacyShape: otel_logs exists but was created by the exporter's
	// own schema. Inserts still work; queries would full-scan. Needs a rename.
	LogsTableLegacyShape
)

func (s LogsTableState) String() string {
	switch s {
	case LogsTableReady:
		return "ready"
	case LogsTableLegacyShape:
		return "legacy-shape"
	default:
		return "unavailable"
	}
}

// EnsureLogsTable creates otel_logs if absent and checks it has the shape the
// exporter and our queries need. Safe to call repeatedly.
func EnsureLogsTable(ctx context.Context, c *Client, retention time.Duration) (LogsTableState, error) {
	if c == nil {
		return LogsTableUnavailable, errors.New("clickhouse: not configured")
	}
	if err := c.Exec(ctx, LogsTableDDL(c.Database, retention)); err != nil {
		return LogsTableUnavailable, fmt.Errorf("create %s: %w", LogsTable, err)
	}
	cols, err := columnsOf(ctx, c, LogsTable)
	if err != nil {
		return LogsTableUnavailable, fmt.Errorf("read %s columns: %w", LogsTable, err)
	}
	var missing []string
	for _, group := range [][]string{exporterInsertColumns, nudgebeeLogColumns} {
		for _, name := range group {
			if _, ok := cols[name]; !ok {
				missing = append(missing, name)
			}
		}
	}
	if len(missing) > 0 {
		db, tbl := quoteIdent(c.Database), quoteIdent(LogsTable)
		return LogsTableLegacyShape, fmt.Errorf(
			"%s.%s exists without columns [%s]; run RENAME TABLE %s.%s TO %s.%s and the runner will recreate it",
			db, tbl, strings.Join(missing, ", "), db, tbl, db, quoteIdent(LogsTable+"_legacy"))
	}
	return LogsTableReady, nil
}

// KeepEnsuringLogsTable calls EnsureLogsTable until the table is ready or ctx
// ends. The collector may start first and ClickHouse may restart, so a single
// attempt is not enough. Logs once per state change, not every attempt.
func KeepEnsuringLogsTable(ctx context.Context, c *Client, retention, interval time.Duration, logger *slog.Logger) LogsTableState {
	last := LogsTableState(-1)
	for {
		state, err := EnsureLogsTable(ctx, c, retention)
		if state == LogsTableReady {
			logger.Info("otel_logs ready", "retention", retention)
			return state
		}
		if state != last {
			logger.Warn("otel_logs not ready; retrying", "state", state.String(), "err", err)
			last = state
		}
		select {
		case <-ctx.Done():
			return state
		case <-time.After(interval):
		}
	}
}
