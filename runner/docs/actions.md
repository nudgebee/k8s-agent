# Action reference

The agent exposes a set of actions over the relay WS. Each action takes a JSON `action_params` map (forwarded as-is from the backend) and returns JSON `data` in the response envelope.

## Auth class

| Symbol | Meaning |
|---|---|
| 🔓 light | Light-action allowlist; no signature required (relay is the perimeter) |
| ✍️ HMAC | Requires HMAC signature OR RSA partial-keys |
| 🔐 RSA | Mutating action; RSA partial-keys strongly recommended |

## Group A — Observability proxies

Thin HTTP forwarders. The agent does NOT parse responses — it returns raw JSON to the caller, who composes.

### Prometheus  🔓
Configured via `PROMETHEUS_URL` + optional `PROMETHEUS_HEADERS`.

| Action | Endpoint | Required params | Optional |
|---|---|---|---|
| `prometheus_query` | `/api/v1/query` | `query` | `time`, `timeout` |
| `prometheus_query_range` | `/api/v1/query_range` | `query`, `start`, `end`, `step` | `timeout` |
| `prometheus_labels` | `/api/v1/labels` | — | `start`, `end`, `match[]` |
| `prometheus_label_values` | `/api/v1/label/{name}/values` | `label` | `start`, `end`, `match[]` |
| `prometheus_series` | `/api/v1/series` | `match[]` | `start`, `end` |
| `prometheus_alerts` | `/api/v1/alerts` | — | — |

### Loki  🔓
Configured via `LOKI_URL` + optional `LOKI_EXTRA_HEADER`.

| Action | Endpoint | Required params | Optional |
|---|---|---|---|
| `loki_query` | `/loki/api/v1/query` | `query` | `time`, `limit` |
| `loki_query_range` | `/loki/api/v1/query_range` | `query` | `start`, `end`, `step`, `direction`, `limit` |
| `loki_labels` | `/loki/api/v1/labels` | — | `start`, `end`, `query` |
| `loki_label_values` | `/loki/api/v1/label/{name}/values` | `label` | `start`, `end`, `query` |
| `loki_series` | `/loki/api/v1/series` | `match[]` | `start`, `end` |

### Elasticsearch  🔓
Configured via `ELASTICSEARCH_URL` + auth (one of `ELASTICSEARCH_USERNAME`+`ELASTICSEARCH_PASSWORD` or `ELASTICSEARCH_APIKEY`).

| Action | Description | Params |
|---|---|---|
| `query_es` | DSL or PPL query | `index`, `query_type`=`dsl`\|`ppl`, `query` |
| `query_es_indices` | List of indices via `_cat/indices` | — |
| `query_es_index_field` | Mapping for one index | `index` |
| `query_es_field_index_values` | Distinct values for one field (terms agg) | `index`, `field_name`, `limit` |

### Signoz  🔓
`SIGNOZ_URL` + `SIGNOZ_API_KEY`. All forward `action_params` as the request body.

| Action | Endpoint |
|---|---|
| `signoz_query_range` | `/api/v3/query_range` |
| `signoz_label_suggest` | `/api/v3/autocomplete/attribute_keys` |
| `signoz_value_suggest` | `/api/v3/autocomplete/attribute_values` |

### Jaeger  🔓
Configured via `JAEGER_URL`.

| Action | Description |
|---|---|
| `jaeger_query_traces` | Search traces (params → query string) |
| `jaeger_query_services` | List known services |
| `jaeger_query_trace_by_id` | Fetch one trace; param `trace_id` |
| `jaeger_query_operations` | List operations for a service; param `service` |
| `jaeger_query_metrics` | SPM metrics; param `metric_type` (`latencies` / `call_rates` / `error_rates` / `min_step`) |

### Chronosphere  🔓
Configured via `CHRONOSPHERE_URL` + `CHRONOSPHERE_API_KEY`.

| Action | Description |
|---|---|
| `chronosphere_query_traces` | POST to `/api/unstable/data/traces/searches`; body = action_params |

### Pinot  🔓
Configured via `PINOT_URL` (controller port 9000) + optional `PINOT_AUTH_TOKEN` (Bearer) or `PINOT_USERNAME`/`PINOT_PASSWORD` (Basic-Auth).
Query path is `/sql` (controller); set `PINOT_URL` to the broker (`pinot-broker:8099`) and it will use `/query/sql` there — or keep it at the controller.

| Action | Description | Required params | Optional |
|---|---|---|---|
| `pinot_query` | Execute a SQL query; returns raw Pinot `resultTable` JSON | `sql` | — |
| `pinot_tables` | List all tables | — | — |
| `pinot_schema` | Column schema for a table | `table` | — |

