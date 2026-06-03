# Scalability Audit — nudgebee-agent at 1,000 nodes / 100,000 workloads

**Scope:** runner (Go agent) + kubewatch forwarder + helm chart, focused on
whether the agent survives a 1,000-node / ~100k-workload cluster, with
attention to **event frequency** and **update (resync) frequency**.

**Verdict:** The agent will **not** survive a cluster this size without
changes. There are **two day-one blockers** that bite simply by the agent
existing in the cluster — the full-snapshot fan-in (§1) and informer-cache
memory with no memory limit (§2) — plus a set of throughput/burst issues
(§3–§8) that degrade the agent under churn and incidents. Below is the
detailed breakdown, ordered by severity, each with the exact code path,
what breaks, the failure mode at scale, and the fix. Severities were
re-verified against the rendered chart manifests and the actual code paths;
§3 was downgraded from blocker to High after confirming the event-path
amplification is gated by matcher predicates + rate limits (not per-event).

---

## 0. Topology recap (where the load lands)

```
kube-apiserver
   │  (1) LIST/WATCH all pods+workloads      (2) WATCH all Events
   ▼                                              ▼
runner informers (pkg/discovery)            kubewatch (separate Deployment)
   │  full snapshot + incremental POSTs           │  POST /api/handle per event
   ▼                                              ▼
   └──────────────► backend /v1/k8s/discovery   runner /api/handle (pkg/alerts)
                                                    │  matcher → 3× Events.LIST
                                                    ▼
                                                 backend /v1/k8s/events
```

Two independent fan-ins both terminate at the single runner pod
(`replicas: 1`, no HPA, **no memory limit set** —
`charts/nudgebee-agent/values.yaml` runner.resources.limits.memory is `~`).

---

## 1. BLOCKER — Full snapshot is a single in-memory, single-POST fan-in

**Code:** `pkg/discovery/service.go:307` (`emitAllSnapshots`) +
`pkg/discovery/sink.go:99` (`Post`).

`emitAllSnapshots` walks every informer's indexer, converts **every object**,
and accumulates them into one `byType[Type][]any` slice. It then emits a
**single** `Envelope` per type with `TotalBatches: 1, IsLastBatch: true`
(service.go:319-331). `Sink.Post` then does `json.Marshal(env)` of the whole
slice, gzips the whole buffer in memory, and does one HTTP POST with a 60s
client timeout.

For this cluster the `"service"` bucket alone is Pods + Deployments +
StatefulSets + DaemonSets + ReplicaSets — call it 100k–300k items. Each
converted dict is ~1–2 KB of JSON.

**What breaks:**
- **Memory spike:** the full `[]any` + `json.Marshal` output + gzip buffer are
  three simultaneous copies of a 150–500 MB+ payload, on top of the informer
  cache. Easily a multi-hundred-MB transient allocation **per snapshot**, and
  it recurs every `DISCOVERY_RESYNC` (default **30 min**, service.go:56 /
  config.go default).
- **Timeout:** a single 150 MB+ gzip POST will frequently exceed the 60s
  client timeout (`sink.go:93`). On timeout the whole snapshot is lost and
  nothing retries until the next 30-min resync — the backend's world view
  goes stale for up to 30 minutes at a time.
- The `BatchID/BatchSequence/TotalBatches/IsFirstBatch/IsLastBatch` fields
  already exist on the envelope (sink.go:61-71) — the batching protocol is
  designed but **never used**; the snapshot hardcodes batch 1-of-1.

**Fix:** chunk `emitAllSnapshots` into N-item batches (e.g. 500–1,000 items),
populate the existing batch fields, stream each batch as its own POST.
Bonus: marshal incrementally (encoder to a `io.Pipe`/gzip writer) instead of
buffering the whole slice. This is the single highest-value change.

---

## 2. BLOCKER — Informer cache memory, unbounded, single replica, no limit

**Code:** `pkg/discovery/service.go:63` (`NewSharedInformerFactory`) +
`RegisterAll` (service.go:211) which registers Pods, Deployments,
StatefulSets, DaemonSets, **ReplicaSets**, Jobs, CronJobs, Nodes, Namespaces.

