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
    # Script pods and scan Jobs run as their own read-only accounts, not as
    # the runner (templates/script-scanner-service-accounts.yaml).
    - name: SCANNER_SERVICE_ACCOUNT
      value: {{ include "nudgebee-agent.fullname" . }}-scanner
    - name: SCRIPT_SERVICE_ACCOUNT
      value: {{ include "nudgebee-agent.fullname" . }}-script
    {{- if .Values.runner.splitIdentities }}
    # Runner identities (templates/runner-identities.yaml).
    - name: RUNNER_IDENTITY_MODE
      value: split
    - name: READER_SERVICE_ACCOUNT
      value: {{ include "nudgebee-agent.fullname" . }}-runner-reader
    {{- if include "nudgebee-agent.execIdentityEnabled" . }}
    - name: EXEC_SERVICE_ACCOUNT
      value: {{ include "nudgebee-agent.fullname" . }}-runner-exec
    {{- end }}
    {{- if include "nudgebee-agent.writerIdentityEnabled" . }}
    - name: WRITER_SERVICE_ACCOUNT
      value: {{ include "nudgebee-agent.fullname" . }}-runner-writer
    {{- end }}
    {{- end }}
    {{- if .Values.runner.scriptPods.allowWrite }}
    - name: SCRIPT_WRITE_SERVICE_ACCOUNT
      value: {{ include "nudgebee-agent.fullname" . }}-script-write
    {{- end }}
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
    {{- if and (not .Values.kubewatch.enabled) (not .Values.runner.k8sWatch.enabled) }}
    {{- fail "\n\nkubewatch.enabled=false with runner.k8sWatch.enabled=false leaves nothing watching the cluster, so no Kubernetes change alerts are raised. Set runner.k8sWatch.enabled=true (the runner watches the cluster itself) or leave kubewatch.enabled=true." }}
    {{- end }}
    {{- with .Values.runner.k8sWatch }}
    {{- if .enabled }}
    - name: K8S_WATCH_ENABLED
      value: "true"
    {{- with $.Values.kubewatch.config }}
    {{- if .namespace }}
    - name: K8S_WATCH_NAMESPACE
      value: {{ .namespace | quote }}
    {{- end }}
    {{- end }}
    - name: K8S_WATCH_KINDS
      value: {{ include "nudgebee-agent.k8sWatchKinds" $ | quote }}
    {{- if .workers }}
    - name: K8S_WATCH_WORKERS
      value: {{ .workers | quote }}
    {{- end }}
    {{- if .maxPending }}
    - name: K8S_WATCH_MAX_PENDING
      value: {{ .maxPending | quote }}
    {{- end }}
    {{- with .customResources }}
    - name: K8S_WATCH_CUSTOM_RESOURCES
      value: {{ include "nudgebee-agent.k8sWatchCustomResources" $ | quote }}
    {{- end }}
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
    # every other mutate action needs a valid relay signature. So enabling
    # the subsystem here does NOT loosen the security posture; it only makes
    # the light-action carve-outs (currently create_or_replace_alert_rule /
    # delete_alert_rule, which also need a signature when the relay key is
    # set) reachable end-to-end.
    #
    # Operators who want a strictly read-only deployment can set
    # `runner.mutateEnabled: false`. The `eq ... false` pattern is
    # intentional: `default true` would treat an explicit `false` as
    # unset and re-enable the subsystem.
    - name: MUTATE_ENABLED
      value: {{ if eq .Values.runner.mutateEnabled false }}"false"{{ else }}"true"{{ end }}
    {{- if .Values.runner.podScriptRunRequireSignature }}
    - name: POD_SCRIPT_RUN_REQUIRE_SIGNATURE
      value: "true"
    {{- end }}
    # Writes through light actions need a valid relay signature when the relay
    # key below is set. `eq ... false` so an explicit false is honoured.
    - name: WRITE_ACTIONS_REQUIRE_SIGNATURE
      value: {{ if eq .Values.runner.writeActionsRequireSignature false }}"false"{{ else }}"true"{{ end }}
    # KUBECTL_ALLOW_WRITE lifts the runner's read-only verb allowlist on
    # kubectl_command_executor. Gated by runner.enableWritePermissions — the
    # SAME switch that grants the service account its write RBAC — so the
    # runner allowlist and the cluster RBAC move together. Default (false)
    # keeps kubectl strictly read-only; mutating verbs (scale/patch/delete/...)
    # are rejected and routed to the signed pkg/mutate actions instead. When
    # true, mutating kubectl commands need a valid relay signature if the
    # relay key below is set (read commands never do); without the key they
    # run unsigned — the operator granting write RBAC opts into that posture.
    - name: KUBECTL_ALLOW_WRITE
      value: {{ if .Values.runner.enableWritePermissions }}"true"{{ else }}"false"{{ end }}
    {{- if .Values.runner.nudgebee.relay_signing_public_key }}
    # Relay Ed25519 public key — the agent verifies relay-signed requests with
    # it. With it set, mutations and the write light actions need a valid relay
    # signature (runner.writeActionsRequireSignature); reads always fall back
    # to the light-action path.
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
    {{- with include "nudgebee-agent.timeseriesCluster" . }}
    {{- if not (has "TIMESERIES_CLUSTER" $envVarNames) }}
    - name: TIMESERIES_CLUSTER
      value: {{ . | quote }}
    {{- end }}
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
  {{- with .Values.runner.extraVolumeMounts }}
  volumeMounts:
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
{{- with .Values.runner.extraVolumes }}
volumes:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end }}

