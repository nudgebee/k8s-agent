{{ define "robusta.configfile" -}}
playbook_repos:
{{ toYaml .Values.playbookRepos | indent 2 }}

{{- if and (eq (len .Values.sinksConfig) 0) (and (not .Values.slackApiKey) (not .Values.robustaApiKey)) }}
{{- fail "At least one sink must be defined!" }}
{{- end }}

{{- range .Values.sinksConfig }}
  {{- if .robusta_sink }}
    {{- if $.Values.disableCloudRouting }}
      {{- fail "You cannot set `disableCloudRouting: true` when the Robusta UI sink (robusta_sink) is enabled, as this flag breaks the UI's behavior.\nPlease remove `disableCloudRouting: true` to continue installing." -}}
    {{- end }}
  {{- end }}
{{- end }}

{{- if or .Values.slackApiKey .Values.robustaApiKey }}
{{- /* support old values files, prior to chart version 0.8.9 */}}
sinks_config:
{{- if .Values.slackApiKey }}
- slack_sink:
    name: slack sink
    api_key: {{ .Values.slackApiKey }}
    slack_channel: {{ required "A valid .Values.slackChannel entry is required!" .Values.slackChannel }}
{{- end }}
{{- if .Values.robustaApiKey }}
- robusta_sink:
    name: robusta_ui_sink
    token: {{ .Values.robustaApiKey }}
{{- end }}

{{ else }}
sinks_config:
{{ toYaml .Values.sinksConfig }}
{{- end }}

global_config:
  {{- if .Values.globalConfig }}
{{ toYaml .Values.globalConfig | indent 2 }}
  {{- end }}

alert_relabel:
{{ toYaml  .Values.alertRelabel | indent 2 }}

light_actions:
{{ toYaml  .Values.lightActions | indent 2 }}

active_playbooks:
{{- if .Values.playbooks }}
  {{- fail "The `playbooks` value is deprecated. Rename `playbooks`  to `customPlaybooks` and remove builtin playbooks which are now defined separately" -}}
{{- end }}

{{- if .Values.priorityBuiltinPlaybooks }}
{{ toYaml .Values.priorityBuiltinPlaybooks | indent 2 }}
{{- end }}

{{- if .Values.customPlaybooks }}
{{ toYaml .Values.customPlaybooks | indent 2 }}
{{- end }}

{{- if .Values.builtinPlaybooks }}
{{ toYaml .Values.builtinPlaybooks | indent 2 }}
{{- end }}

