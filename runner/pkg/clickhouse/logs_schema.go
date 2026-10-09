package clickhouse

// otel_logs: the table container logs land in (#40144).
//
// The gateway collector's clickhouse exporter runs with create_schema: false,
// so this package owns the table. The exporter's own schema (v0.157) sorts by
// (toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp), so a query by
// namespace or pod reads every row in its time range; ours materializes the
// Kubernetes labels and sorts by them.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
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
TTL %s
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1`

// LogsTableDDL returns the CREATE TABLE IF NOT EXISTS statement for otel_logs.
func LogsTableDDL(database string, retention time.Duration) string {
	return fmt.Sprintf(logsTableTemplate, quoteIdent(database), quoteIdent(LogsTable), logsTTL(retention))
}

// logsTTL is the table's TTL expression, written exactly as ClickHouse 24.12
// prints it back in system.tables.engine_full.
func logsTTL(retention time.Duration) string {
	return fmt.Sprintf("toDateTime(Timestamp) + toIntervalHour(%d)", retentionHours(retention))
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

// effectiveRetention is the retention the table actually applies.
func effectiveRetention(d time.Duration) time.Duration {
	return time.Duration(retentionHours(d)) * time.Hour
}

var (
	ttlClausePattern = regexp.MustCompile(`\bTTL (.+?)(?: SETTINGS |$)`)
	ttlHoursPattern  = regexp.MustCompile(`^toDateTime\(Timestamp\) \+ toIntervalHour\((\d+)\)$`)
)

// ensureLogsTTL makes an existing table follow the configured retention.
// CREATE IF NOT EXISTS never changes a table that exists, so without this a
// new logs.retention would only reach a table created after the change.
func ensureLogsTTL(ctx context.Context, c *Client, retention time.Duration, logger *slog.Logger) error {
	q := fmt.Sprintf(
		"SELECT engine_full FROM system.tables WHERE database = '%s' AND name = '%s'",
		escapeLiteral(c.Database), escapeLiteral(LogsTable),
	)
	res, err := c.Query(ctx, q, nil)
	if err != nil {
		return err
	}
	if res.Error != nil {
		return fmt.Errorf("%s", *res.Error)
	}
	if len(res.Data) == 0 || len(res.Data[0]) == 0 {
		return fmt.Errorf("%s not found in system.tables", LogsTable)
	}
	engine, _ := res.Data[0][0].(string)
	var current string
	if m := ttlClausePattern.FindStringSubmatch(engine); m != nil {
		current = m[1]
	}
	want := logsTTL(retention)
	if current == want {
		return nil
	}
	stmt := fmt.Sprintf("ALTER TABLE %s.%s MODIFY TTL %s", quoteIdent(c.Database), quoteIdent(LogsTable), want)
	if err := c.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("modify TTL: %w", err)
	}
	logger.Info("otel_logs retention changed",
		"old_retention", describeTTL(current), "new_retention", effectiveRetention(retention).String())
	return nil
}

// describeTTL renders a TTL clause found in engine_full for a log line.
func describeTTL(clause string) string {
	if m := ttlHoursPattern.FindStringSubmatch(clause); m != nil {
		if h, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			return (time.Duration(h) * time.Hour).String()
		}
	}
	if clause == "" {
		return "none"
	}
	return clause
}

// LogsTableState is what EnsureLogsTable found.
type LogsTableState int

const (
	// LogsTableUnavailable: ClickHouse unreachable or the DDL failed. Retry.
	LogsTableUnavailable LogsTableState = iota
	// LogsTableReady: otel_logs has every exporter and Nudgebee column.
	LogsTableReady
	// LogsTableLegacyShape: otel_logs was created by the exporter's own schema
	// and could not be moved aside automatically (it has rows, or the rename
	// failed). Inserts still work; queries would full-scan. Needs a rename.
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

// EnsureLogsTable creates otel_logs if absent, checks it has the shape the
// exporter and our queries need, and brings its TTL in line with retention.
// An empty exporter-created otel_logs (what the 0.1.27 gateway left behind)
// is renamed to otel_logs_legacy and replaced. Safe to call repeatedly.
func EnsureLogsTable(ctx context.Context, c *Client, retention time.Duration, logger *slog.Logger) (LogsTableState, error) {
	if c == nil {
		return LogsTableUnavailable, errors.New("clickhouse: not configured")
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	missing, err := createLogsTable(ctx, c, retention)
	if err != nil {
		return LogsTableUnavailable, err
	}
	if len(missing) > 0 {
		if err := moveEmptyLegacyTable(ctx, c, logger); err != nil {
			return LogsTableLegacyShape, legacyShapeError(c.Database, missing, err)
		}
		if missing, err = createLogsTable(ctx, c, retention); err != nil {
			return LogsTableUnavailable, err
		}
		if len(missing) > 0 {
			return LogsTableLegacyShape, legacyShapeError(c.Database, missing, errors.New("columns still missing after the rename"))
		}
	}
	if err := ensureLogsTTL(ctx, c, retention, logger); err != nil {
		return LogsTableUnavailable, fmt.Errorf("%s retention: %w", LogsTable, err)
	}
	return LogsTableReady, nil
}

// createLogsTable runs the DDL and returns the exporter and Nudgebee columns
// the table lacks. CREATE IF NOT EXISTS leaves an existing table as it is.
func createLogsTable(ctx context.Context, c *Client, retention time.Duration) ([]string, error) {
	if err := c.Exec(ctx, LogsTableDDL(c.Database, retention)); err != nil {
		return nil, fmt.Errorf("create %s: %w", LogsTable, err)
	}
	cols, err := columnsOf(ctx, c, LogsTable)
	if err != nil {
		return nil, fmt.Errorf("read %s columns: %w", LogsTable, err)
	}
	var missing []string
	for _, group := range [][]string{exporterInsertColumns, nudgebeeLogColumns} {
		for _, name := range group {
			if _, ok := cols[name]; !ok {
				missing = append(missing, name)
			}
		}
	}
	return missing, nil
}

// moveEmptyLegacyTable renames an empty exporter-created otel_logs to
// otel_logs_legacy so the runner's table can take its place. Rows are never
// moved automatically: a table that has any is left for the operator.
func moveEmptyLegacyTable(ctx context.Context, c *Client, logger *slog.Logger) error {
	db, tbl, legacy := quoteIdent(c.Database), quoteIdent(LogsTable), quoteIdent(LogsTable+"_legacy")
	res, err := c.Query(ctx, fmt.Sprintf("SELECT count() FROM %s.%s", db, tbl), nil)
	if err != nil {
		return fmt.Errorf("count rows: %w", err)
	}
	if res.Error != nil {
		return fmt.Errorf("count rows: %s", *res.Error)
	}
	if len(res.Data) == 0 || len(res.Data[0]) == 0 {
		return errors.New("count rows: empty result")
	}
	// JSONCompact quotes UInt64 by default; accept a bare number too.
	var rows uint64
	switch v := res.Data[0][0].(type) {
	case string:
		if rows, err = strconv.ParseUint(v, 10, 64); err != nil {
			return fmt.Errorf("count rows: %w", err)
		}
	case float64:
		rows = uint64(v)
	default:
		return fmt.Errorf("count rows: unexpected value %v", v)
	}
	if rows > 0 {
		return fmt.Errorf("it is not empty: count() = %d", rows)
	}
	if err := c.Exec(ctx, fmt.Sprintf("RENAME TABLE %s.%s TO %s.%s", db, tbl, db, legacy)); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	logger.Info("renamed empty exporter-created otel_logs to otel_logs_legacy; creating the runner's otel_logs",
		"database", c.Database, "from", LogsTable, "to", LogsTable+"_legacy")
	return nil
}

func legacyShapeError(database string, missing []string, notMoved error) error {
	db, tbl, legacy := quoteIdent(database), quoteIdent(LogsTable), quoteIdent(LogsTable+"_legacy")
	cols := strings.Join(exporterInsertColumns, ", ")
	return fmt.Errorf(
		"%s.%s exists without columns [%s] and was not renamed automatically (%v); "+
			"run RENAME TABLE %s.%s TO %s.%s and the runner will recreate it; "+
			"to copy the old rows afterwards, run INSERT INTO %s.%s (%s) SELECT %s FROM %s.%s",
		db, tbl, strings.Join(missing, ", "), notMoved, db, tbl, db, legacy,
		db, tbl, cols, cols, db, legacy)
}

// KeepEnsuringLogsTable calls EnsureLogsTable until the table is ready or ctx
// ends. The collector may start first and ClickHouse may restart, so a single
// attempt is not enough. Logs once per state change, not every attempt.
func KeepEnsuringLogsTable(ctx context.Context, c *Client, retention, interval time.Duration, logger *slog.Logger) LogsTableState {
	last := LogsTableState(-1)
	for {
		state, err := EnsureLogsTable(ctx, c, retention, logger)
		if state == LogsTableReady {
			logger.Info("otel_logs ready", "retention", effectiveRetention(retention).String())
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