{{/*
Runner imagePullSecrets template. Invoked with root context.
*/}}
{{- define "nudgebee.runner.imagePullSecrets" -}}
{{- include "nudgebee-agent.imagePullSecrets" (dict "secrets" .Values.runner.imagePullSecrets "root" .) }}
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
imagePullSecrets for a pod spec or ServiceAccount: the component's own list
followed by global.imagePullSecrets, as the Bitnami charts combined them.
Called with (dict "secrets" <component list> "root" $). Entries are a secret
name or a {name: ...} map. Empty entries and duplicates are dropped; renders
nothing when no name is left.
*/}}
{{- define "nudgebee-agent.imagePullSecrets" -}}
{{- $names := list }}
{{- $global := (default (dict) .root.Values.global).imagePullSecrets }}
{{- range concat (default list .secrets) (default list $global) }}
{{- if kindIs "map" . }}{{ with .name }}{{ $names = append $names . }}{{ end }}{{ else if . }}{{ $names = append $names . }}{{ end }}
{{- end }}
{{- with $names | uniq -}}
imagePullSecrets:
{{- range . }}
  - name: {{ . | quote }}
{{- end }}
{{- end }}
{{- end }}

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
<release>-<name>, or the release name alone when it already contains <name>, as
the Bitnami subchart's common.names.fullname did. Takes (list <root> <name>).
*/}}
{{- define "nudgebee-agent.clickhouse.releaseName" -}}
{{- $release := (index . 0).Release.Name -}}
{{- $name := index . 1 -}}
{{- if contains $name $release -}}
{{- $release | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" $release $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
The bundled ClickHouse's default name, from the release name alone. The
collector's values use it: they render inside the subchart, which cannot see
.Values.clickhouse and so cannot call clickhouse.servicename below.
*/}}
{{- define "nudgebee-agent.clickhouse.defaultName" -}}
{{- include "nudgebee-agent.clickhouse.releaseName" (list . "clickhouse") -}}
{{- end -}}

{{/*
ClickHouse fullname (Services, Secret, ConfigMaps, StatefulSet prefix). Same
rules as the Bitnami subchart's common.names.fullname, so the names don't change.
*/}}
{{- define "nudgebee-agent.clickhouse.servicename" -}}
{{- if .Values.clickhouse.fullnameOverride -}}
{{- .Values.clickhouse.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- include "nudgebee-agent.clickhouse.releaseName" (list . (default "clickhouse" .Values.clickhouse.nameOverride)) -}}
{{- end -}}
{{- end -}}