client-go caches the **full** object for every watched resource. At 100k
workloads you are realistically caching 100k–300k Pods plus all ReplicaSet
revisions (ReplicaSets accumulate per Deployment rollout — often 10×
Deployment count). Decoded Go objects are far larger than the wire form:
budget **1–2 GB** resident just for the pod+RS caches.

**What breaks:**
- Runner requests `1000Mi` and has **no memory limit** (chart). It will blow
  well past its request; on a pressured node the kubelet evicts it, it
  restarts, re-lists the whole cluster (a thundering LIST against the
  apiserver), and re-emits the giant snapshot — a crash-loop amplifier.
- `replicas: 1`, no HPA → no horizontal relief; this is a vertical-only
  scaling story today.

**Fixes (combine):**
- Trim what's cached. Use `informers.WithTransform` to strip
  `managedFields`, large annotations, and unused status before the object
  enters the cache — typically 30–60% memory off pods.
- Reconsider watching **all ReplicaSets**. The converter already drops
  `replicas==0` RS at emit time (converters.go:112) but the informer still
  **caches** every historical RS. Either field-select/scope them or rely on
  the Pod→RS→Deployment lookup without a full RS cache.
- Set a memory **limit** and request sized for the target cluster, document a
  sizing table (nodes/workloads → memory), and add a readiness probe so a
  restarting runner doesn't get traffic mid-resync.
- Long term: shard discovery (by namespace ranges) across replicas, or move
  to metadata-only informers (`metadatainformer`) for resources where only
  identity/owner is needed.

---

## 3. HIGH — Each matched finding does 3 etcd-backed Event LISTs (burst-sensitive)

**Code:** `pkg/triggers/engine.go:146-148` →
`fetchSubjectEvents` + `fetchNodeEvents` + `fetchNamespaceEvents`, each calling
`recentEventsTable` → `k8sEventsLister.ListEvents`
(`cmd/agent/k8s_events_lister.go:42`) which does a **live**
`CoreV1().Events(ns).List(...)`.

**Important gating (this is NOT per-event).** kubewatch forwards resource
UPDATEs to `/api/handle`; the engine runs them through matchers gated by
Kind + operation + predicate + rate limit (engine.go:103-128). The builtins
fire only on genuinely-bad state — `pod_crash_loop`/`pod_oom_killed`
(rate-limit **1h**), `image_pull_backoff` (**10m**), `job_failure`,
`node_not_ready`, and `babysitter_*` which fires **only on an actual spec
diff** (predicates.go:462), rate-limited **30s**. So ordinary healthy pod
churn — the bulk of update volume — matches nothing and triggers **zero**
LISTs. The 3 LISTs happen **per matched, non-rate-limited finding**, not per
event.

**Why it's still a real cost:** each of the 3 calls is a typed
`clientset` LIST with no `resourceVersion` and a `Limit` set
(lister.go:56-59), so it's served from **etcd, not the watch cache**. The
subject/node calls use **field selectors on Events**, which aren't indexed —
the apiserver scans the namespace's events to satisfy them. (The
namespace-wide call is *not* a huge page — it's capped at `Limit: 20` /
`supplementaryEventsLimit*2`; I corrected an earlier overstatement here.)
Three etcd reads-with-scan per finding.

**What breaks at scale:**
- The danger is **incident / mass-rollout bursts**: hundreds of *distinct*
  pods going bad at once = hundreds of distinct fingerprints = rate limiting
  doesn't dedup them → a burst of findings, each doing 3 etcd LISTs, against
  an apiserver that's already hot. Can trip API priority-and-fairness
  throttling for the runner's SA exactly when triage is happening.
- `handleK8sEvent` does `w.WriteHeader(202)` and then spawns an **unbounded
  goroutine per matched event** for forwarding (alerts/server.go:197) — no
  worker pool, no backpressure. A burst → goroutine and connection pileup.
