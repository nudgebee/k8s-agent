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
    {{- if .Values.rsa }}
    - name: RSA_PRIVATE_KEY_PATH
      value: /etc/nudgebee/auth/prv
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
