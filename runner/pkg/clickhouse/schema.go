package clickhouse

// Materialized-column management for the OTel traces table.
//
// The backend serves trace queries from one of two SQL shapes, picked by the
// `hasMaterializedColumn` flag the agent reports in its heartbeat:
//
//   - true  — SELECT the materialized columns directly (workload_name,
//     trace_source, …). Cheap: ClickHouse computed them at insert time.
//   - false — recompute every one of them from the raw SpanAttributes /
//     ResourceAttributes maps on every scan.
//
// Those columns are not part of the OTel exporter's schema; the agent adds
// them. The legacy Python runner did this at startup (safe_alter_clickhouse_table)
// and probed system.columns to report the flag honestly. Neither survived the
// Go rewrite: the flag was hardcoded false and the columns stopped being
// created, so every install silently took the slow path and pre-existing
// installs were mis-reported as lacking columns they actually had.
//
// EnsureMaterializedColumns restores both halves.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// TracesTable is the OTel exporter's traces table. The exporter only creates
// its tables when absent — it never ALTERs one — so additive schema changes
// are ours to make.
const TracesTable = "otel_traces"

// scopeNameColumn is the collector's own scope column, added in the
// 0.75 -> 0.156 exporter upgrade. Its presence is what decides which
// trace_source expression a given install can actually run: referencing it on
// a table that predates it fails the ALTER outright.
const scopeNameColumn = "ScopeName"

// ebpfScopeName identifies spans emitted by our eBPF node agent, as opposed to
// application-instrumented OTel spans.
const ebpfScopeName = "nudgebee-node-agent"

// materializedColumns are the columns the backend's materialized query shape
// selects, in the order the legacy runner declared them. Definitions are ported
// verbatim except trace_source, which is built per-install by
// traceSourceDefinition.
//
// Adding an entry here is enough to roll it out — the ensure pass adds whatever
// is missing, and the flag is computed from this map rather than a hardcoded
// count, so a new column can't silently flip installs to the slow path.
var materializedColumns = map[string]string{
	"workload_namespace": "String MATERIALIZED CASE WHEN mapContains(SpanAttributes, 'source.workload_namespace') " +
		"THEN SpanAttributes['source.workload_namespace'] WHEN mapContains(ResourceAttributes, " +
		"'k8s.namespace.name') THEN ResourceAttributes['k8s.namespace.name'] ELSE " +
		"ResourceAttributes['service.namespace'] END CODEC(ZSTD(1))",
	"workload_name": "String MATERIALIZED CASE WHEN mapContains(SpanAttributes, 'source.workload_name') THEN " +
		"SpanAttributes['source.workload_name'] WHEN mapContains(ResourceAttributes, " +
		"'k8s.deployment.name') THEN ResourceAttributes['k8s.deployment.name'] ELSE ResourceAttributes[" +
		"'service.name'] END CODEC(ZSTD(1))",
	// http.url before url.full: the node agent puts the templated path in
	// http.url so trace groups don't split per ID, and keeps the raw URL in
	// url.full. OTel SDKs on semconv >= 1.21 send only url.full; without the
	// last arm their client spans had an empty resource. The query string is
	// cut so tokens in it are not stored and don't split groups either.
	"resource": "String MATERIALIZED CASE WHEN mapContains(SpanAttributes, 'db.statement') THEN SpanAttributes[" +
		"'db.statement'] WHEN SpanAttributes['http.url'] != '' THEN SpanAttributes['http.url'] ELSE " +
		"cutQueryStringAndFragment(SpanAttributes['url.full']) END CODEC(ZSTD(1))",
	"destination_workload_name": "String MATERIALIZED CASE WHEN mapContains(SpanAttributes, " +
		"'destination.workload_name') THEN SpanAttributes['destination.workload_name'] WHEN " +
		"mapContains(ResourceAttributes, 'k8s.deployment.name') THEN ResourceAttributes[" +
		"'k8s.deployment.name'] WHEN mapContains(ResourceAttributes, 'service.name') THEN " +
		"ResourceAttributes['service.name'] ELSE ResourceAttributes['net.peer.name'] END CODEC(ZSTD(1))",
	"destination_workload_namespace": "String MATERIALIZED CASE WHEN mapContains(SpanAttributes, " +
		"'destination.workload_namespace') THEN SpanAttributes['destination.workload_namespace'] WHEN " +
		"mapContains(ResourceAttributes, 'k8s.namespace.name') THEN ResourceAttributes['k8s.namespace.name'] " +
		"ELSE ResourceAttributes['service.namespace'] END CODEC(ZSTD(1))",
	"destination_name": "String MATERIALIZED CASE WHEN mapContains(SpanAttributes, 'destination.name') THEN " +
		"SpanAttributes['destination.name'] WHEN mapContains(ResourceAttributes, 'service.name') THEN " +
		"ResourceAttributes['service.name'] ELSE ResourceAttributes['net.peer.name'] END CODEC(ZSTD(1))",
	"headers":          "String MATERIALIZED base64Decode(toString(SpanAttributes['http.headers'])) CODEC(ZSTD(1))",
	"http_status_code": "LowCardinality(String) MATERIALIZED toString(SpanAttributes['http.status_code']) CODEC(ZSTD(1))",
	"request_payload":  "String MATERIALIZED toString(SpanAttributes['http.request_payload']) CODEC(ZSTD(1))",
	"http_response":    "String MATERIALIZED toString(SpanAttributes['http.response']) CODEC(ZSTD(1))",
	"workload_zone":    "String MATERIALIZED toString(ResourceAttributes['cloud.availability_zone']) CODEC(ZSTD(1))",
	"destination_workload_zone": "String MATERIALIZED toString(SpanAttributes['destination.cloud.availablity_zone']) " +
		"CODEC(ZSTD(1))",
	"cloud_availability_zone": "String MATERIALIZED toString(ResourceAttributes['cloud.availability_zone']) " +
		"CODEC(ZSTD(1))",
	// trace_source is filled in by requiredColumns — its definition depends on
	// whether this install's table has ScopeName.
	"trace_source": "",
}

