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
# Longer than the 10s default: a timed-out insert that ClickHouse committed is retried and duplicated.
[ "$(q '.exporters["clickhouse/logs"].timeout' <<<"$cfg")" = "30s" ] || fail "clickhouse/logs timeout must be 30s"
# The pre-upgrade schema job ALTERs the logs table that clickhouse/logs writes.
job_logs_table() { q 'select(.kind == "Job" and (.metadata.name | test("ch-schema-upgrade"))) | .spec.template.spec.containers[0].env[] | select(.name == "CH_LOGS_TABLE") | .value' <<<"$1"; }
[ "$(job_logs_table "$out")" = "otel_logs" ] || fail "schema upgrade job must default the logs table to otel_logs"
job=$(render --set 'opentelemetry-collector.config.exporters.clickhouse/logs.logs_table_name=custom_logs')
[ "$(job_logs_table "$job")" = "custom_logs" ] || fail "schema upgrade job must take the logs table from clickhouse/logs"
grep -q 'name: LOGS_CLICKHOUSE_ENABLED' <<<"$out" || fail "runner missing LOGS_CLICKHOUSE_ENABLED"
grep -A1 'name: LOGS_RETENTION' <<<"$out" | grep -q '"72h"' || fail "runner LOGS_RETENTION default must be 72h"

# Capture renders first so a failing `helm template` aborts under set -e.
ret=$(render --set logs.retention=24h)
grep -A1 'name: LOGS_RETENTION' <<<"$ret" | grep -q '"24h"' || fail "logs.retention not passed through"
ret=$(render --set logs.retention=72h)
grep -A1 'name: LOGS_RETENTION' <<<"$ret" | grep -q '"72h"' || fail "logs.retention=72h must render"
# The runner reads LOGS_RETENTION with time.ParseDuration, which has no days:
# a bad value must stop the install, not silently become 72h.
for bad in 7d ""; do
  if err=$(render --set "logs.retention=$bad" 2>&1); then fail "logs.retention='$bad' must fail the render"; fi
  grep -q 'logs.retention must be a duration in hours, minutes or seconds' <<<"$err" || fail "logs.retention='$bad': unexpected error: $err"
done
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
q '.receivers.filelog.exclude[]' <<<"$agent_cfg" | grep -q '^/var/log/pods/nudgebee_nudgebee-agent-otel-log-agent' || fail "log agent must exclude its own logs"
[ "$(q '.service.pipelines | keys | length' <<<"$agent_cfg")" = "1" ] || fail "log agent must only run a logs pipeline"
q 'select(.kind == "Service") | .metadata.name' <<<"$on" | grep -qx 'nudgebee-agent-opentelemetry-collector' || fail "gateway Service name changed"
q 'select(.kind == "DaemonSet" and (.metadata.name | test("otel-log-agent"))) | .spec.template.spec.volumes[].hostPath.path' <<<"$on" | grep -qx '/var/log/pods' || fail "log agent must mount /var/log/pods"
agent_sc=$(q 'select(.kind == "DaemonSet" and (.metadata.name | test("otel-log-agent"))) | .spec.template.spec.containers[0].securityContext' <<<"$on")
[ "$(q '.runAsUser' <<<"$agent_sc")" = "0" ] || fail "log agent must run as root to read pod logs"
# Root only for file ownership: no privileges beyond that.
[ "$(q '.allowPrivilegeEscalation' <<<"$agent_sc")" = "false" ] || fail "log agent must set allowPrivilegeEscalation: false"
[ "$(q '.capabilities.drop | join(",")' <<<"$agent_sc")" = "ALL" ] || fail "log agent must drop ALL capabilities"
[ "$(q '.readOnlyRootFilesystem' <<<"$agent_sc")" = "true" ] || fail "log agent must have a read-only root filesystem"
[ "$(q '.seccompProfile.type' <<<"$agent_sc")" = "RuntimeDefault" ] || fail "log agent must use the RuntimeDefault seccomp profile"
svc_stmts=$(q '.processors["transform/service_name"].log_statements[].statements[]' <<<"$agent_cfg")
[ -n "$svc_stmts" ] || fail "transform/service_name statements missing"
if grep -q 'service.name"\] == nil' <<<"$svc_stmts"; then fail "service.name statements must be unconditional (k8s_attributes pre-sets it from labels)"; fi
tail -n1 <<<"$svc_stmts" | grep -q 'attributes\["k8s.deployment.name"\]' || fail "last service.name statement must use k8s.deployment.name"
# Mirror (static control-plane) pods can't be associated by k8s_attributes; the agent only reads its own node's logs.
[ "$(q '.processors["resource/node"].attributes[] | select(.key == "k8s.node.name") | .action' <<<"$agent_cfg")" = "insert" ] || fail "resource/node must insert k8s.node.name (never overwrite k8s_attributes)"
[ "$(q '.processors["resource/node"].attributes[] | select(.key == "k8s.node.name") | .value' <<<"$agent_cfg")" = '${env:K8S_NODE_NAME}' ] || fail "resource/node must take the node from K8S_NODE_NAME"
procs=$(q '.service.pipelines.logs.processors | join(",")' <<<"$agent_cfg")
case "$procs" in *k8s_attributes,resource/node*) ;; *) fail "resource/node must run right after k8s_attributes: $procs";; esac
q 'select(.kind == "DaemonSet" and (.metadata.name | test("otel-log-agent"))) | .spec.template.spec.containers[0].env[] | select(.name == "K8S_NODE_NAME") | .valueFrom.fieldRef.fieldPath' <<<"$on" | grep -qx 'spec.nodeName' || fail "log agent must have K8S_NODE_NAME from spec.nodeName"
echo "PASS: log agent"