- Steady-state (no incident) cost is modest — this is a tail/burst risk, not a
  constant load, which is why it's High and not a day-one blocker like §1/§2.

**Fixes:**
- Serve recent-events evidence from an **Events informer cache** (RV=0 list
  semantics) instead of etcd field-selector LISTs — turns 3 etcd reads per
  finding into in-memory lookups.
- Bound the forward fan-out with a worker pool + queue; shed load (and meter
  it) instead of spawning goroutines without limit.
- Consider gating the supplementary (node + namespace) event tables behind a
  per-cluster budget so an incident burst can't multiply apiserver load.

---

## 4. HIGH — Incremental updates: one synchronous POST per object event

**Code:** `pkg/discovery/service.go:342` (`workerLoop`) → `processOne` (353) →
`Sink.Post`.

There is exactly **one worker goroutine per resource type** (Run spawns one
`workerLoop` per handler, service.go:270-276), and each processes the
workqueue **one key at a time**, doing a full HTTP POST of a **1-item**
envelope per event (service.go:374).

**What breaks:**
- Pod churn dominates in a 1,000-node cluster (status updates, restarts,
  readiness flaps, scale events). One worker doing ~20–50ms synchronous POSTs
  caps at roughly 20–50 events/sec. Sustained churn above that → the pod
  workqueue grows unbounded and the backend's incremental view lags
  arbitrarily.
- Each event is its own TLS round-trip + headers for a tiny body → terrible
  payload-to-overhead ratio. Bodies under 16 KB aren't even gzipped
  (sink.go:114), which is correct, but the per-request overhead is the
  problem, not compression.

**Mitigations that already help:** the workqueue keys by
`MetaNamespaceKeyFunc` (service.go:386), so rapid repeated updates to the same
object **coalesce** into one processing — good. The rate-limiter LRU and
resync grace window (engine.go) also damp the *event* path.

**Fixes:**
- **Batch + debounce** incremental emits: drain up to N keys (or a short time
  window, e.g. 1–2s) and POST them as one multi-item envelope.
- Run a small **pool of workers** per type (or a shared pool) instead of one.
- Reuse a tuned `http.Transport` (raise `MaxIdleConnsPerHost`) so the sink
  isn't bottlenecked on 2 idle conns under burst.

---

## 5. HIGH — `podLookup` is O(workloads × pods) per snapshot

**Code:** `pkg/discovery/service.go:131` (`podLookup`) called from the
Deployment/StatefulSet/DaemonSet/ReplicaSet converters
(`pkg/discovery/converters.go:336` in `serviceDict`).

For each workload converted, `podLookup` does
`indexer.ByIndex(NamespaceIndex, ns)` then a **linear scan** of every pod in
the namespace doing a label-selector match until it finds one running pod
(service.go:140-158).

**What breaks:** in `emitAllSnapshots` (and on every workload event), this is
O(workloads_in_ns × pods_in_ns) per namespace. A namespace with 1,000 pods
and 200 workloads = 200k selector evaluations; summed across the cluster this
is tens of millions of operations on **every 30-min snapshot** and on the
initial sync — a CPU spike that stacks on top of the marshal spike from §1.

**Fix:** build a pod-by-owner/selector index once per snapshot (or maintain an
owner→representative-pod map updated by the pod handler) instead of a fresh
linear scan per workload. The lookup only needs `qos_class/ip/conditions`
from one representative pod — that can be precomputed.

---

## 6. MEDIUM — Relay handler spawns a goroutine per inbound message

**Code:** `pkg/relay/client.go:132` — `go c.handler(ctx, msg, send)` for every
WS read.

The dispatcher's semaphore (`pkg/dispatch/dispatch.go:269`, default pool 10 /
high 3) bounds concurrent *handler execution*, but goroutines blocked on
`pool.Acquire` accumulate without limit. A flood of action requests → growing
blocked-goroutine set holding request bytes in memory. The 503 "agent
overloaded" path only triggers on ctx cancel, not on pool saturation.

**Fix:** bound inbound concurrency at the read loop (semaphore or fixed reader
worker pool) and fast-fail with 503 when saturated, rather than queuing
unboundedly.

