# Architecture

This document describes the agent's runtime model, the wire protocol it speaks to the relay, and the auth scheme. For a quick visual, see the diagram in [the README](../README.md#architecture-at-a-glance).

## Design principle

The agent does only what physically requires in-cluster execution:

1. **K8s API calls** that need the in-cluster ServiceAccount token.
2. **Network reads** of services (Prometheus, Loki, Elasticsearch, AlertManager) that are only reachable from inside the cluster.
3. **Privileged operations** like `kubectl exec`, draining nodes, scheduling K8s Jobs.
4. **AlertManager webhook reception** — the URL has to live somewhere AlertManager can reach.

Everything else — playbook composition, trigger matching, finding building, enricher logic — lives on the Nudgebee backend. The agent is intentionally a thin gateway.

The motivating constraint: agent bugs ship slowly (customer Helm upgrade), backend bugs ship in our deploy pipeline. Anything we put on the agent should be stable; anything that's likely to change goes server-side.

## Process model

One binary, one Deployment, one replica per cluster (default). Three concurrent subsystems share the process:

```
                ┌────────────────────────────────────────────┐
                │              cmd/agent (main)              │
                ├────────────────────────────────────────────┤
                │                                            │
                │   ┌──────────────────┐                     │
                │   │  pkg/relay (WS)  │                     │
                │   │   reconnect      │  ──┐                │
                │   │   greeting       │    │                │
                │   └────────┬─────────┘    │                │
                │            │              │                │
                │   ┌────────▼─────────┐    │                │
                │   │  pkg/dispatch    │    │  errgroup      │
                │   │  - auth check    │    │  with shared   │
                │   │  - normal pool   │    │  ctx           │
                │   │  - high-priority │    │                │
                │   │  - 180s deadline │    │                │
                │   └────────┬─────────┘    │                │
                │            │              │                │
                │            ▼              │                │
                │   ┌──────────────────┐    │                │
                │   │ action handlers  │    │                │
                │   └──────────────────┘    │                │
                │                           │                │
                │   ┌──────────────────┐    │                │
                │   │  pkg/discovery   │  ──┤                │
                │   │  - informers     │    │                │
                │   │  - workqueue     │    │                │
                │   │  - sink (POST)   │    │                │
                │   └──────────────────┘    │                │
                │                           │                │
                │   ┌──────────────────┐    │                │
                │   │  HTTP server     │  ──┘                │
                │   │  /api/alerts     │                     │
                │   │  /metrics        │                     │
                │   │  /healthz        │                     │
                │   └──────────────────┘                     │
                │                                            │
                └────────────────────────────────────────────┘
```

Why one process? Singleton tasks (discovery cache, scheduled jobs) and stateless tasks (WS-pulled actions) coexist cleanly on goroutines when the deployment is 1-replica — no need to split into background and foreground pods.

If a customer ever needs HA for WS throughput, scaling to N replicas with discovery + scheduled jobs gated by leader election is a future option. Not implemented in v1.

## Wire protocol

The agent connects to relay's `/register` over WebSocket with HTTP Basic-Auth carrying the tenant's auth secret. After upgrade, the agent sends a greeting:

```json
{
  "action": "auth",
  "version": "<protocol version>",
  "agent_version": "<git tag>",
  "agent_commit": "<git sha>",
  "agent_build_time": "<RFC3339>"
}
```

Inbound action requests (relay → agent):

```json
{
  "body": {
    "action_name": "kubectl_command_executor",
    "timestamp": 1700000000,
    "action_params": { "command": "get pods -A" },
    "account_id": "...",
    "cluster_name": "..."
  },
  "relay_signature": "<base64 Ed25519 over the raw body bytes>",
  "relay_signature_v2": "<base64 Ed25519 over the v2 message, see below>",
  "relay_signed_at": "<RFC 3339>",
  "relay_nonce": "<uuid>",
  "relay_key_id": "<key id>",
  "request_id": "<uuid for sync response>"
}
```

Outbound responses (agent → relay):

```json
{
  "action": "response",
  "request_id": "<echoed from request>",
  "status_code": 200,
  "data": <action-specific JSON>,
  "output_type": "actions"
}
```

`request_id` empty in the inbound request means fire-and-forget — the agent runs the action but does NOT send a response.

## Authentication

Two checks:

- **Relay signature** — the relay signs the raw `body` bytes of every request it forwards (Ed25519, `relay_*` envelope fields). The agent verifies it with `RELAY_SIGNING_PUBLIC_KEY`; a valid signature authorizes any action.
- **Light-action allowlist** — read actions (Prometheus query, get_resource, etc.) run without a valid signature, so reads keep working on an agent without the key or under clock skew.