// traceSourceDefinition returns the trace_source column definition valid for a
// table with the given column set.
//
// The collector upgrade moved the scope name out of SpanAttributes and into a
// dedicated ScopeName column: on 0.157 tables SpanAttributes['otel.scope.name']
// is empty on every span, and on pre-0.156 tables ScopeName does not exist at
// all. Neither expression works on both, and a bare reference to a missing
// column fails the whole statement at name-resolution — so the OR arm cannot
// serve as a fallback and the choice has to be made here, from the real schema.
func traceSourceDefinition(columns map[string]struct{}) string {
	expr := fmt.Sprintf("toString(SpanAttributes['otel.scope.name']) = '%s'", ebpfScopeName)
	if _, ok := columns[scopeNameColumn]; ok {
		// Keep the attribute arm alongside the column: a table upgraded across
		// the boundary holds spans written under both schemas.
		expr = fmt.Sprintf("%s = '%s' OR %s", scopeNameColumn, ebpfScopeName, expr)
	}
	return fmt.Sprintf("LowCardinality(String) MATERIALIZED CASE WHEN %s THEN 'ebpf' ELSE 'otel' END CODEC(ZSTD(1))", expr)
}

// requiredColumns resolves the schema-dependent definitions against a table's
// actual column set.
func requiredColumns(columns map[string]struct{}) map[string]string {
	out := make(map[string]string, len(materializedColumns))
	for name, def := range materializedColumns {
		if name == "trace_source" {
			def = traceSourceDefinition(columns)
		}
		out[name] = def
	}
	return out
}

// EnsureMaterializedColumns brings otel_traces up to the materialized-column
// schema and reports whether the table now carries all of them — the value the
// heartbeat's hasMaterializedColumn flag must carry.
//
// Safe to call repeatedly: it diffs against system.columns and only issues DDL
// for what is missing or stale. ADD COLUMN ... MATERIALIZED is a metadata-only
// operation in ClickHouse — existing parts keep their data and compute the
// expression on read — so this does not rewrite the table.
//
// Every failure path returns false rather than an error the caller must handle
// as fatal: reporting "no materialized columns" costs a slower query shape,
// which is exactly the degraded mode the backend already supports.
func EnsureMaterializedColumns(ctx context.Context, c *Client, logger *slog.Logger) bool {
	if c == nil {
		return false
	}
	columns, err := tableColumns(ctx, c)
	if err != nil {
		logger.Warn("clickhouse: cannot read otel_traces columns; reporting no materialized columns", "err", err)
		return false
	}
	if len(columns) == 0 {
		// The exporter creates the table lazily on the first span batch. Nothing
		// to alter yet; a later tick picks it up.
		logger.Info("clickhouse: otel_traces does not exist yet; skipping materialized-column setup")
		return false
	}

	required := requiredColumns(columns)
	var adds []string
	for name, def := range required {
		if _, ok := columns[name]; !ok {
			adds = append(adds, fmt.Sprintf("ADD COLUMN %s %s", name, def))
		}
	}
	// Name-presence alone can't see a column whose definition has since changed,
	// so compare the stored expressions against what this schema should have.
	// MODIFY COLUMN on a MATERIALIZED expression is metadata-only: parts already
	// written keep the values they were inserted with, and new spans get the new
	// expression.
	if exprs, err := columnExpressions(ctx, c, "trace_source", "resource"); err != nil {
		logger.Warn("clickhouse: cannot inspect materialized-column definitions", "err", err)
	} else {
		for _, name := range staleColumns(columns, exprs) {
			adds = append(adds, fmt.Sprintf("MODIFY COLUMN %s %s", name, required[name]))
		}
	}

	if len(adds) == 0 {
		return hasAll(columns, required)
	}

	sort.Strings(adds)
	stmt := fmt.Sprintf("ALTER TABLE %s.%s %s", quoteIdent(c.Database), quoteIdent(TracesTable), strings.Join(adds, ", "))
	res, err := c.Query(ctx, stmt, nil)
	if err != nil {
		logger.Warn("clickhouse: materialized-column ALTER failed", "err", err)
		return false
	}
	if res.Error != nil {
		logger.Warn("clickhouse: materialized-column ALTER failed", "err", *res.Error)
		return false
	}
	logger.Info("clickhouse: otel_traces materialized columns updated", "statements", len(adds))

	// Re-read rather than assume: a partially applied ALTER must not report
	// success, or the backend selects columns that aren't there.
	columns, err = tableColumns(ctx, c)
	if err != nil {
		logger.Warn("clickhouse: cannot re-read otel_traces columns after ALTER", "err", err)
		return false
	}
	return hasAll(columns, required)
}