{{/*
Fails the render when ClickHouse's name, Secret or Secret key is changed but the
collector still has the settings for the defaults: it could not start, while the
install or upgrade reported success. Settings of the operator's own are left
alone.
*/}}
{{- define "nudgebee-agent.clickhouse.validateCollector" -}}
{{- $collector := index .Values "opentelemetry-collector" -}}
{{- $default := include "nudgebee-agent.clickhouse.defaultName" . -}}
{{- $service := include "nudgebee-agent.clickhouse.servicename" . -}}
{{- $secret := include "nudgebee-agent.clickhouse.secretName" . -}}
{{- $key := include "nudgebee-agent.clickhouse.secretKey" . -}}
{{- $marker := "nudgebee-agent.clickhouse.defaultName" -}}
{{- if and (ne $service $default) (contains $marker (toString (dig "config" "exporters" "clickhouse" "endpoint" "" $collector))) -}}
{{- fail (printf "ClickHouse is named %q, but the collector still connects to %q: set opentelemetry-collector.config.exporters.clickhouse.endpoint" $service $default) -}}
{{- end -}}
{{- range $collector.extraEnvs -}}
{{- if eq .name "CLICKHOUSE_PASSWORD" -}}
{{- $name := toString (dig "valueFrom" "secretKeyRef" "name" "" .) -}}
{{- $refKey := toString (dig "valueFrom" "secretKeyRef" "key" "" .) -}}
{{- $isDefault := contains $marker $name -}}
{{- if and $isDefault (ne $secret $default) -}}
{{- fail (printf "The ClickHouse password is in Secret %q, but the collector still reads %q: set the CLICKHOUSE_PASSWORD entry of opentelemetry-collector.extraEnvs to Secret %q, key %q" $secret $default $secret $key) -}}
{{- end -}}
{{- if and (or $isDefault (eq $name $secret)) (ne $refKey $key) -}}
{{- fail (printf "The ClickHouse password is under key %q of Secret %q, but the collector reads key %q: set the key of the CLICKHOUSE_PASSWORD entry of opentelemetry-collector.extraEnvs" $key $secret $refKey) -}}
{{- end -}}
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
collectorQuoted for a value that may itself hold a {{ }} reference, which is
filled in first. Takes (list <collector chart context> <value>). Separate from
collectorQuoted, whose callers a --reuse-values upgrade keeps.
*/}}
{{- define "nudgebee-agent.collectorValue" -}}
{{- include "nudgebee-agent.collectorQuoted" (tpl (toString (index . 1)) (index . 0)) -}}
{{- end -}}

{{/*
The cluster label the collector puts on the node-agent metrics it writes to
ClickHouse's TimeSeries table, and the runner's TIMESERIES_CLUSTER, while
clickhouse.timeseries is on: clickhouse.timeseries.clusterName, else the
kube-system namespace's UID (unique per cluster, never changes). Empty when
neither is known: lookup returns nothing under helm template (ArgoCD) and
client-side dry runs. The runner's CLUSTER_NAME is left alone, as it also reports
it to the backend.
*/}}
{{- define "nudgebee-agent.timeseriesCluster" -}}
{{- if and (include "nudgebee-agent.clickhouse.installed" .) (dig "timeseries" "enabled" false .Values.clickhouse) -}}
{{- $name := dig "timeseries" "clusterName" "" .Values.clickhouse -}}
{{- if not $name -}}
{{- $name = dig "metadata" "uid" "" (lookup "v1" "Namespace" "" "kube-system") -}}
{{- end -}}
{{- $name -}}
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
kubewatch is deployed only while the runner does not watch the cluster itself:
with runner.k8sWatch.enabled the runner ignores kubewatch's posts anyway.
*/}}
{{- define "nudgebee-agent.kubewatchDeployed" -}}
{{- if and .Values.kubewatch.enabled (not .Values.runner.k8sWatch.enabled) }}true{{ end -}}
{{- end }}

{{/*
The kinds the runner's own watch covers, from kubewatch.config.resource, so
switching from kubewatch keeps the same kinds. Kinds missing from the map count
as on, as in the chart defaults.
*/}}
{{- define "nudgebee-agent.k8sWatchKinds" -}}
{{- $r := default dict .Values.kubewatch.config.resource -}}
{{- $kinds := list -}}
{{- range $key, $kind := dict "pod" "pod" "job" "job" "node" "node" "deployment" "deployment" "daemonset" "daemonset" "statefulset" "statefulset" "services" "service" "configmap" "configmap" "ingress" "ingress" "rollout" "rollout" -}}
{{- if (ne (toString (index $r $key)) "false") -}}
{{- $kinds = append $kinds $kind -}}
{{- end -}}
{{- end -}}
{{- join "," $kinds -}}
{{- end }}

