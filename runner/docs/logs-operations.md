# Logs operations (otel_logs)

How the ClickHouse logs table behaves across installs, upgrades and restarts
(#40144). The row shape is in [logs-row-contract.md](logs-row-contract.md).

The runner owns `<CLICKHOUSE_DB>.otel_logs` (default database `default`): it
creates the table, keeps its TTL in line with `logs.retention`, and retries
every 30s until the table is ready. The gateway's `clickhouse/logs` exporter
only inserts (`create_schema: false`).

## Upgrading from 0.1.27 or earlier

The 0.1.27 gateway wrote logs with the `clickhouse` exporter, whose
`create_schema` defaults to true, so existing installs already have an
`otel_logs` in the exporter's own shape. That table lacks the runner's columns
(`namespace`, `workload`, `pod`, `container`, `node`, `stream`, `level`) and
sorts by `(toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp)`.

- **Empty table:** handled automatically. The runner renames it to
  `otel_logs_legacy`, logs `renamed empty exporter-created otel_logs to
  otel_logs_legacy` at Info, creates its own `otel_logs` and logs
  `otel_logs ready`.
- **Table with rows:** the runner leaves it alone and logs
  `otel_logs not ready; retrying` with `state: legacy-shape`. Rename it
  yourself, wait for `otel_logs ready` in the runner log (up to 30s), then copy
  the old rows across if you want them:

  ```sql
  RENAME TABLE default.otel_logs TO default.otel_logs_legacy;

  -- after the runner logs "otel_logs ready":
  INSERT INTO default.otel_logs
    (Timestamp, TraceId, SpanId, TraceFlags, SeverityText, SeverityNumber,
     ServiceName, Body, ResourceSchemaUrl, ResourceAttributes, ScopeSchemaUrl,
     ScopeName, ScopeVersion, ScopeAttributes, LogAttributes)
  SELECT Timestamp, TraceId, SpanId, TraceFlags, SeverityText, SeverityNumber,
     ServiceName, Body, ResourceSchemaUrl, ResourceAttributes, ScopeSchemaUrl,
     ScopeName, ScopeVersion, ScopeAttributes, LogAttributes
  FROM default.otel_logs_legacy;
  ```

  The materialized Kubernetes columns compute themselves on insert. Run the
  INSERT once: a second run duplicates every row. Drop `otel_logs_legacy`
  when you no longer need it. Use your `CLICKHOUSE_DB` in place of `default`.

Inserts keep working while the table has the legacy shape, so rows written
before the rename stay in `otel_logs_legacy` until you copy them.

## Delivery is at-least-once

If the exporter's insert times out after ClickHouse has already committed it,
the exporter retries and the batch is written twice. `otel_logs` is a plain
MergeTree, and ClickHouse does not deduplicate async inserts into it, so
queries can see occasional duplicate rows. Deduplication is #40148.

## Expected gateway ERROR on a fresh install

On a fresh install the gateway usually starts before the runner has created
`otel_logs`, and logs this at ERROR as it starts:

```
schema detection failed ... Table default.otel_logs does not exist
```

It is expected, and rows flow once the table exists. The exporter detects
optional columns such as `EventName` only at start, so until the gateway
restarts it does not write `EventName`. Container logs carry no `EventName`;
if applications send OTLP log events that need it, restart the gateway once
after `otel_logs ready`.

## First start of the log agent on a node

The per-node log agent (`otel-log-agent`) reads with `start_at: end`. On a
node's first agent start (including nodes added later), lines already in the
log files are not collected; a quiet pod appears once it logs again. After
that the agent checkpoints its offsets in `/var/lib/otelcol` on the node, so
restarts resume where they stopped and new files are read from the beginning.

## Changing retention

`logs.retention` takes hours, minutes and/or seconds (`72h`, `90m`, `168h`).
The chart refuses anything else, such as `7d`. The table keeps whole hours,
rounded up.

A change applies on the next runner start, which a `helm upgrade` that changes
the value triggers: the runner compares the table's TTL with the configured
retention and runs `ALTER TABLE ... MODIFY TTL` when they differ, logging
`otel_logs retention changed` with the old and new values. ClickHouse then
re-evaluates the TTL on existing parts in the background.