Some light actions change the cluster: `schedule_k8s_job`, `delete_k8s_job`, `create_or_replace_alert_rule`, `delete_alert_rule`, `continuous_rightsizing` (unless `recommend_only`), `pod_script_run_enricher`, `kubectl_command_executor` write commands (with `KUBECTL_ALLOW_WRITE`) and `http_proxy_request` calls other than GET/HEAD. When the agent has the relay key, these need a valid relay signature (`WRITE_ACTIONS_REQUIRE_SIGNATURE`, on by default). Without the key they run unsigned and the agent logs a warning at startup.

Pod-shell and Grafana / API-proxy / Prometheus messages carry no `body`; the relay puts the exact request it signed in `relay_signed_payload` and the agent runs that. With the key, pod-shell `start`/`exec` and HTTP calls other than GET/HEAD or a query POST (Prometheus query endpoints, Grafana `/api/ds/query`) need a valid signature; `read`/`close` and reads never do.

`relay_signature_v2` signs `nb-relay-sig-v2\n<kind>\n<relay_signed_at>\n<relay_nonce>\n<payload>` (kind `body`, `terminal` or `http`), so a captured message cannot be replayed under a fresh timestamp and nonce. When present it is the one checked. `relay_signature` (body only) is still accepted on action requests from servers that predate v2, so there a captured request with its v2 field stripped can still be replayed until v1 is no longer accepted; pod-shell and HTTP-proxy messages are v2-only.

The allowlist is hot-swappable via `refresh_playbook` so new read-only actions can roll out without a customer Helm upgrade; a refreshed list cannot drop the signature requirement.

## Discovery

`client-go`'s `SharedInformerFactory` watches K8s resources. Per-resource informers feed a `workqueue.RateLimitingInterface` that workers consume — the standard controller-runtime shape.

Two delivery modes:

1. **Full snapshot** — on startup (after `WaitForCacheSync`) and on every periodic resync (default 30 min). One POST per resource type, marked `full_load: true, is_first_batch: true, is_last_batch: true`. The collector treats this as authoritative.
2. **Incremental updates** — between resyncs. ADD/UPDATE/DELETE events from the informer get enqueued and emitted as small payloads with no batch metadata; the collector treats them as deltas (no cleanup).

Resources covered: Pod, Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, CronJob, Node, Namespace, HelmRelease (via `owner=helm` Secret label selector + base64+gzip+json blob decode).

ArgoCD Rollouts and OpenShift `DeploymentConfig` are behind feature flags and not yet wired into `RegisterAll()`.

## Action dispatch

[pkg/dispatch](../pkg/dispatch/dispatch.go) holds:
- An action-name → handler `map`
- Two `golang.org/x/sync/semaphore` instances:
  - **Normal pool** (default 10) — most actions
  - **High-priority pool** (default 3) — actions with `action_params.high_priority = true`
- A 180-second hard deadline per action via `context.WithTimeout`

The pool split (`WEBSOCKET_THREADPOOL_SIZE` + `WEBSOCKET_HIGH_PRIORITY_THREADPOOL_SIZE`) ensures a flood of slow actions can't starve fast probes.

On receipt:
1. Parse envelope, look up action.
2. Run auth validator (rejects 401 if not allowed).
3. Acquire pool slot (rejects 503 on saturation).
4. Run handler under timeout.
5. Map outcome to response: handler error → 500, deadline exceeded → 504, success → 200.
6. If `request_id` is empty, skip the response (fire-and-forget).

## Refresh + hot reload

`refresh_playbook` GETs `<NUDGEBEE_ENDPOINT>/v1/agent/config`, expects `{"light_actions": [...]}`, and atomically swaps the validator's allowlist. Static actions (`ping`, `echo`, `health`, `refresh_playbook`) are always merged in — they can never be locked out.

The validator's `LightActions` is held in an `atomic.Pointer[map[string]struct{}]` so concurrent `Validate` calls during a refresh see either old-or-new but never a torn read. Race-detector-clean.

This means: adding a new read-only action to the agent doesn't require a customer Helm upgrade. Server pushes the new name to the allowlist endpoint, agent picks it up on the next refresh.

## Observability of the agent itself

`/metrics` exposes:
- `nudgebee_agent_actions_total{action,status}` — every action, labelled by name + outcome
- `nudgebee_agent_action_duration_seconds{action}` — histogram
- `nudgebee_agent_alerts_forwarded_total` / `_dropped_total`
- `nudgebee_agent_discovery_posts_total{type,full_load}` / `_errors_total{type}`
- `nudgebee_agent_relay_reconnects_total` / `_relay_connected`
- Standard Go runtime + process metrics (`go_*`, `process_*`)

The chart at `nudgebee/k8s-agent` already deploys a `ServiceMonitor` pointing at the runner Service's port; nothing to change there during cutover.