// hasAll reports whether every required column is present. Deliberately a set
// comparison and not a count: the legacy probe asserted `count() = 14`, so
// adding a column to the set would have flipped every install to the slow path
// until each one had been altered.
func hasAll(columns map[string]struct{}, required map[string]string) bool {
	for name := range required {
		if _, ok := columns[name]; !ok {
			return false
		}
	}
	return true
}

// tableColumns returns the column names of otel_traces. An absent table yields
// an empty set, not an error — that is the normal pre-first-span state.
func tableColumns(ctx context.Context, c *Client) (map[string]struct{}, error) {
	q := fmt.Sprintf(
		"SELECT name FROM system.columns WHERE database = '%s' AND table = '%s'",
		escapeLiteral(c.Database), escapeLiteral(TracesTable),
	)
	res, err := c.Query(ctx, q, nil)
	if err != nil {
		return nil, err
	}
	if res.Error != nil {
		return nil, fmt.Errorf("%s", *res.Error)
	}
	out := make(map[string]struct{}, len(res.Data))
	for _, row := range res.Data {
		if len(row) == 0 {
			continue
		}
		if name, ok := row[0].(string); ok && name != "" {
			out[name] = struct{}{}
		}
	}
	return out, nil
}

// columnExpressions returns the stored default_expression of each named
// column that exists on otel_traces.
func columnExpressions(ctx context.Context, c *Client, names ...string) (map[string]string, error) {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + escapeLiteral(n) + "'"
	}
	q := fmt.Sprintf(
		"SELECT name, default_expression FROM system.columns WHERE database = '%s' AND table = '%s' AND name IN (%s)",
		escapeLiteral(c.Database), escapeLiteral(TracesTable), strings.Join(quoted, ", "),
	)
	res, err := c.Query(ctx, q, nil)
	if err != nil {
		return nil, err
	}
	if res.Error != nil {
		return nil, fmt.Errorf("%s", *res.Error)
	}
	out := make(map[string]string, len(res.Data))
	for _, row := range res.Data {
		if len(row) < 2 {
			continue
		}
		name, _ := row[0].(string)
		expr, _ := row[1].(string)
		if name != "" {
			out[name] = expr
		}
	}
	return out, nil
}

// staleColumns names the existing columns whose stored definition predates the
// current one, sorted.
//
//   - trace_source carried over from an install that predates the collector
//     upgrade still tests only SpanAttributes['otel.scope.name'], which is
//     empty on every span the new collector writes — it would silently classify
//     all traffic as 'otel'. Only stale once the table has ScopeName: without
//     the column there is nothing better to migrate to.
//   - resource without the url.full arm leaves OTel SDK client spans with an
//     empty resource.
func staleColumns(columns map[string]struct{}, exprs map[string]string) []string {
	var stale []string
	if expr, ok := exprs["resource"]; ok && !strings.Contains(expr, "url.full") {
		stale = append(stale, "resource")
	}
	if expr, ok := exprs["trace_source"]; ok {
		if _, hasScope := columns[scopeNameColumn]; hasScope && !strings.Contains(expr, scopeNameColumn) {
			stale = append(stale, "trace_source")
		}
	}
	return stale
}

// escapeLiteral escapes a value for a single-quoted ClickHouse string literal —
// the `WHERE database = '…'` position in the system.columns reads.
func escapeLiteral(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

// quoteIdent backtick-quotes a database or table name for the identifier
// position in DDL. Distinct from escapeLiteral: the same name is a quoted
// identifier in `ALTER TABLE db.table` but a string literal in a system.columns
// predicate, and the two take different quoting.
//
// CLICKHOUSE_DB is operator-supplied, so it can legally contain characters —
// a hyphen most likely — that the parser rejects unquoted.
func quoteIdent(s string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(s) + "`"
}