---

## 7. MEDIUM — Periodic spikes are synchronized and coarse

Defaults (config.go): `DISCOVERY_RESYNC = 30m`, `ALERT_RULES_INTERVAL = 30m`,
`CLUSTER_STATUS_PERIOD_SEC = 60s`, `TASK_RUNNER_WINDOW = 120s`.

- The 30-min full resync (§1) and the alert-rules collect both fire on coarse
  timers; at this scale the resync is a periodic memory/CPU/network cliff, not
  a smooth background cost. None of these intervals are exposed as chart
  values (the chart agent found no `DISCOVERY_RESYNC` etc. in `values.yaml`),
  so operators can't tune them without manual env injection.
- Telemetry tick (60s) runs several synchronous probes incl.
  `queryNodeAgentCount` PromQL and provider detection; fine in isolation but
  worth keeping off the critical path.

**Fix:** expose resync/intervals as chart values; once §1 batches the
snapshot, consider lengthening resync (the incremental path keeps the backend
fresh) to make the full reconcile cheaper/rarer. **Note:** deletions are
currently only reconciled at resync — incremental deletes are logged and
**dropped** (service.go:360-367). Lengthening resync worsens deletion lag
unless tombstones are emitted on the incremental path.

---

## 8. MEDIUM — Deletions never propagate incrementally

**Code:** `pkg/discovery/service.go:360-368`. On a delete, `processOne` finds
the key absent in the indexer, logs "will reconcile on next snapshot", and
`Forget`s it. So a deleted pod/workload stays "live" in the backend for up to
`DISCOVERY_RESYNC` (30 min). In a high-churn 1,000-node cluster (pods deleted
constantly), the backend's active-resource set is chronically inflated between
snapshots — and the full snapshot is exactly the thing §1 says may time out.

**Fix:** emit a tombstone/`deleted:true` incremental envelope on delete (the
converters already carry a `deleted` field). Capture the object via
`DeletedFinalStateUnknown` handling in the delete handler so the key/identity
is available.

---

## 9. LOW / positives worth keeping

- Shared informer factory dedups LIST/WATCH across handlers (service.go:62).
- Workqueue key-coalescing damps duplicate updates (service.go:386).
- Rate-limiter LRU is bounded at 10k entries (~1 MB) and won't grow unbounded
  under label cardinality (ratelimit.go:38-48).
- Resync grace window suppresses re-fires on every rollout (engine.go:168).
- gzip above 16 KB on the discovery sink (sink.go:114).
- `cluster_snapshot` kubewatch payloads are dropped, avoiding double full-sync
  (alerts/server.go:156).

---

## Priority summary

| # | Severity | Issue | Primary fix |
|---|----------|-------|-------------|
| 1 | Blocker | Full snapshot = one in-mem, one-POST fan-in | Batch via existing envelope fields + streaming marshal |
| 2 | Blocker | Informer cache memory, 1 replica, no mem limit | Transform/trim cache, scope ReplicaSets, set limits + sizing, readiness probe |
| 3 | High | 3 etcd-backed Event LISTs per matched finding (burst-sensitive, gated by predicate + rate limit) | Serve events from informer cache; bound forward pool; budget supplementary tables |
| 4 | High | 1 synchronous POST per object event, single worker | Batch+debounce emits, worker pool, tuned transport |
| 5 | High | `podLookup` O(workloads×pods) per snapshot | Precompute owner→pod index |
| 6 | Medium | Unbounded goroutine per WS message | Bound reader concurrency, fast-fail 503 |
| 7 | Medium | Coarse synchronized 30m spikes, not tunable | Expose intervals as chart values |
| 8 | Medium | Incremental deletes dropped (30m lag) | Emit tombstones on delete |

### Suggested sequencing
1. Ship #1 (snapshot batching) and #2 (cache trim + memory limit) together —
   these are what make the agent *start* surviving the cluster.
2. Then #3 + #4 (event/update throughput) for steady-state load.
3. Then #5–#8 as hardening.
