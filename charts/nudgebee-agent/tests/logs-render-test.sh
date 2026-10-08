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

# --- per-node log agent (#40144) ---
def=$(render)
if q 'select(.kind == "DaemonSet") | .metadata.name' <<<"$def" | grep -q otel-log-agent; then fail "log agent must be off by default"; fi

on=$(render --set otel-log-agent.enabled=true)
q 'select(.kind == "DaemonSet") | .metadata.name' <<<"$on" | grep -q 'otel-log-agent' || fail "no log agent DaemonSet"
agent_cfg=$(q 'select(.kind == "ConfigMap" and (.metadata.name | test("otel-log-agent"))) | .data.relay' <<<"$on")
[ -n "$agent_cfg" ] || fail "log agent ConfigMap not found"
[ "$(q '.service.pipelines.logs.receivers[0]' <<<"$agent_cfg")" = "filelog" ] || fail "log agent must read filelog"
# 0.165.0 names the preset processor k8s_attributes (underscore), not k8sattributes.
q '.service.pipelines.logs.processors[]' <<<"$agent_cfg" | grep -qx k8s_attributes || fail "log agent must run k8sattributes"
q '.processors.k8s_attributes.extract.metadata[]' <<<"$agent_cfg" | grep -qx k8s.deployment.name || fail "k8sattributes must extract the owning deployment"
[ "$(q '.exporters.otlp.endpoint' <<<"$agent_cfg")" = "nudgebee-agent-opentelemetry-collector:4317" ] || fail "log agent must send to the gateway"
q '.receivers.filelog.operators[].type' <<<"$agent_cfg" | grep -qx container || fail "CRI container parser missing"
q '.receivers.filelog.operators[].type' <<<"$agent_cfg" | grep -qx recombine || fail "stack-trace recombine missing"
q '.receivers.filelog.exclude[]' <<<"$agent_cfg" | grep -q 'otel-log-agent' || fail "log agent must exclude its own logs"
[ "$(q '.service.pipelines | keys | length' <<<"$agent_cfg")" = "1" ] || fail "log agent must only run a logs pipeline"
q 'select(.kind == "Service") | .metadata.name' <<<"$on" | grep -qx 'nudgebee-agent-opentelemetry-collector' || fail "gateway Service name changed"
q 'select(.kind == "DaemonSet" and (.metadata.name | test("otel-log-agent"))) | .spec.template.spec.volumes[].hostPath.path' <<<"$on" | grep -qx '/var/log/pods' || fail "log agent must mount /var/log/pods"
[ "$(q 'select(.kind == "DaemonSet" and (.metadata.name | test("otel-log-agent"))) | .spec.template.spec.containers[0].securityContext.runAsUser' <<<"$on")" = "0" ] || fail "log agent must run as root to read pod logs"
echo "PASS: log agent"
