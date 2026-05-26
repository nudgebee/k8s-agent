# Development

## Prerequisites

- Go 1.26+ (matches `go.mod`)
- `make`
- For integration tests:
  - Docker daemon (testcontainers spins up Prometheus + Loki containers)
  - `setup-envtest` for K8s envtest binaries (`make setup-envtest` installs it)

## Build & test

```bash
make build              # builds bin/nudgebee-agent
make fmt                # gofmt -s -w
make lint               # go vet (+ golangci-lint if installed)
make test               # unit tests, no Docker, ~10s
make test-coverage      # unit + coverage report → coverage.html
make validate           # fmt + lint + test
```

### Integration tests

```bash
make setup-envtest      # one-time: installs setup-envtest + downloads K8s 1.32 binaries
make test-integration   # unit + 6 integration suites against real infra (~1 min)
```

The integration suites:
- `pkg/discovery` — envtest with real kube-apiserver+etcd; creates Pods, asserts informer events
- `pkg/kube` — envtest; round-trips ConfigMaps via dynamic client
- `pkg/podexec` — envtest; SPDY exec dial against real apiserver (URL build verified)
- `pkg/scanners` — envtest; Job spec accepted, lifecycle observed via fake status updates
- `pkg/observability/prometheus` — testcontainers `prom/prometheus:v3.0.0`; all 6 endpoints
- `pkg/observability/loki` — testcontainers `grafana/loki:2.9.4`; query, query_range, labels

## Running locally

The agent expects a relay endpoint to connect to. Two options:

**Option 1 — fake relay**: any WebSocket echo server. The agent will dial, send the greeting, and idle waiting for inbound action requests. SIGTERM exits cleanly.

```bash
WEBSOCKET_RELAY_ADDRESS=ws://localhost:8080/register \
NUDGEBEE_AUTH_SECRET_KEY=test-secret \
HTTP_LISTEN_ADDR=127.0.0.1:5000 \
make run
```

**Option 2 — local Nudgebee stack**: if you have access to the backend, bring up the relay-server + collector locally and point the agent at it.

For testing without any backend, just hit `/healthz`:
```bash
curl http://localhost:5000/healthz   # → ok
curl http://localhost:5000/metrics   # Prometheus exposition
```

## Adding a new action

1. Decide the package: read primitive → `pkg/observability/<vendor>` or `pkg/kube`; mutation → `pkg/mutate`; scanner → `pkg/scanners`; new transport pattern → its own package.

2. Implement the handler. The contract is:
   ```go
   type Handler func(ctx context.Context, params map[string]any) (any, error)
   ```
   Return whatever JSON-marshalable shape the backend expects. Errors become 500 in the response envelope; `context.DeadlineExceeded` becomes 504.

3. Register it via the package's `Handlers(...)` function. Wire that into [`cmd/agent/main.go`](../cmd/agent/main.go) under the matching subsystem toggle.

4. Decide auth class:
   - Read-only / safe → add to `lightActions` map in main; allowlist syncs via `refresh_playbook`
   - Mutating / pod-exec / scanner → leave OUT of `lightActions`; the validator enforces HMAC or RSA partial-keys

5. Tests:
   - **Unit**: `httptest.Server` for HTTP-backed actions; `k8s.io/client-go/kubernetes/fake` for K8s-backed actions
   - **Integration** (optional): `//go:build integration` tag; envtest for K8s, testcontainers-go for upstream services

6. Update `docs/actions.md` with the new entry.

## Adding a new discovery resource type

1. Implement a converter in `pkg/discovery/converters.go`:
   ```go
   func convertX(obj any) (any, bool) {
       x, ok := obj.(*<typeFromClientGo>)
       if !ok { return nil, false }
       return map[string]any{...}, true
   }
   ```
   The output map should match the wire shape the collector's discovery handler expects.

2. Add a `RegisterX()` method in `pkg/discovery/service.go` that wires the right informer + workqueue with the converter.

3. Add to `RegisterAll()` so the default-on path picks it up.

4. Add a test in `pkg/discovery/converters_test.go` (synthetic input → asserted output map).


## Repo layout

| Path | Purpose |
|---|---|
| `cmd/agent/main.go` | Wiring + signal handling; everything plugs in here |
| `pkg/relay/` | WS client (dial, greeting, reconnect, concurrent-safe writes) |
| `pkg/auth/` | HMAC + RSA-OAEP partial-keys + atomic light-action allowlist |
| `pkg/canonjson/` | Byte-deterministic JSON for HMAC parity with Python |
| `pkg/dispatch/` | Action registry, normal+high-priority pools, deadline |
| `pkg/discovery/` | Informers, workqueue, sink, per-resource converters, Helm release detection |
| `pkg/observability/<vendor>/` | One package per upstream (Prometheus, Loki, ES, Signoz, Jaeger, Chronosphere, GCP, http proxy) |
| `pkg/kube/` | Dynamic-client reads + kubectl shell-out (read-verb allowlist) |
| `pkg/podexec/` | SPDY exec wrapper |
| `pkg/mutate/` | K8s mutations + AlertManager silences + Prometheus + Loki rule CRUD |
| `pkg/scanners/` | Trivy / Popeye / KRR / kube-bench / cert Job orchestration |
| `pkg/servicemap/` | Coroot eBPF metric → topology builder |
| `pkg/alerts/` | AlertManager webhook receiver → backend forwarder |
| `pkg/control/` | refresh_playbook (hot-reload allowlist) |
| `pkg/metrics/` | Prometheus `/metrics` registry + dispatch hooks |
| `pkg/config/` | Env-driven config |
| `internal/k8sclient/` | InClusterConfig + kubeconfig fallback |
| `docs/` | This documentation |

## Code conventions

- **No comments stating WHAT** — well-named functions and types do that. Comments explain WHY (a non-obvious constraint, a workaround, a Python parity quirk).
- **Errors propagate**: don't swallow. Wrap with `fmt.Errorf("...: %w", err)` so callers can `errors.Is`/`errors.As`.
- **Context-aware**: every blocking call takes `context.Context`. Honour cancellation in loops; don't sleep without a select.
- **No global state** beyond `pkg/version` (linker-stamped) and `pkg/dispatch.actions` (process-scoped registry built at startup).
- **Tests live next to code**, build tags separate `_integration_test.go` files from unit tests.

## Releasing

1. Tag: `git tag vX.Y.Z`
2. Push: `git push --tags`
3. Build: `make build && make docker-build` (the `git describe` output ends up in the binary's `/healthz` and `/metrics`).
4. CI builds and pushes multi-arch images to `ghcr.io/nudgebee/nudgebee-agent` on every push to `main`.
5. Bump the chart at `nudgebee/k8s-agent` (`runner.image.tag`).