{{- if and .Values.enablePlatformPlaybooks .Values.platformPlaybooks }}
{{ toYaml .Values.platformPlaybooks | indent 2 }}
{{- end }}
{{ end }}

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
Common runner container template
Usage: include "nudgebee.runner.container" (dict "root" . "config" .Values.runner "containerName" "runner" "runnerMode" "BACKGROUND")
*/}}
{{- define "nudgebee.runner.container" -}}
{{- $root := .root }}
{{- $config := .config }}
{{- $containerName := .containerName }}
{{- $runnerMode := .runnerMode }}
{{- $isDualMode := and $root.Values.apiServer.enabled (eq $containerName "runner") }}
- name: {{ $containerName }}
  image: "{{ default $root.Values.runner.image.repository $config.image.repository }}:{{ $config.image.tag | default $root.Values.runner.image.tag | default $root.Chart.AppVersion }}"
  imagePullPolicy: {{ default $root.Values.runner.imagePullPolicy $config.imagePullPolicy }}
  securityContext:
    allowPrivilegeEscalation: false
    capabilities: {}
    privileged: false
    readOnlyRootFilesystem: false
  env:
    - name: PLAYBOOKS_CONFIG_FILE_PATH
      value: /etc/robusta/config/active_playbooks.yaml
    - name: RELEASE_NAME
      value: {{ $root.Release.Name | quote }}
    - name: PROMETHEUS_ENABLED
      value: {{ $root.Values.enablePrometheusStack | quote}}
    - name: SEND_ADDITIONAL_TELEMETRY
      value: {{ default $root.Values.runner.sendAdditionalTelemetry $config.sendAdditionalTelemetry | quote }}
    - name: LOG_LEVEL
      value: {{ default $root.Values.runner.log_level $config.log_level }}
    - name: INSTALLATION_NAMESPACE
      valueFrom:
        fieldRef:
          fieldPath: metadata.namespace
    {{- if $root.Values.disableCloudRouting }}
    - name: CLOUD_ROUTING
      value: "False"
    {{- end }}
    {{- if not $root.Values.monitorHelmReleases }}
    - name: DISABLE_HELM_MONITORING
      value: "True"
    {{- end }}
    {{- if $root.Values.scaleAlertsProcessing }}
    - name: ALERTS_WORKERS_POOL
      value: "True"
    {{- end }}
    - name: RUNNER_VERSION
      value: {{ $root.Chart.AppVersion }}
    - name: KRR_IMAGE_OVERRIDE
      value: {{ default $root.Values.runner.krr_image_override $config.krr_image_override }}
    - name: IMAGE_REGISTRY
      value: {{ default $root.Values.runner.image_registry $config.image_registry }}
    - name: WEBSOCKET_RELAY_ADDRESS
      value: {{ default $root.Values.runner.relay_address $config.relay_address }}
    - name: PROFILE_IMAGE_TAG_OVERRIDE
      value: {{ default $root.Values.runner.profiler_image_override $config.profiler_image_override }}
    - name: KUBEPUG_IMAGE_OVERRIDE
      value: {{ default $root.Values.runner.kubepug_image_override $config.kubepug_image_override }}
    - name: NOVA_IMAGE_OVERRIDE
      value: {{ default $root.Values.runner.nova_image_override $config.nova_image_override }}
    - name: RUNBOOK_SIDECAR_IMAGE
      value: {{ default $root.Values.runner.image_registry $config.image_registry }}/nudgebee_runbook_sidecar_agent:{{ default $root.Values.runner.runbook_sidecar_image_tag $config.runbook_sidecar_image_tag }}
    {{- if default $root.Values.runner.victoria_metrics_enabled $config.victoria_metrics_enabled }}
    - name: VICTORIA_METRICS_CONFIGURED
      value: "True"
    {{- end }}
    {{- if and (default $root.Values.runner.clickhouse_enabled $config.clickhouse_enabled) (default $root.Values.runner.clickhouse_secret $config.clickhouse_secret) }}
    - name: CLICKHOUSE_PASSWORD
      valueFrom:
        secretKeyRef:
          name: {{ $root.Release.Name }}-clickhouse
          key: admin-password
    {{- end }}
    {{- if kindIs "string" $root.Values.runner.additional_env_vars }}
    {{- fail "The `additional_env_vars` string value is deprecated. Change the `additional_env_vars` value to an array" -}}
    {{- end }}
    {{- if and (hasKey $config "additional_env_vars") (kindIs "string" $config.additional_env_vars) }}
    {{- fail "The `additional_env_vars` string value is deprecated. Change the `additional_env_vars` value to an array" -}}
    {{- end }}
    {{- if $isDualMode }}
    - name: RUNNER_DUAL_MODE_ENABLED
      value: "true"
    - name: RUNNER_MODE
      value: "{{ $runnerMode }}"
    {{- end }}
    {{- if eq $containerName "apiserver" }}
    - name: RUNNER_BACKGROUND_SERVER_URL
      value: "http://{{ $root.Release.Name }}-runner:80"
    {{- end }}
    {{- if $root.Values.runner.additional_env_vars }}
    {{ toYaml $root.Values.runner.additional_env_vars | nindent 4 }}
    {{- end }}
    {{- if and (hasKey $config "additional_env_vars") $config.additional_env_vars }}
    {{ toYaml $config.additional_env_vars | nindent 4 }}
    {{- end }}
  envFrom:
  - secretRef:
      name: {{ $root.Release.Name }}-runner-secret
      optional: true
  {{- if $root.Values.runner.additional_env_froms }}
  {{ toYaml $root.Values.runner.additional_env_froms | nindent 2 }}
  {{- end }}
  {{- if and (hasKey $config "additional_env_froms") $config.additional_env_froms }}
  {{ toYaml $config.additional_env_froms | nindent 2 }}
  {{- end }}
  volumeMounts:
    - name: auth-config-secret
      mountPath: /etc/robusta/auth
    - name: playbooks-config-secret
      mountPath: /etc/robusta/config
    {{- if $root.Values.playbooksPersistentVolume }}
    - name: persistent-playbooks-storage
      mountPath: /etc/robusta/playbooks/storage
    {{- end }}
    {{- with $root.Values.runner.extraVolumeMounts }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
    {{- if and (hasKey $config "extraVolumeMounts") $config.extraVolumeMounts }}
    {{- with $config.extraVolumeMounts }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
    {{- end }}
  lifecycle:
    preStop:
      exec:
        command: ["bash", "-c", "kill -SIGINT 1"]
  resources:
    requests:
      cpu: {{ $config.resources.requests.cpu }}
      memory: {{ if $root.Values.isSmallCluster }}"512Mi"{{ else }}{{ $config.resources.requests.memory | quote }}{{ end }}
    limits:
      {{ if $config.resources.limits.memory }}memory: {{ if $root.Values.isSmallCluster }}"512Mi"{{ else if $config.resources.limits.memory }}{{ $config.resources.limits.memory | quote }}{{ else }}{{ $config.resources.requests.memory | quote }}{{ end }}
      {{ end }}
      {{ if $config.resources.limits.cpu }}cpu: {{ $config.resources.limits.cpu | quote }}{{ end }}
{{- end }}

{{/*
Common runner volumes template
*/}}
{{- define "nudgebee.runner.volumes" -}}
volumes:
  - name: playbooks-config-secret
    secret:
      secretName: {{ .Release.Name }}-playbooks-config-secret
      optional: true
  - name: auth-config-secret
    secret:
      secretName: {{ .Release.Name }}-auth-config-secret
      optional: true
  {{- if .Values.playbooksPersistentVolume }}
  - name: persistent-playbooks-storage
    persistentVolumeClaim:
      claimName: persistent-playbooks-pv-claim
  {{- end }}
  {{- with .Values.runner.extraVolumes }}
  {{- toYaml . | nindent 2 }}
  {{- end }}
{{- end }}

{{/*
Common runner imagePullSecrets template
Usage: include "nudgebee.runner.imagePullSecrets" (dict "root" . "config" .Values.apiServer)
*/}}
{{- define "nudgebee.runner.imagePullSecrets" -}}
{{- $root := .root }}
{{- $config := .config }}
{{- if or $root.Values.runner.imagePullSecrets $config.imagePullSecrets }}
imagePullSecrets:
{{- if $config.imagePullSecrets }}
{{- toYaml $config.imagePullSecrets | nindent 0 }}
{{- else }}
{{- toYaml $root.Values.runner.imagePullSecrets | nindent 0 }}
{{- end }}
{{- end }}
{{- end }}