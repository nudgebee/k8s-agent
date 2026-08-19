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
{{- $lim := (dig "resources" "limits" "memory" "" .Values.runner) | toString -}}
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
{{- $ratio := .Values.runner.goMemLimitRatio | default 0.8 -}}
{{- printf "%d" (int64 (mulf (float64 $num) $mult $ratio)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Runner container template. Invoked with root context: include "nudgebee.runner.container" .
*/}}
{{- define "nudgebee.runner.container" -}}
- name: runner
  image: "{{ .Values.runner.image.repository }}:{{ .Values.runner.image.tag | default .Chart.AppVersion }}"
  imagePullPolicy: {{ .Values.runner.imagePullPolicy }}
  securityContext:
    allowPrivilegeEscalation: false
    capabilities: {}
    privileged: false
    readOnlyRootFilesystem: false
  env:
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
    {{- if .Values.runner.scannerAutoCopyPullSecrets }}
    - name: SCANNER_AUTO_COPY_PULL_SECRETS
      value: "true"
    {{- end }}
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
    {{- end }}
    {{- if .Values.runner.profilerImage }}
    - name: PROFILER_IMAGE
      value: {{ .Values.runner.profilerImage | quote }}
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
    {{- if or (index (default (dict) (index .Values "opentelemetry-collector")) "enabled") .Values.runner.clickhouse_enabled }}
    {{- $clickhouseSecret := .Values.runner.clickhouse_secret }}
    {{- if not $clickhouseSecret }}
      {{- $clickhouseSecret = include "nudgebee-agent.clickhouse.servicename" . }}
    {{- end }}
    {{- $envVarNames := list }}
    {{- if and .Values.runner.additional_env_vars (kindIs "slice" .Values.runner.additional_env_vars) }}
      {{- range .Values.runner.additional_env_vars }}
        {{- if and (kindIs "map" .) (hasKey . "name") }}
          {{- $envVarNames = append $envVarNames .name }}
        {{- end }}
      {{- end }}
    {{- end }}
    {{- if not (has "CLICKHOUSE_HOST" $envVarNames) }}
    - name: CLICKHOUSE_HOST
      value: {{ include "nudgebee-agent.clickhouse.servicename" . }}
    {{- end }}
    - name: CLICKHOUSE_PASSWORD
      valueFrom:
        secretKeyRef:
          name: {{ if .Values.runner.clickhouse_password }}{{ include "nudgebee-agent.fullname" . }}-runner-secret{{ else }}{{ $clickhouseSecret }}{{ end }}
          key: {{ if .Values.runner.clickhouse_password }}CLICKHOUSE_PASSWORD{{ else }}admin-password{{ end }}
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
ClickHouse service name - handles fullnameOverride/nameOverride for clickhouse subchart
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
