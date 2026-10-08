#!/usr/bin/env bash
# Render-level checks for ClickHouse logs (#40144).
# Run from the repo root after: helm dependency build charts/nudgebee-agent
set -euo pipefail
CHART=charts/nudgebee-agent
fail() { echo "FAIL: $*" >&2; exit 1; }
# yq.exe on Windows may emit CRLF; strip it so string comparisons work everywhere.
q() { yq "$@" | tr -d '\r'; }
render() {
  helm template nudgebee-agent "$CHART" --namespace nudgebee \
    --set runner.nudgebee.auth_secret_key=test \
    --set runner.relay_address=ws://relay:8080/register "$@" | tr -d '\r'
}
# Match by substring: the upstream chart may suffix names (e.g. "-agent" in daemonset mode).
gateway_cfg() { q 'select(.kind == "ConfigMap" and (.metadata.name | test("opentelemetry-collector")) and (.metadata.name | test("otel-log-agent") | not)) | .data.relay' <<<"$1"; }

out=$(render)
cfg=$(gateway_cfg "$out")
[ -n "$cfg" ] || fail "gateway collector ConfigMap not found"
[ "$(q '.exporters["clickhouse/logs"].create_schema' <<<"$cfg")" = "false" ] || fail "clickhouse/logs must not create the schema"
[ "$(q '.exporters["clickhouse/logs"].async_insert' <<<"$cfg")" = "true" ]   || fail "clickhouse/logs must use async_insert"
[ "$(q '.exporters["clickhouse/logs"].logs_table_name' <<<"$cfg")" = "otel_logs" ] || fail "wrong logs table"
[ "$(q '.service.pipelines.logs.exporters[0]' <<<"$cfg")" = "clickhouse/logs" ] || fail "logs pipeline must export to clickhouse/logs"
grep -q 'name: LOGS_CLICKHOUSE_ENABLED' <<<"$out" || fail "runner missing LOGS_CLICKHOUSE_ENABLED"
grep -A1 'name: LOGS_RETENTION' <<<"$out" | grep -q '"72h"' || fail "runner LOGS_RETENTION default must be 72h"

# Capture renders first so a failing `helm template` aborts under set -e.
ret=$(render --set logs.retention=24h)
grep -A1 'name: LOGS_RETENTION' <<<"$ret" | grep -q '"24h"' || fail "logs.retention not passed through"
off=$(render --set opentelemetry-collector.enabled=false)
if grep -q 'name: LOGS_CLICKHOUSE_ENABLED' <<<"$off"; then fail "LOGS_CLICKHOUSE_ENABLED must be absent without the collector"; fi
echo "PASS: gateway + runner"
