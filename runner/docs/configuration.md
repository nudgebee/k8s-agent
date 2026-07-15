# Configuration

The agent reads its config from environment variables at startup. Variable names align with the existing `nudgebee/k8s-agent` Helm chart Secret so the chart works without major changes — just bump the `runner.image.tag` to a Go-agent image and add the new subsystem toggles below.

## Required

| Variable | Description |
|---|---|
| `WEBSOCKET_RELAY_ADDRESS` | Relay `/register` URL (e.g. `wss://relay.nudgebee.com/register`) |
| `NUDGEBEE_AUTH_SECRET_KEY` | Tenant auth secret. Sent as Basic-Auth on the WS handshake AND used as the HMAC signing key. |

## Identification

| Variable | Default | Description |
|---|---|---|
| `NUDGEBEE_ENDPOINT` | (none) | Backend HTTP base URL — alerts forward, refresh_playbook, discovery sink, scanner output upload all need this. |
| `ACCOUNT_ID` | (none) | Tenant account id; sent as `X-NB-Account-Id` on backend HTTP calls. |
| `CLUSTER_NAME` | (none) | Cluster name; sent as `X-NB-Cluster` and used in service-map cluster-filter expansion. |

## HTTP server

| Variable | Default | Description |
|---|---|---|
| `HTTP_LISTEN_ADDR` | `:5000` | Address for the agent's HTTP server (`/api/alerts`, `/metrics`, `/healthz`). |

## Subsystem toggles

The K8s subsystems (discovery, kube reads, pod-exec) **default on** — swapping the runner image is enough; no env additions needed. Operators opt out per-subsystem with `<NAME>=false`.

The subsystems that need extra config (mutate, scanners, GCP) **default off**:

| Toggle | Default | Variable | Notes |
|---|---|---|---|
| Discovery | **on** | `DISCOVERY_ENABLED=false` to disable; `DISCOVERY_RESYNC=30m` | client-go informers; needs RBAC to list/watch core + apps + batch resources |
| Kube reads | **on** | `KUBE_ENABLED=false` to disable | `get_resource` etc. — needs broad cluster read RBAC |
| Pod-exec | **on** | `PODEXEC_ENABLED=false` to disable | RSA partial-keys auth strongly recommended for production |
| Mutations | off | `MUTATE_ENABLED=true` | RSA required; configures `dynamic` client for PrometheusRule CRUD |
| Scanners | off | `SCANNERS_ENABLED=true`, `SCANNER_NAMESPACE`, `SCANNER_SERVICE_ACCOUNT` | Needs RBAC to create Jobs in the namespace |
| GCP | off | `GCP_ENABLED=true`, `GCP_PROJECT_ID` | Workload Identity in-cluster |

If a K8s subsystem is enabled but the agent fails to build a K8s client (no kubeconfig, not in-cluster), it logs a warning and disables the K8s subsystems automatically — the agent stays up serving observability proxies.

## Observability targets

| Variable | Required | Description |
|---|---|---|
| `PROMETHEUS_URL` | recommended | Enables `prometheus_*` actions and `service_map` |
| `PROMETHEUS_HEADERS` | optional | Semicolon-separated `Header: value` pairs (e.g. `X-Scope-OrgID: tenant-1`); use for static basic/bearer auth |
| `PROMETHEUS_AUTH` | optional | Static `Authorization` header value (e.g. `Basic dXNlcjpwYXNz` or `Bearer tok`); applied to every request, overriding any `Authorization` in `PROMETHEUS_HEADERS` |
| `PROMETHEUS_URL_QUERY_STRING` | optional | Query-string fragment appended to every Prometheus request (e.g. `extra_label=foo`) |
| `PROMETHEUS_RETENTION_TIME` | optional | Retention value reported to the UI when `/status/flags` is unavailable (e.g. VictoriaMetrics vmsingle) |
| `AWS_ACCESS_KEY` / `AWS_SECRET_ACCESS_KEY` / `AWS_REGION` | optional | Managed Prometheus: sign requests with AWS SigV4. `AWS_SERVICE_NAME` defaults to `aps` |
| `CORALOGIX_PROMETHEUS_TOKEN` | optional | Managed Prometheus: sent as `token` header |
| `AZURE_USE_MANAGED_ID` / `AZURE_CLIENT_SECRET` (+ `AZURE_CLIENT_ID` / `AZURE_TENANT_ID`) | optional | Managed Prometheus: Azure AD Bearer token (managed identity or client-secret). Precedence: AWS → Coralogix → Azure |
| `LOKI_URL` | optional | Enables `loki_*` actions |
| `LOKI_EXTRA_HEADER` | optional | Same format as PROMETHEUS_HEADERS |
| `LOKI_USERNAME` / `LOKI_PASSWORD` | optional | Loki HTTP Basic-Auth |
| `ELASTICSEARCH_URL` | optional | Enables `query_es*` actions |
| `ELASTICSEARCH_USERNAME` / `_PASSWORD` | optional | Basic auth (one of these or `_APIKEY` is needed if ES requires auth) |
| `ELASTICSEARCH_APIKEY` | optional | Alternative to user/pass |
| `ELASTICSEARCH_SSL_VERIFY` | optional | Verify the ES server TLS cert (https only). Default `false` (skip verify), matching the legacy client |
| `SIGNOZ_URL` / `SIGNOZ_API_KEY` | optional | `signoz_*` actions; API key sent as `SIGNOZ-API-KEY` header |
| `SIGNOZ_USER` / `SIGNOZ_PASSWORD` | optional | Alternative auth: JWT minted via `/api/v1/login` (used when `SIGNOZ_API_KEY` is unset) |
| `JAEGER_URL` | optional | `jaeger_*` actions |
| `JAEGER_TOKEN` | optional | Bearer token for the Jaeger query API |
| `CHRONOSPHERE_URL` / `CHRONOSPHERE_API_KEY` | optional | `chronosphere_query_traces`; API key sent as `Authorization: Bearer` |
| `HTTP_PROXY_TARGETS` | optional | `name=url;name=url` for `http_proxy_request` named targets |