{{/*
Read-only (get/list/watch) cluster rules shared by the read-only runner
ClusterRole (runner-service-account-readonly.yaml) and the
script-scanner-read ClusterRole that script and scan pods run under
(script-scanner-service-accounts.yaml).
No Secrets, no write verbs. Emitted as list items indented two spaces.
*/}}
{{- define "nudgebee-agent.readOnlyClusterRules" }}
  - apiGroups:
      - ""
    resources:
      - configmaps
      - daemonsets
      - deployments
      - events
      - namespaces
      - persistentvolumes
      - persistentvolumeclaims
      - pods
      - pods/status
      - pods/log
      - replicasets
      - replicationcontrollers
      - services
      - serviceaccounts
      - endpoints
      - nodes
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - "apiregistration.k8s.io"
    resources:
      - apiservices
    verbs:
      - get
      - list

  - apiGroups:
      - "rbac.authorization.k8s.io"
    resources:
      - clusterroles
      - clusterrolebindings
      - roles
      - rolebindings
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - "autoscaling"
    resources:
      - horizontalpodautoscalers
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - apps
    resources:
      - daemonsets
      - deployments
      - deployments/scale
      - replicasets
      - replicasets/scale
      - statefulsets
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - extensions
    resources:
      - daemonsets
      - deployments
      - deployments/scale
      - ingresses
      - replicasets
      - replicasets/scale
      - replicationcontrollers/scale
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - batch
    resources:
      - cronjobs
      - jobs
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - "events.k8s.io"
    resources:
      - events
    verbs:
      - get
      - list

  - apiGroups:
      - networking.k8s.io
    resources:
      - ingresses
      - networkpolicies
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - "policy"
    resources:
      - poddisruptionbudgets
      - podsecuritypolicies
    verbs:
      - get
      - list

  - apiGroups:
      - "scheduling.k8s.io"
    resources:
      - priorityclasses
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - "storage.k8s.io"
    resources:
      - storageclasses
      - volumeattachments
      - csidrivers
      - csinodes
      - csistoragecapacities
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - "coordination.k8s.io"
    resources:
      - leases
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - "discovery.k8s.io"
    resources:
      - endpointslices
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - "node.k8s.io"
    resources:
      - runtimeclasses
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - "admissionregistration.k8s.io"
    resources:
      - mutatingwebhookconfigurations
      - validatingwebhookconfigurations
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - "flowcontrol.apiserver.k8s.io"
    resources:
      - flowschemas
      - prioritylevelconfigurations
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - cert-manager.io
    resources:
      - certificates
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - "monitoring.coreos.com"
    resources:
      - prometheusrules
    verbs:
      - get
      - list

  # ruleSelector discovery: the runner reads the Prometheus CR's
  # spec.ruleSelector.matchLabels so UI-created PrometheusRules carry the
  # labels prometheus-operator requires before it will load them. Read-only.
  - apiGroups:
      - "monitoring.coreos.com"
    resources:
      - prometheuses
    verbs:
      - get
      - list

  - apiGroups: ["metrics.k8s.io"]
    resources:
      - pods
      - nodes
    verbs: ["get", "list"]

  # Argo Rollouts read permissions are granted unconditionally. An RBAC rule
  # for an absent CRD is inert and becomes active the moment Argo Rollouts is
  # installed (see runner-service-account.yaml for the full rationale).
  - apiGroups:
      - argoproj.io
    resources:
      - rollouts
    verbs:
      - get
      - list
      - watch

  # Karpenter read permissions are granted unconditionally. An RBAC rule for
  # an absent CRD is inert and becomes active the moment karpenter is
  # installed (see runner-service-account.yaml for the full rationale).
  - apiGroups:
      - karpenter.sh
    resources:
      - nodepools
      - nodepools/status
      - nodeclaims
      - nodeclaims/status
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - karpenter.k8s.aws
    resources:
      - ec2nodeclasses
    verbs:
      - get
      - list
      - watch

  - apiGroups:
      - karpenter.azure.com
    resources:
      - aksnodeclasses
    verbs:
      - get
      - list
      - watch

{{- if .Capabilities.APIVersions.Has "snapshot.storage.k8s.io/v1/VolumeSnapshot" }}
  - apiGroups:
      - snapshot.storage.k8s.io
    resources:
      - volumesnapshots
    verbs:
      - get
      - list
      - watch
{{- end }}
{{- end }}

{{/*
OpenShift additions to the read-only rules: `use` on the agent SCCs and
monitoring reads. Renders nothing unless openshift.enabled.
*/}}
{{- define "nudgebee-agent.openshiftReadOnlyClusterRules" }}
{{- if .Values.openshift.enabled }}
  - apiGroups:
    - security.openshift.io
    resources:
    - securitycontextconstraints
    verbs:
    - use
    resourceNames:
    - {{ if .Values.openshift.createScc }}"{{ include "nudgebee-agent.fullname" . }}-scc"{{ else }}{{ .Values.openshift.sccName | quote }}{{ end }}
{{- if .Values.openshift.createPrivilegedScc }}
    - {{ include "nudgebee-agent.fullname" . }}-scc-privileged
{{- end }}
{{- if .Values.openshift.privilegedSccName }}
    - {{ .Values.openshift.privilegedSccName }}
{{- end }}
  - apiGroups:
    - monitoring.coreos.com
    resources:
    - servicemonitors
    - prometheusrules
    - alertmanagers
    - silences
    - podmonitors
    verbs:
    - get
    - list
    - watch
{{- end }}
{{- end }}

