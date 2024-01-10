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