## Mutation-specific config

| Variable | Description |
|---|---|
| `ALERTMANAGER_URL` | Enables `get_silences`, `add_silence`, `delete_silence` |
| `ALERTMANAGER_HEADERS` | Comma-separated headers for AlertManager |
| `LOKI_RULES_URL` | Loki ruler component URL — enables `create_loki_alert_rule`, etc. |
| `LOKI_RULES_HEADERS` | Comma-separated headers for Loki rules API |

## Authentication

| Variable | Required | Description |
|---|---|---|
| `RSA_PRIVATE_KEY_PATH` | for mutations | PEM file path (PKCS#1 or PKCS#8). Without this, partial-keys auth is disabled and mutations are unreachable. |

## RBAC

The agent's ServiceAccount needs:

```yaml
# Read primitives + discovery
- apiGroups: [""]
  resources: [pods, services, namespaces, nodes, configmaps, events]
  verbs: [get, list, watch]
- apiGroups: [apps]
  resources: [deployments, statefulsets, daemonsets, replicasets]
  verbs: [get, list, watch]
- apiGroups: [batch]
  resources: [jobs, cronjobs]
  verbs: [get, list, watch]
# Helm release detection (label-selected secrets)
- apiGroups: [""]
  resources: [secrets]
  verbs: [list, watch]
  resourceNames: []  # filtered server-side by labels in informer

# Pod-exec (only if PODEXEC_ENABLED)
- apiGroups: [""]
  resources: [pods/exec]
  verbs: [create]

# Mutations (only if MUTATE_ENABLED)
- apiGroups: [""]
  resources: [pods, nodes]
  verbs: [delete, patch]
- apiGroups: [""]
  resources: [pods/eviction]
  verbs: [create]
- apiGroups: [apps]
  resources: [deployments, statefulsets, daemonsets]
  verbs: [patch]
- apiGroups: [monitoring.coreos.com]
  resources: [prometheusrules]
  verbs: [get, create, update, delete]

# Scanners (only if SCANNERS_ENABLED)
- apiGroups: [batch]
  resources: [jobs]
  verbs: [create, get, list, watch, delete]
- apiGroups: [""]
  resources: [pods, pods/log]
  verbs: [get, list]
```

Scanners that need cluster-wide reads (`popeye_scan`, `krr_scan`, `trivy_cis_scan`) get them via the `SCANNER_SERVICE_ACCOUNT` — distinct from the agent's own SA so the agent can run with narrower permissions.

## Deployment

The agent ships in the `nudgebee/k8s-agent` Helm chart as the `runner` Deployment image (`ghcr.io/nudgebee/nudgebee-agent`). The chart's existing Secret provides every env var the agent needs (NUDGEBEE_ENDPOINT, NUDGEBEE_AUTH_SECRET_KEY, PROMETHEUS_URL, LOKI_URL, ELASTICSEARCH_URL, SIGNOZ_URL, etc.), and discovery / kube reads / pod-exec are default-on, so the agent serves its full surface with zero env additions. The Service name, port, and AlertManager webhook URL stay the same — no AlertManager config change needed.

For mutating actions (drain/cordon/delete + alert-rule CRUD), set `MUTATE_ENABLED=true` and mount an RSA private key at `RSA_PRIVATE_KEY_PATH`. For scanners, set `SCANNERS_ENABLED=true` plus `SCANNER_SERVICE_ACCOUNT`. These are conscious opt-ins because they expand the agent's blast radius.

See [docs/development.md](development.md) for local dev setup.