**Live table**: `k8s_logs` — columns `namespace`, `pod`, `container`, `log`, `ingest_hour` (HOURS epoch), `stream`, `node`.

### GCP (GKE)  🔓
`GCP_ENABLED=true`. Auth via Workload Identity (in-cluster) or `GOOGLE_APPLICATION_CREDENTIALS`. Default project from `GCP_PROJECT_ID`, overridable per call.

| Action | Description | Params |
|---|---|---|
| `gke_logs` | Cloud Logging entries for a GKE node pool | `project_id`?, `zone`, `limit` |
| `gke_traces` | Arbitrary BigQuery SQL | `project_id`?, `query` |

### HTTP proxy  🔓
Generic named-target HTTP forwarder for Grafana and arbitrary upstream APIs. Targets configured via `HTTP_PROXY_TARGETS` env (`name=url;name=url`).

| Action | Description |
|---|---|
| `http_proxy_request` | Forward to named target. Params: `target`, `method`, `path`, `headers`, `query`, `body` |

Use `target: "*"` only if the operator opts into wildcard mode (ON only when `*=...` is in the targets map). Otherwise, only configured names resolve.

## Group B — Kube reads  🔓

| Action | Description | Params |
|---|---|---|
| `get_resource` | Get one resource (or list of types) via dynamic client | `group`, `version`, `resource_type` (comma-separated allowed), `namespace`?, `name`?, `all_namespaces`? |
| `get_resource_yaml` | Same, returned as YAML | (same) |
| `list_resource_names` | Just names + namespaces | (same as get_resource) |
| `kubectl_command_executor` | Run kubectl with read verbs only | `command` (full kubectl line) |

The read family also accepts the legacy/UI shape `kind` ("Deployment") in place of `group`/`version`/`resource_type` — it's resolved to the canonical GVR for common built-in kinds and the workload CRDs (see `pkg/kube/kind_resolver.go`). An explicit `resource_type` always wins over `kind`; arbitrary/CRD kinds not in the table must pass an explicit GVR.

`kubectl_command_executor` enforces a **read-only verb allowlist**: `get`, `describe`, `logs`, `top`, `explain`, `api-resources`, `api-versions`, `version`, `cluster-info`, `config`, `auth`. Mutating verbs (`apply`, `delete`, `patch`, etc.) are rejected — they go through Group D actions instead, which require RSA partial-keys.

## Group C — Pod-exec  🔐

Both run via SPDY exec (no kubectl binary required). Security: arbitrary shell in customer pods; per-call audit log strongly recommended.

| Action | Description | Params |
|---|---|---|
| `pod_bash_enricher` | Run `bash -c "<command>"` in container | `namespace`, `pod`, `container`?, `command` |
| `pod_script_run_enricher` | Pipe a script body into bash via stdin | `namespace`, `pod`, `container`?, `script`, `interpreter`? (`bash`/`python`/`python3`) |

## Group D — Mutations  🔐

K8s + AlertManager + alert-rule mutations. ALL of these MUST be served behind RSA partial-keys auth in production.

### K8s mutations

| Action | Description | Params |
|---|---|---|
| `delete_pod` | Delete one pod | `namespace`, `name`, `grace_period_seconds`? |
| `delete_job` | Delete a Job (background propagation) | `namespace`, `name` |
| `cordon` | Mark node unschedulable | `node` |
| `uncordon` | Clear unschedulable flag | `node` |
| `rollout_restart` | Trigger rolling restart (annotation patch) | `kind` (Deployment/StatefulSet/DaemonSet), `namespace`, `name` |
| `drain` | Cordon + evict eligible pods + wait for termination | `node`, `ignore_daemonsets`?=true, `delete_emptydir_data`?=false, `force`?=false, `disable_eviction`?=false, `timeout_seconds`?=300, `grace_period_seconds`? |

### AlertManager silences (require `ALERTMANAGER_URL`)

| Action | Description |
|---|---|
| `get_silences` | List active silences; optional `filters: ["alertname=X", ...]` |
| `add_silence` | Create silence; body forwarded to AlertManager |
| `delete_silence` | Cancel by `id` |

### Prometheus alert-rule CRUD (require dynamic client)

PrometheusRule CRDs (`monitoring.coreos.com/v1`).

| Action | Description |
|---|---|
| `create_or_replace_alert_rule` | Apply a PrometheusRule manifest (create or update) |
| `delete_alert_rule` | Delete by `namespace` + `name` |

