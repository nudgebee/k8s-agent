# nudgebee-agent

The Nudgebee Kubernetes agent — a small Go binary that runs in a customer's K8s cluster and exposes a primitive surface (kube reads, observability proxies, mutations, scanners, discovery) over a WebSocket connection to the Nudgebee backend.

**All composition / playbook / enricher logic lives on the backend** — this agent is a thin in-cluster gateway.

> This is the `runner` component of the [NudgeBee Kubernetes agent](../README.md). It is built into the `ghcr.io/nudgebee/nudgebee-agent` image and deployed by the Helm chart in [`charts/nudgebee-agent`](../charts/nudgebee-agent). The other deployed components (`node-agent`, `kubewatch`) live in their own repositories.

Licensed under Apache-2.0.

## What it does

Three concurrent subsystems run in one process:

1. **WS dispatcher** — connects to the Nudgebee relay over WebSocket, receives signed action requests, runs them under a bounded worker pool, returns responses. Actions cover observability proxies, kube reads/mutations, pod-exec, scanners, service-map, and discovery (see [docs/actions.md](docs/actions.md)).
2. **Discovery** — `client-go` shared informers watch core K8s resources and POST snapshots + deltas to `/v1/k8s/discovery/{type}` on the backend.
3. **HTTP server** — receives AlertManager webhooks at `/api/alerts` and forwards them to the backend, exposes Prometheus metrics at `/metrics`, plus `/healthz`.

## Quick start

### Build
```bash
make build              # → bin/nudgebee-agent (stripped, version-stamped)
make docker-build       # → nudgebee-agent:<git-version> image
```

### Test
```bash
make test               # unit tests, ~10s, no Docker needed
make test-coverage      # unit + coverage report (coverage.html)
make setup-envtest      # one-time: download kube-apiserver/etcd binaries
make test-integration   # unit + real K8s + real Prometheus + real Loki (Docker required, ~1 min)
```

### Run locally
```bash
export WEBSOCKET_RELAY_ADDRESS=ws://relay.nudgebee.local:8080/register
export NUDGEBEE_AUTH_SECRET_KEY=<your-tenant-secret>
export NUDGEBEE_ENDPOINT=https://api.nudgebee.com
export PROMETHEUS_URL=http://prometheus.monitoring.svc:9090
./bin/nudgebee-agent
```

The agent connects to the relay, reads its kubeconfig (or in-cluster credentials), and starts answering action requests. K8s subsystems (discovery, kube reads, pod-exec) are default-on; if no kubeconfig is present, the agent logs a warning and serves only observability proxies. See [docs/configuration.md](docs/configuration.md) for the full env-var surface.

## Architecture at a glance

```
┌───────────────────────────────────────────────────────────────┐
│                  Customer Kubernetes cluster                  │
│                                                               │
│   AlertManager ──webhook──┐                                   │
│   K8s events watcher ─────┤                                   │
│                           │                                   │
│                  ┌────────▼────────┐                          │
│                  │  nudgebee-agent │                          │
│                  │                 │                          │
│                  │ ┌─────────────┐ │      kube/prom/loki      │
│                  │ │ Dispatcher  │ │◄─────query primitives    │
│                  │ │  (worker    │ │                          │
│                  │ │   pools)    │ │                          │
│                  │ └──────┬──────┘ │      Trivy/Popeye Jobs   │
│                  │        │        │──────►create+watch       │
│                  │ ┌──────▼──────┐ │                          │
│                  │ │ WS client   │ │                          │
│                  │ │ (auto-recon)│ │                          │
│                  │ └──────┬──────┘ │                          │
│                  └────────┼────────┘                          │
│                           │                                   │
└───────────────────────────┼───────────────────────────────────┘
                            │ WSS + Basic-Auth
                            │ HMAC-signed action requests
                            ▼
                  ┌──────────────────┐
                  │  Nudgebee relay  │
                  └─────────┬────────┘
                            │
                            ▼
                  ┌──────────────────┐
                  │ Nudgebee backend │
                  │ (composition,    │
                  │  playbooks,      │
                  │  enrichers)      │
                  └──────────────────┘
```

See [docs/architecture.md](docs/architecture.md) for the full design.

## Action surface

Actions across 8 groups:

| Group | Examples | Count |
|---|---|---|
| Observability proxies | `prometheus_query`, `query_loki`, `query_es`, `signoz_query_range`, `jaeger_query_traces`, `chronosphere_query_traces`, `gke_logs`, `http_proxy_request` | 27 |
| Kube reads | `get_resource`, `get_resource_yaml`, `list_resource_names`, `kubectl_command_executor` | 4 |
| Pod-exec | `pod_bash_enricher`, `pod_script_run_enricher` | 2 |
| Mutations | `delete_pod`, `drain`, `cordon`, `rollout_restart`, alert-rule CRUD, silences CRUD | 17 |
| Scanners | `image_scanner` (Trivy), `popeye_scan`, `krr_scan`, `kube_bench_scan`, `certificate_scanner` | 6 |
| Service map | `service_map`, `service_map_enricher`, `traces_dependency_map` | 3 |
| Control plane | `ping`, `echo`, `health`, `refresh_playbook` | 4 |
| Discovery | Pod, Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, CronJob, Node, Namespace, HelmRelease | 10 resource types |

Plus background subsystems: AlertManager forwarder, OpenCost publisher, task poller, Prometheus `/metrics`.

[Full action list with parameters →](docs/actions.md)

## Authentication

Three modes:

- **HMAC signature** — relay HMACs the action body with the tenant's signing key
- **RSA partial-keys** — required for mutations and pod-exec; agent loads its private key from `RSA_PRIVATE_KEY_PATH`
- **Light-action allowlist** — read-only actions (Prometheus query, get_resource, etc.) skip signature checks; relay is the perimeter

The allowlist is hot-swappable via `refresh_playbook` so new read-only actions can roll out without a customer Helm upgrade.

## Repo layout

```
cmd/agent/          — main entry point + wiring
pkg/relay/          — WS client (dial, greeting, reconnect)
pkg/auth/           — HMAC + RSA-OAEP partial-keys + light-action allowlist
pkg/canonjson/      — byte-deterministic JSON for HMAC parity with Python
pkg/dispatch/       — action registry + worker pools + 180s deadline
pkg/discovery/      — client-go informers + sink + converters
pkg/observability/  — Prometheus / Loki / ES / Signoz / Jaeger / Chronosphere / GCP / http proxy
pkg/kube/           — get_resource + kubectl_command_executor
pkg/podexec/        — kubectl exec via SPDY
pkg/mutate/         — delete / drain / cordon / silence CRUD / alert-rule CRUD
pkg/scanners/       — Trivy / Popeye / KRR / kube-bench Job orchestration
pkg/servicemap/     — eBPF-metric → service-topology builder
pkg/alerts/         — AlertManager webhook forwarder
pkg/control/        — refresh_playbook (allowlist hot-reload)
pkg/metrics/        — Prometheus /metrics endpoint
pkg/config/         — env-driven config
internal/k8sclient/ — in-cluster + kubeconfig fallback
```

## License

Apache-2.0 (see [LICENSE](../LICENSE)).
