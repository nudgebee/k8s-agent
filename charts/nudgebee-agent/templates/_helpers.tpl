{{/*
Create a fully qualified Prometheus server name
in a similar way as prometheus/templates/_helpers.tpl creates "prometheus.server.fullname".
*/}}
{{- define "nudgebee.prometheus.server.fullname" -}}
{{- if .Values.prometheus.server.fullnameOverride -}}
{{- .Values.prometheus.server.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-prometheus-%s" .Release.Name .Values.prometheus.server.name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}


{{- define "node-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "node-agent.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "node-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "node-agent.labels" -}}
helm.sh/chart: {{ include "node-agent.chart" . }}
{{ include "node-agent.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "node-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "node-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
GOMEMLIMIT (in bytes) for the runner, derived from its container memory limit.

The runner is a Go process and the Go runtime is cgroup-unaware: without
GOMEMLIMIT the GC paces off GOGC alone, so a transient allocation burst can run
the heap past the container limit and get the pod OOM-killed while the live heap
is still small. GOMEMLIMIT is a soft limit — as the heap approaches it the GC
runs progressively harder, trading CPU to stay under the cgroup ceiling.

Derived rather than hardcoded so it cannot drift from resources.limits.memory:
raising the limit raises the headroom automatically. Emits nothing when no
memory limit is set (unbounded cgroup — a soft limit would be arbitrary).

Ratio is runner.goMemLimitRatio (default 0.8); the remaining 20% covers non-heap
RSS (goroutine stacks, runtime metadata, mmap'd binary). Set runner.goMemLimit to
override with an explicit value and skip the derivation entirely.
*/}}
{{- define "nudgebee-agent.goMemLimit" -}}
{{- if .Values.runner.goMemLimit -}}
{{- .Values.runner.goMemLimit -}}
{{- else -}}
{{- include "nudgebee-agent.goMemLimitFrom" (dict "limit" (dig "resources" "limits" "memory" "" .Values.runner) "ratio" .Values.runner.goMemLimitRatio) -}}
{{- end -}}
{{- end -}}

{{/*
GOMEMLIMIT bytes for a memory limit string: limit x ratio (default 0.8).
Takes (dict "limit" <quantity> "ratio" <float>). Emits nothing for an empty or
unparseable limit.
*/}}
{{- define "nudgebee-agent.goMemLimitFrom" -}}
{{- $lim := .limit | toString -}}
{{- $num := regexFind "^[0-9.]+" $lim -}}
{{- if $num -}}
{{- $mult := 1.0 -}}
{{- if hasSuffix "Ki" $lim -}}{{- $mult = 1024.0 -}}
{{- else if hasSuffix "Mi" $lim -}}{{- $mult = 1048576.0 -}}
{{- else if hasSuffix "Gi" $lim -}}{{- $mult = 1073741824.0 -}}
{{- else if hasSuffix "Ti" $lim -}}{{- $mult = 1099511627776.0 -}}
{{- else if hasSuffix "k" $lim -}}{{- $mult = 1000.0 -}}
{{- else if hasSuffix "M" $lim -}}{{- $mult = 1000000.0 -}}
{{- else if hasSuffix "G" $lim -}}{{- $mult = 1000000000.0 -}}
{{- else if hasSuffix "T" $lim -}}{{- $mult = 1000000000000.0 -}}
{{- end -}}
{{- $ratio := .ratio | default 0.8 -}}
{{- printf "%d" (int64 (mulf (float64 $num) $mult $ratio)) -}}
{{- end -}}
{{- end -}}

{{/*
Memory limit of the forwarder (kubewatch) container. The limit equals the
request: kubewatch's footprint is its informer caches, which do not burst.
*/}}
{{- define "nudgebee-agent.kubewatchMemory" -}}
{{- if .Values.isSmallCluster -}}64Mi{{- else -}}{{- .Values.kubewatch.resources.requests.memory -}}{{- end -}}
{{- end -}}

{{/*
"true" when the bundled ClickHouse gets the read-only user query_data uses
(templates/clickhouse-reader-users.yaml): the bundled ClickHouse is installed and
clickhouse.reader.enabled. The StatefulSet mounts the users file itself. An
upgrade with --reuse-values keeps a release's old values, and values from before
the reader have no clickhouse.reader; then the user doesn't exist, so nothing
names it.
*/}}
{{- define "nudgebee-agent.clickhouseReader" -}}
{{- $reader := (default (dict) .Values.clickhouse).reader -}}
{{- if and (include "nudgebee-agent.clickhouse.installed" .) (kindIs "map" $reader) (get $reader "enabled") -}}true{{- end -}}
{{- end -}}

{{/*
The cluster's container-log collection mode (logs.collection), validated:
"none" (default) or "node-agent". One mode per cluster, so no second source
double-ingests. The runner reports clickhouseLogsCollected from it, so the
report can't disagree with what the chart deploys.
*/}}
{{- define "nudgebee-agent.logsCollection" -}}
{{- $mode := dig "collection" "none" (default (dict) .Values.logs) | default "none" -}}
{{- if eq $mode "node-agent" -}}
{{- if not (index (default (dict) (index .Values "opentelemetry-collector")) "enabled") -}}
{{- fail "logs.collection=node-agent sends container logs to ClickHouse through the bundled OpenTelemetry collector: set opentelemetry-collector.enabled=true" -}}
{{- end -}}
{{- if not .Values.nodeAgent.enabled -}}
{{- fail "logs.collection=node-agent needs nodeAgent.enabled=true" -}}
{{- end -}}
{{- if .Values.nodeAgent.logsEndpoint -}}
{{- fail "logs.collection=node-agent sets the node-agent's logs endpoint itself: unset nodeAgent.logsEndpoint" -}}
{{- end -}}
{{- range (default (list) .Values.nodeAgent.env) -}}
{{- if and (kindIs "map" .) (eq (toString (get . "name")) "LOGS_ENDPOINT") -}}
{{- fail "logs.collection=node-agent sets the node-agent's logs endpoint itself: remove LOGS_ENDPOINT from nodeAgent.env" -}}
{{- end -}}
{{- end -}}
{{- else if eq $mode "otel-daemonset" -}}
{{- fail "logs.collection=otel-daemonset is not available yet: use node-agent or none" -}}
{{- else if ne $mode "none" -}}
{{- fail (printf "logs.collection must be none or node-agent, not %q" $mode) -}}
{{- end -}}
{{- $mode -}}
{{- end -}}

{{/*
Runner container template. Invoked with root context: include "nudgebee.runner.container" .
*/}}
{{- define "nudgebee.runner.container" -}}
{{- $logsCollection := include "nudgebee-agent.logsCollection" . }}
- name: runner
  image: "{{ .Values.runner.image.repository }}:{{ .Values.runner.image.tag | default .Chart.AppVersion }}"
  imagePullPolicy: {{ .Values.runner.imagePullPolicy }}
  securityContext:
    allowPrivilegeEscalation: false
    capabilities: {}
    privileged: false
    readOnlyRootFilesystem: false
  env:
    {{- with .Values.runner.nudgebee.authSecretKeyFrom }}
    # Sourced from an externally-provisioned Secret rather than the runner
    # Secret. An explicit `env` entry outranks the same name arriving through
    # `envFrom`, and runner.yaml omits it from the runner Secret in this case,
    # so there is exactly one source either way.
    - name: NUDGEBEE_AUTH_SECRET_KEY
      valueFrom:
        secretKeyRef:
          name: {{ .name }}
          key: {{ .key }}
    {{- end }}
    - name: INSTALLATION_NAMESPACE
      valueFrom:
        fieldRef:
          fieldPath: metadata.namespace
    - name: RUNNER_VERSION
      value: {{ .Chart.AppVersion }}
    {{- with include "nudgebee-agent.goMemLimit" . }}
    - name: GOMEMLIMIT
      value: {{ . | quote }}
    {{- end }}
    - name: WEBSOCKET_RELAY_ADDRESS
      value: {{ .Values.runner.relay_address }}
    - name: SCANNERS_ENABLED
      value: "true"
    {{- if .Values.runner.pprof }}
    - name: PPROF_ENABLED
      value: "true"
    {{- end }}
    - name: SCANNER_NAMESPACE
      value: {{ .Release.Namespace }}
    - name: SCANNER_SERVICE_ACCOUNT
      value: {{ include "nudgebee-agent.fullname" . }}-runner-service-account
    # SCANNER_AUTO_COPY_PULL_SECRETS lets an image scan pull a private workload
    # image by copying that workload's imagePullSecrets into this namespace for
    # the life of the scan Job. Opt-in: it needs read access to registry
    # Secrets in every namespace a scanned workload runs in. Forced off under
    # runner.readOnly, which has no Secrets access at all.
    - name: SCANNER_AUTO_COPY_PULL_SECRETS
      value: {{ if and .Values.runner.scannerAutoCopyPullSecrets (not .Values.runner.readOnly) }}"true"{{ else }}"false"{{ end }}
    {{- with .Values.runner.scaling }}
    {{- if hasKey . "snapshotBatching" }}
    - name: DISCOVERY_SNAPSHOT_BATCHING
      value: {{ .snapshotBatching | quote }}
    {{- end }}
    {{- if .batchSize }}
    - name: DISCOVERY_BATCH_SIZE
      value: {{ .batchSize | quote }}
    {{- end }}
    {{- if .incrementalBatchSize }}
    - name: DISCOVERY_INCREMENTAL_BATCH_SIZE
      value: {{ .incrementalBatchSize | quote }}
    {{- end }}
    {{- if hasKey . "emitTombstones" }}
    - name: DISCOVERY_EMIT_TOMBSTONES
      value: {{ .emitTombstones | quote }}
    {{- end }}
    {{- if .forwardPoolSize }}
    - name: FORWARD_POOL_SIZE
      value: {{ .forwardPoolSize | quote }}
    {{- end }}
    {{- if .relayHandlerPoolSize }}
    - name: RELAY_HANDLER_POOL_SIZE
      value: {{ .relayHandlerPoolSize | quote }}
    {{- end }}
    {{- if .kubewatchWorkers }}
    - name: KUBEWATCH_WORKERS
      value: {{ .kubewatchWorkers | quote }}
    {{- end }}
    {{- if .kubewatchQueueSize }}
    - name: KUBEWATCH_QUEUE_SIZE
      value: {{ .kubewatchQueueSize | quote }}
    {{- end }}
    {{- if .kubewatchQueueMaxBytes }}
    - name: KUBEWATCH_QUEUE_MAX_BYTES
      value: {{ .kubewatchQueueMaxBytes | int64 | quote }}
    {{- end }}
    {{- if hasKey . "eventsCache" }}
    - name: TRIGGER_EVENTS_CACHE
      value: {{ .eventsCache | quote }}
    {{- end }}
    {{- end }}
    {{- if .Values.runner.profilerImage }}
    - name: PROFILER_IMAGE
      value: {{ .Values.runner.profilerImage | quote }}
    {{- end }}
    {{- if .Values.runner.triggerRateLimits }}
    - name: TRIGGER_RATE_LIMITS
      value: {{ .Values.runner.triggerRateLimits | quote }}
    {{- end }}
    # MUTATE_ENABLED gates the runner's mutate subsystem (delete_pod,
    # cordon, rollout_restart, PrometheusRule CRUD, AlertManager silences,
    # Loki rules, ...). The auth boundary lives inside the runner — only
    # the explicitly-allowlisted light actions accept unsigned requests;
    # every other mutate action falls through to the validator's
    # HMAC/RSA-partial-keys path and is rejected without a signed request.
    # So enabling the subsystem here does NOT loosen the security posture
    # on installations that omit `.Values.rsa`; it only makes the
    # light-action carve-outs (currently create_or_replace_alert_rule /
    # delete_alert_rule) reachable end-to-end.
    #
    # Operators who want a strictly read-only deployment can set
    # `runner.mutateEnabled: false`. The `eq ... false` pattern is
    # intentional: `default true` would treat an explicit `false` as
    # unset and re-enable the subsystem.
    - name: MUTATE_ENABLED
      value: {{ if eq .Values.runner.mutateEnabled false }}"false"{{ else }}"true"{{ end }}
    # KUBECTL_ALLOW_WRITE lifts the runner's read-only verb allowlist on
    # kubectl_command_executor. Gated by runner.enableWritePermissions — the
    # SAME switch that grants the service account its write RBAC — so the
    # runner allowlist and the cluster RBAC move together. Default (false)
    # keeps kubectl strictly read-only; mutating verbs (scale/patch/delete/...)
    # are rejected and routed to the signed pkg/mutate actions instead. When
    # true, mutating kubectl runs over the unsigned, relay-secret-gated light
    # action path — the operator granting write RBAC opts into that posture.
    - name: KUBECTL_ALLOW_WRITE
      value: {{ if .Values.runner.enableWritePermissions }}"true"{{ else }}"false"{{ end }}
    {{- if .Values.rsa }}
    - name: RSA_PRIVATE_KEY_PATH
      value: /etc/nudgebee/auth/prv
    {{- end }}
    {{- if .Values.runner.nudgebee.relay_signing_public_key }}
    # Relay Ed25519 public key — the agent verifies relay-signed requests with
    # it and authorizes UI-triggered workload mutations. A valid relay signature
    # is required for mutations; reads always fall back to the light-action path,
    # so this is purely additive.
    - name: RELAY_SIGNING_PUBLIC_KEY
      value: {{ .Values.runner.nudgebee.relay_signing_public_key | quote }}
    {{- end }}
    {{- /*
    ClickHouse wiring for the runner. The in-chart ClickHouse is installed
    only with `opentelemetry-collector.enabled`, so with the collector off
    neither the `<release>-clickhouse` Service nor its Secret exists. Point the
    runner at either of them anyway and kubelet cannot resolve the secretKeyRef:
    the pod never starts and sits in phase Pending with CreateContainerConfigError.
    So the in-chart defaults are gated on the collector being enabled, and only
    the operator-supplied sources (clickhouse_password / clickhouse_secret /
    a CLICKHOUSE_HOST in additional_env_vars) survive with it off.
    */}}
    {{- $otelEnabled := index (default (dict) (index .Values "opentelemetry-collector")) "enabled" }}
    {{- if or $otelEnabled .Values.runner.clickhouse_enabled }}
    {{- $envVarNames := list }}
    {{- if and .Values.runner.additional_env_vars (kindIs "slice" .Values.runner.additional_env_vars) }}
      {{- range .Values.runner.additional_env_vars }}
        {{- if and (kindIs "map" .) (hasKey . "name") }}
          {{- $envVarNames = append $envVarNames .name }}
        {{- end }}
      {{- end }}
    {{- end }}
    {{- /* Only default CLICKHOUSE_HOST to the in-chart Service when it is installed. */}}
    {{- if and $otelEnabled (not (has "CLICKHOUSE_HOST" $envVarNames)) }}
    - name: CLICKHOUSE_HOST
      value: {{ include "nudgebee-agent.clickhouse.servicename" . }}
    {{- end }}
    {{- /*
    Where the bundled collector writes logs, for the runner's ClickHouse logs
    check. Read from the exporter config, as clickhouse-schema-upgrade-job.yaml
    does. With the collector off (or an external writer) these stay unset and
    the runner falls back to otel_logs in CLICKHOUSE_DB.
    */}}
    {{- if $otelEnabled }}
    {{- $chExporter := dig "config" "exporters" "clickhouse" (dict) (index .Values "opentelemetry-collector") }}
    {{- if not (has "CLICKHOUSE_LOGS_DATABASE" $envVarNames) }}
    - name: CLICKHOUSE_LOGS_DATABASE
      value: {{ default "default" (get $chExporter "database") | quote }}
    {{- end }}
    {{- if not (has "CLICKHOUSE_LOGS_TABLE" $envVarNames) }}
    - name: CLICKHOUSE_LOGS_TABLE
      value: {{ default "otel_logs" (get $chExporter "logs_table_name") | quote }}
    {{- end }}
    {{- /*
    query_data reads through the read-only user templates/clickhouse-reader-users.yaml
    creates, which exists only where the bundled ClickHouse mounts it: not when an
    operator points CLICKHOUSE_HOST elsewhere.
    */}}
    {{- if and (include "nudgebee-agent.clickhouseReader" .) (not (has "CLICKHOUSE_HOST" $envVarNames)) (not (has "CLICKHOUSE_READER_USER" $envVarNames)) }}
    - name: CLICKHOUSE_READER_USER
      value: nudgebee_reader
    {{- end }}
    {{- /* Reported as clickhouseLogsCollected; set only by the switch. */}}
    {{- if eq $logsCollection "node-agent" }}
    - name: CLICKHOUSE_LOGS_COLLECTED
      value: "true"
    {{- end }}
    {{- end }}
    {{- /*
    Password source, in precedence order: runner.clickhouse_password (lands in
    the runner Secret), runner.clickhouse_secret (operator-provisioned), then the
    bundled ClickHouse's Secret (clickhouse.auth.existingSecret when set) — the
    last only when the bundled ClickHouse is actually installed.
    No source at all means no env var rather than a reference to a missing Secret.
    */}}
    {{- if .Values.runner.clickhouse_password }}
    - name: CLICKHOUSE_PASSWORD
      valueFrom:
        secretKeyRef:
          name: {{ include "nudgebee-agent.fullname" . }}-runner-secret
          key: CLICKHOUSE_PASSWORD
    {{- else if .Values.runner.clickhouse_secret }}
    - name: CLICKHOUSE_PASSWORD
      valueFrom:
        secretKeyRef:
          name: {{ .Values.runner.clickhouse_secret }}
          key: admin-password
    {{- else if $otelEnabled }}
    - name: CLICKHOUSE_PASSWORD
      valueFrom:
        secretKeyRef:
          name: {{ include "nudgebee-agent.clickhouse.secretName" . }}
          key: {{ include "nudgebee-agent.clickhouse.secretKey" . }}
    {{- end }}
    {{- end }}
    {{- if kindIs "string" .Values.runner.additional_env_vars }}
    {{- fail "The `additional_env_vars` string value is deprecated. Change the `additional_env_vars` value to an array" -}}
    {{- end }}
    {{- if .Values.runner.additional_env_vars }}
    {{ toYaml .Values.runner.additional_env_vars | nindent 4 }}
    {{- end }}
  envFrom:
  - secretRef:
      name: {{ include "nudgebee-agent.fullname" . }}-runner-secret
      optional: true
  {{- if .Values.runner.additional_env_froms }}
  {{ toYaml .Values.runner.additional_env_froms | nindent 2 }}
  {{- end }}
  volumeMounts:
    - name: auth-config-secret
      mountPath: /etc/nudgebee/auth
    {{- with .Values.runner.extraVolumeMounts }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  lifecycle:
    preStop:
      exec:
        command: ["bash", "-c", "kill -SIGINT 1"]
  {{- if and .Values.runner.probes .Values.runner.probes.enabled }}
  {{- $readiness := .Values.runner.probes.readiness | default dict }}
  {{- $liveness := .Values.runner.probes.liveness | default dict }}
  readinessProbe:
    httpGet:
      path: /healthz
      port: 5000
    initialDelaySeconds: {{ $readiness.initialDelaySeconds | default 5 }}
    periodSeconds: {{ $readiness.periodSeconds | default 10 }}
    timeoutSeconds: {{ $readiness.timeoutSeconds | default 3 }}
  livenessProbe:
    httpGet:
      path: /healthz
      port: 5000
    initialDelaySeconds: {{ $liveness.initialDelaySeconds | default 15 }}
    periodSeconds: {{ $liveness.periodSeconds | default 20 }}
    timeoutSeconds: {{ $liveness.timeoutSeconds | default 5 }}
    failureThreshold: {{ $liveness.failureThreshold | default 6 }}
  {{- end }}
  resources:
    requests:
      cpu: {{ .Values.runner.resources.requests.cpu }}
      memory: {{ if .Values.isSmallCluster }}"512Mi"{{ else }}{{ .Values.runner.resources.requests.memory | quote }}{{ end }}
    limits:
      {{ if .Values.runner.resources.limits.memory }}memory: {{ if .Values.isSmallCluster }}"512Mi"{{ else }}{{ .Values.runner.resources.limits.memory | quote }}{{ end }}
      {{ end }}
      {{ if .Values.runner.resources.limits.cpu }}cpu: {{ .Values.runner.resources.limits.cpu | quote }}{{ end }}
{{- end }}

{{/*
Runner volumes template. Invoked with root context.
*/}}
{{- define "nudgebee.runner.volumes" -}}
volumes:
  - name: auth-config-secret
    secret:
      secretName: {{ include "nudgebee-agent.fullname" . }}-auth-config-secret
      optional: true
  {{- with .Values.runner.extraVolumes }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
{{- end }}

{{/*
Runner imagePullSecrets template. Invoked with root context.
*/}}
{{- define "nudgebee.runner.imagePullSecrets" -}}
{{- with .Values.runner.imagePullSecrets }}
imagePullSecrets:
{{- toYaml . | nindent 0 }}
{{- end }}
{{- end }}

{{/*
True on OpenShift: either the user opted in with openshift.enabled, or the
cluster serves the SecurityContextConstraints API. OpenShift assigns each
namespace its own UID range, so pods must not hardcode a runAsUser.
Renders "true" or "" (use with `if include ... .`).
*/}}
{{- define "nudgebee-agent.isOpenShift" -}}
{{- if or .Values.openshift.enabled (.Capabilities.APIVersions.Has "security.openshift.io/v1/SecurityContextConstraints") -}}true{{- end -}}
{{- end -}}

{{/*
Expand the name of the chart.
*/}}
{{- define "nudgebee-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "nudgebee-agent.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "nudgebee-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "nudgebee-agent.labels" -}}
helm.sh/chart: {{ include "nudgebee-agent.chart" . }}
{{ include "nudgebee-agent.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "nudgebee-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nudgebee-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Labels a PrometheusRule / ServiceMonitor / PodMonitor must carry before the cluster's
Prometheus will select it.

prometheus-operator only evaluates an object whose labels match the Prometheus CR's
ruleSelector / serviceMonitorSelector / podMonitorSelector. kube-prometheus-stack defaults
those to `release: <its own Helm release name>`, which is NOT this chart's release name --
the two are separate Helm releases in every install we ship. An object without the matching
labels is accepted by the apiserver and reported healthy; it just never loads.

Empty by default, so this changes nothing until an operator opts in. installation.sh reads
the labels off the Prometheus CR and passes them automatically; set them by hand for a
Prometheus this chart did not install:

  prometheusStack:
    selectorLabels:
      release: kube-prometheus-stack

See runner/pkg/mutate/promruleselector.go, which does the same discovery at runtime for
alert rules created from the UI.
*/}}
{{- define "nudgebee-agent.prometheusSelectorLabels" -}}
{{- with .Values.prometheusStack }}
{{- with .selectorLabels }}
{{- toYaml . }}
{{- end }}
{{- end }}
{{- end }}

{{/*
"true" when the chart installs its own ClickHouse (templates/clickhouse-*.yaml).
It follows opentelemetry-collector.enabled, the condition the Bitnami subchart
had, so turning the chart upgrade on installs exactly what it did before.
*/}}
{{- define "nudgebee-agent.clickhouse.installed" -}}
{{- if index (default (dict) (index .Values "opentelemetry-collector")) "enabled" -}}true{{- end -}}
{{- end -}}

{{/*
app.kubernetes.io/name of the ClickHouse objects. nameOverride changes it, as it
did in the Bitnami subchart.
*/}}
{{- define "nudgebee-agent.clickhouse.name" -}}
{{- default "clickhouse" .Values.clickhouse.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
ClickHouse selector labels. They are in the StatefulSet selector and its
volumeClaimTemplates, which Kubernetes does not let an upgrade change, and the
Services and the ServiceMonitor select on them: keep them as the Bitnami
subchart rendered them.
*/}}
{{- define "nudgebee-agent.clickhouse.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nudgebee-agent.clickhouse.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: clickhouse
{{- end -}}

{{- define "nudgebee-agent.clickhouse.labels" -}}
{{ include "nudgebee-agent.clickhouse.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ include "nudgebee-agent.chart" . }}
{{- end -}}

{{/*
The Secret and key that hold the ClickHouse admin password:
clickhouse.auth.existingSecret / existingSecretKey when set, else the chart's own
<fullname> Secret under admin-password.
*/}}
{{- define "nudgebee-agent.clickhouse.secretName" -}}
{{- default (include "nudgebee-agent.clickhouse.servicename" .) (dig "auth" "existingSecret" "" .Values.clickhouse) -}}
{{- end -}}

{{- define "nudgebee-agent.clickhouse.secretKey" -}}
{{- if dig "auth" "existingSecret" "" .Values.clickhouse -}}
{{- default "admin-password" (dig "auth" "existingSecretKey" "" .Values.clickhouse) -}}
{{- else -}}
admin-password
{{- end -}}
{{- end -}}

{{/*
ClickHouse fullname (Services, Secret, ConfigMaps, StatefulSet prefix). Same
rules as the Bitnami subchart's common.names.fullname, so the names don't change.
*/}}
{{- define "nudgebee-agent.clickhouse.servicename" -}}
{{- if .Values.clickhouse.fullnameOverride -}}
{{- .Values.clickhouse.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default "clickhouse" .Values.clickhouse.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
PromQL label matchers for this release's ClickHouse scrape target. The Prometheus
operator labels every ServiceMonitor target with the Service's namespace and name;
job is the Service name too, unless serviceMonitor.jobLabel renames it.
*/}}
{{- define "nudgebee-agent.clickhouse.target" -}}
{{- $ns := default .Release.Namespace .Values.clickhouse.namespaceOverride | trunc 63 | trimSuffix "-" -}}
{{- printf "namespace=%q,service=%q" $ns (include "nudgebee-agent.clickhouse.servicename" .) -}}
{{- end -}}

{{/*
A value copied into the collector config by a {{ }} reference in its values (the
clickhouse/logs exporter). The collector chart renders its config with
tpl (toYaml ...), which leaves the reference inside single quotes, so a ' in the
value is doubled to keep the rendered YAML valid.
*/}}
{{- define "nudgebee-agent.collectorQuoted" -}}
{{- replace "'" "''" (toString .) -}}
{{- end -}}

{{/*
OpenTelemetry Collector service name - handles fullnameOverride/nameOverride for otel subchart
*/}}
{{- define "nudgebee-agent.otelcollector.servicename" -}}
{{- if index .Values "opentelemetry-collector" "fullnameOverride" -}}
{{- index .Values "opentelemetry-collector" "fullnameOverride" | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default "opentelemetry-collector" (index .Values "opentelemetry-collector" "nameOverride") -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
OpenCost service name - handles fullnameOverride/nameOverride for opencost subchart
*/}}
{{- define "nudgebee-agent.opencost.servicename" -}}
{{- if .Values.opencost.fullnameOverride -}}
{{- .Values.opencost.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default "opencost" .Values.opencost.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}