{{/*
Exec and writer identities exist only when split identities are on, the
switch is on, and the runner is not read-only. Each returns "true" or "".
*/}}
{{- define "nudgebee-agent.execIdentityEnabled" -}}
{{- if and .Values.runner.splitIdentities .Values.runner.exec.enabled (not .Values.runner.readOnly) }}true{{ end -}}
{{- end }}
{{- define "nudgebee-agent.writerIdentityEnabled" -}}
{{- if and .Values.runner.splitIdentities .Values.runner.changes.enabled (not .Values.runner.readOnly) }}true{{ end -}}
{{- end }}

{{/*
Read-only projection of runner.customClusterRoleRules for the reader: verbs
reduced to get/list/watch (`*` becomes those three), Secrets, connect
subresources (exec, attach, portforward, proxy of any resource) and any
resource containing `*` removed, `*`-resource rules dropped,
nonResourceURLs rules kept with `get`. Renders a YAML list or nothing.
*/}}
{{- define "nudgebee-agent.readerCustomRules" -}}
{{- $readVerbs := list "get" "list" "watch" -}}
{{- $blocked := list "secrets" "pods/exec" "pods/attach" "pods/portforward" "pods/proxy" "services/proxy" "nodes/proxy" -}}
{{- $out := list -}}
{{- range .Values.runner.customClusterRoleRules -}}
{{- $ruleVerbs := .verbs | default list -}}
{{- $verbs := list -}}
{{- if has "*" $ruleVerbs -}}
{{- $verbs = $readVerbs -}}
{{- else -}}
{{- range $ruleVerbs -}}{{- if has . $readVerbs -}}{{- $verbs = append $verbs . -}}{{- end -}}{{- end -}}
{{- end -}}
{{- if .nonResourceURLs -}}
{{- if has "get" $verbs -}}{{- $out = append $out (dict "nonResourceURLs" .nonResourceURLs "verbs" (list "get")) -}}{{- end -}}
{{- else if not (has "*" (.resources | default list)) -}}
{{- $res := list -}}
{{- range (.resources | default list) -}}
{{- $sub := splitList "/" . | rest | join "/" -}}
{{- if not (or (has . $blocked) (contains "*" .) (has $sub (list "exec" "attach" "portforward" "proxy")) (hasPrefix "secrets" (lower .))) -}}{{- $res = append $res . -}}{{- end -}}
{{- end -}}
{{- if and $res $verbs -}}
{{- $r := dict "apiGroups" (.apiGroups | default (list "")) "resources" $res "verbs" $verbs -}}
{{- if .resourceNames -}}{{- $_ := set $r "resourceNames" .resourceNames -}}{{- end -}}
{{- $out = append $out $r -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if $out }}{{ toYaml $out }}{{ end -}}
{{- end }}

{{/*
runner.k8sWatch.customResources, validated, as the JSON the runner reads from
K8S_WATCH_CUSTOM_RESOURCES. Refuses what runner.customResourceRead refuses
(the core group, "*", secrets); the runner itself refuses kinds that can hold
credentials, from one list in pkg/kube/secretpolicy.go.
*/}}
{{- define "nudgebee-agent.k8sWatchCustomResources" -}}
{{- $out := list -}}
{{- range $i, $e := .Values.runner.k8sWatch.customResources -}}
{{- if not (kindIs "map" $e) -}}
{{- fail (printf "runner.k8sWatch.customResources[%d] must be a map with group and resource" $i) -}}
{{- end -}}
{{- /* RBAC matches group and resource names exactly, and both are lower-case. */ -}}
{{- $group := lower (toString (default "" $e.group)) -}}
{{- $resource := lower (toString (default "" $e.resource)) -}}
{{- if or (eq $group "") (eq $group "*") -}}
{{- fail (printf "runner.k8sWatch.customResources[%d]: group %q is not allowed; list custom resources only" $i $group) -}}
{{- end -}}
{{- if or (eq $resource "") (eq $resource "*") (eq (lower $resource) "secrets") -}}
{{- fail (printf "runner.k8sWatch.customResources[%d]: resource %q is not allowed" $i $resource) -}}
{{- end -}}
{{- $entry := dict "group" $group "resource" $resource -}}
{{- with $e.version }}{{- $_ := set $entry "version" (toString .) -}}{{- end -}}
{{- with $e.diffPaths }}
{{- if not (kindIs "slice" .) -}}
{{- fail (printf "runner.k8sWatch.customResources[%d].diffPaths must be a list" $i) -}}
{{- end -}}
{{- $_ := set $entry "diffPaths" . -}}
{{- end -}}
{{- $out = append $out $entry -}}
{{- end -}}
{{- toJson $out -}}
{{- end }}
