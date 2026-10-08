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
	"fmt"
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
        match(Body, '(?i)\b(fatal|panic|critical)\b'), 'critical',
        match(Body, '(?i)\b(error|err|exception)\b'), 'error',
        match(Body, '(?i)\bwarn(ing)?\b'), 'warning',
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
