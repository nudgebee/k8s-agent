# Architecture

This document describes the agent's runtime model, the wire protocol it speaks to the relay, and the auth scheme. For a quick visual, see the diagram in [the README](../README.md#architecture-at-a-glance).

## Design principle

The agent does only what physically requires in-cluster execution:

1. **K8s API calls** that need the in-cluster ServiceAccount token.
2. **Network reads** of services (Prometheus, Loki, Elasticsearch, AlertManager, OpenCost) that are only reachable from inside the cluster.
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
  "signature": "v0=<hex hmac>",          // OR
  "partial_auth_a": "<base64 RSA>",      // partial-keys mode
  "partial_auth_b": "<base64 RSA>",
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

Three modes; the dispatcher picks the first present per-request:

### 1. HMAC signature
Used for most authenticated read paths. Relay HMACs the request body (after [byte-deterministic JSON canonicalisation](#canonical-json)) with the tenant's signing key:

```
v0=<hex(HMAC-SHA256(signing_key, "v0:" + canonical_json(body)))>
```

The agent recomputes and constant-time-compares. Any drift = reject.

### 2. RSA partial-keys
Used for mutating actions (Group D — drain/cordon/delete_*) and pod-exec. Two halves of the signing key are RSA-OAEP-MGF1-SHA256 encrypted (one by the relay, one by another trusted party); the agent decrypts both with its private key, XORs them, and the result must equal the signing key UUID's int form. Each half also carries `v0=sha256(canonical_json(body))` to prevent body tampering.

The agent loads its private key at startup from `RSA_PRIVATE_KEY_PATH` (PEM, PKCS#1 or PKCS#8).

### 3. Light-action allowlist
Read-only actions (Prometheus query, get_resource, etc.) skip signature checks; the relay is the perimeter. The allowlist is hot-swappable via `refresh_playbook` — see [refresh_playbook](#refresh_playbook).

### Canonical JSON

HMAC parity with the relay (a Python service) is brittle. The relay uses `body.json(exclude_none=True, sort_keys=True, separators=(",",":"))` (and a separate variant with default separators for the signature path). [pkg/canonjson](../pkg/canonjson/canonjson.go) reproduces both byte-for-byte:
- Sorted object keys (Unicode code-point order)
- Drop nil entries (`exclude_none`)
- ASCII-escape non-ASCII as `\uXXXX` (Python's `ensure_ascii=True` default; Go's encoder does NOT do this)
- HTML chars (`<`, `>`, `&`) NOT escaped (Python doesn't; Go's encoder DOES by default — explicitly disabled here)
- Whole-number floats keep `.0` suffix (`1.0` → `"1.0"`, not `"1"`)

The package has 31 fixture tests for cross-language parity. Any drift = every authenticated request rejected, so this is critical.

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