### Loki rule CRUD (require `LOKI_RULES_URL`)

Loki ruler API (`/loki/api/v1/rules/{namespace}[/{group}]`).

| Action | Description |
|---|---|
| `create_loki_alert_rule` | POST YAML body for a namespace |
| `update_loki_alert_rule` | Same (Loki rules API is upsert) |
| `delete_loki_alert_rule` | Delete by `namespace` + `group` |

## Group E — HTTP proxy

Covered above under [Observability proxies > HTTP proxy](#http-proxy).

## Group F — Scanners  🔐

Each schedules a Kubernetes Job, watches it to completion, returns the raw stdout/stderr to the caller. **The agent does NOT parse scanner output** — the existing collector parsers (`event_handler.handle_image_scan`, etc.) handle that.

Versions are pinned in [pkg/scanners/scanners.go](../pkg/scanners/scanners.go); bump with care.

| Action | Tool |
|---|---|
| `image_scanner` | Trivy image scan |
| `trivy_cis_scan` | Trivy K8s CIS Benchmark |
| `popeye_scan` | Popeye linter |
| `krr_scan` | KRR rightsizing |
| `kube_bench_scan` | kube-bench CIS (HostPID + privileged) |
| `certificate_scanner` | x509 cert scanner |

Image references are pinned in [pkg/scanners/scanners.go](../pkg/scanners/scanners.go).

Per-action params:
- `image_scanner` → `image`
- Others → no required params

Configured via `SCANNERS_ENABLED=true`, `SCANNER_NAMESPACE`, `SCANNER_SERVICE_ACCOUNT`.

## Group G — Service map  🔓

Builds a topology of applications and their connections from in-cluster Prometheus metrics emitted by the [Coroot eBPF node agent](https://github.com/coroot/coroot-node-agent) (shipped as a DaemonSet alongside this agent).

| Action | Description |
|---|---|
| `service_map` | Cluster-wide topology |
| `service_map_enricher` | Same, scoped to a workload (params: `workload_name`, `workload_namespace`) |
| `traces_dependency_map` | Alias used by some backend callers |

Required params (all optional): `r_start_time`, `r_end_time` (RFC3339), `duration` (minutes; default 1440 = 24h), `workload_filter: {workload_name, workload_namespace}` (also accepted nested).

Returns `[]Application` — `Id`, `Category`, `Labels`, `Status`, `Upstreams`, `Downstreams`, `Instances`, `Type`, `OOMKills`, `Restarts`, `CPUThrottlingTime`, `IsHealthy`, `HealthReason`, etc.

> **MVP fidelity note:** the implementation covers the orchestration + query catalog + core graph build; per-protocol classification across upstreams, NaN handling in throttle/restart sums, and application-category classification are simplified.

## Group H — Discovery (background, not WS actions)

Watches K8s resources and POSTs to `<NUDGEBEE_ENDPOINT>/v1/k8s/discovery/{type}`. Configured via `DISCOVERY_ENABLED=true`, `DISCOVERY_RESYNC` (default 30m).

Resource types covered:

- Pod, Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, CronJob, Node, Namespace, HelmRelease (Helm v3 secrets via `owner=helm` label)

ArgoCD Rollouts + OpenShift DeploymentConfig — behind feature flags, not yet wired into `RegisterAll()`.

## Control plane  🔓

| Action | Description |
|---|---|
| `ping` | Returns `{pong: true, ts}` |
| `echo` | Returns the params back |
| `health` | Returns version, build time, Go version, goroutine count |
| `refresh_playbook` | Hot-reload light-action allowlist from `<NUDGEBEE_ENDPOINT>/v1/agent/config`. Returns `{refreshed: bool, action_count, backend_count, static_count}` |

`refresh_playbook` is how new read-only actions roll out without a customer Helm upgrade — backend pushes the new name to its `/v1/agent/config` response, then calls `refresh_playbook` to make the agent pick it up.

## Backend HTTP endpoints (consumed by agent)

These are server-side endpoints the agent calls (not actions the agent serves):

| Endpoint | Method | Used by |
|---|---|---|
| `/v1/k8s/discovery/{type}` | POST | Discovery sink — full snapshots + deltas |
| `/v1/alerts/intake` | POST | AlertManager forwarder |
| `/v1/k8s/runbook/action/output` | POST | Scanner Job output uploader |
| `/v1/agent/config` | GET | refresh_playbook hot-reload source |
| `/v1/k8s/tasks` | GET | Task poller (placeholder; not yet implemented) |
