# NudgeBee Kubernetes Agent

[![License: Apache 2.0](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Helm Lint](https://github.com/nudgebee/k8s-agent/actions/workflows/helm-dev-lint.yml/badge.svg)](https://github.com/nudgebee/k8s-agent/actions/workflows/helm-dev-lint.yml)
[![Helm Test](https://github.com/nudgebee/k8s-agent/actions/workflows/helm-prod-test.yml/badge.svg)](https://github.com/nudgebee/k8s-agent/actions/workflows/helm-prod-test.yml)
[![Release](https://img.shields.io/github/v/release/nudgebee/k8s-agent)](https://github.com/nudgebee/k8s-agent/releases)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/nudgebee/k8s-agent/badge)](https://securityscorecards.dev/viewer/?uri=github.com/nudgebee/k8s-agent)

Helm chart for the NudgeBee Kubernetes agent. The agent runs in your cluster, collects Kubernetes state, events, metrics, logs, and traces, and forwards them to the NudgeBee backend for observability, cost visibility, and incident automation.

## What it deploys

| Component | Purpose |
| --- | --- |
| `runner` | Connects to the NudgeBee backend over WebSocket; executes diagnostic and remediation actions in-cluster (source: [`runner/`](runner/)) |
| `kubewatch` (forwarder) | Streams Kubernetes resource changes and events to the runner |
| `node-agent` (DaemonSet) | Per-node logs, profiles, and traces collector (eBPF-based) |
| `opencost` (subchart) | Kubernetes cost allocation metrics |
| `opentelemetry-collector` (subchart) | Receives OTLP signals from `node-agent` and exports to ClickHouse |
| `clickhouse` (subchart) | Local store for traces / logs / metrics (7-day TTL by default) |
| Prometheus rules / ServiceMonitors | Default alerting + scrape config for `kube-prometheus-stack` users |

The runner connects out to `wss://relay.nudgebee.com/register` and `https://collector.nudgebee.com`. No inbound connectivity is required.

## Prerequisites

- Kubernetes 1.24+
- Helm 3.12+
- (Optional but recommended) [`kube-prometheus-stack`](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) — the chart ships `ServiceMonitor` and `PrometheusRule` resources by default
- A NudgeBee account and auth key — sign up at <https://nudgebee.com>

## Install

```bash
helm repo add nudgebee-agent https://nudgebee.github.io/k8s-agent/
helm repo update

helm upgrade --install nudgebee-agent nudgebee-agent/nudgebee-agent \
  --namespace nudgebee-agent --create-namespace \
  --set runner.nudgebee.auth_secret_key="<your-auth-key>"
```

Or use the opinionated installer (auto-installs `kube-prometheus-stack` and wires up Prometheus discovery):

```bash
curl -sSL https://raw.githubusercontent.com/nudgebee/k8s-agent/main/installation.sh \
  | bash -s -- -a "<your-auth-key>"
```

### Verifying chart signatures

Chart packages are signed with [cosign](https://github.com/sigstore/cosign)
keyless signing. Each GitHub release attaches `<chart>.tgz.sig` and
`<chart>.tgz.pem` alongside the chart tarball. To verify a downloaded
package:

```bash
VERSION=0.1.1
BASE="https://github.com/nudgebee/k8s-agent/releases/download/nudgebee-agent-${VERSION}"
curl -sSLO "${BASE}/nudgebee-agent-${VERSION}.tgz"
curl -sSLO "${BASE}/nudgebee-agent-${VERSION}.tgz.sig"
curl -sSLO "${BASE}/nudgebee-agent-${VERSION}.tgz.pem"

cosign verify-blob \
  --certificate "nudgebee-agent-${VERSION}.tgz.pem" \
  --signature "nudgebee-agent-${VERSION}.tgz.sig" \
  --certificate-identity-regexp "^https://github\.com/nudgebee/k8s-agent/\.github/workflows/release(-rc)?\.yml@.*" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "nudgebee-agent-${VERSION}.tgz"
```

## Configuration

All configurable values live in [`charts/nudgebee-agent/values.yaml`](charts/nudgebee-agent/values.yaml). Common overrides:

```yaml
runner:
  # Required
  nudgebee:
    auth_secret_key: "<your-auth-key>"

  # Optional integrations — set the URL to enable
  loki:
    url: ""
  es:
    url: ""
    apiKey: ""
  signoz:
    url: ""
    apiKey: ""
  grafana:
    url: ""
    username: ""
    password: ""

  # Grant write permissions (drain nodes, scale workloads, manage services etc.)
  # Off by default — only enable if you want NudgeBee to perform remediations.
  enableWritePermissions: false

# Subcharts can be disabled if not needed
opencost:
  enabled: true
opentelemetry-collector:
  enabled: true
clickhouse:
  enabled: true
```

Full configuration reference: [installation guide](https://app.nudgebee.com/help/docs/installation/agent/installation/).

## Uninstall

```bash
helm uninstall nudgebee-agent --namespace nudgebee-agent
kubectl delete namespace nudgebee-agent
```

Note: ClickHouse PVCs are not deleted automatically — remove them manually if you no longer need the data.

## Data sent to NudgeBee

By default, the agent forwards:

- Kubernetes object state (deployments, pods, services, etc.) and events
- Cluster and node metrics (via Prometheus scrape)
- OpenCost allocation data
- Logs, traces, and profiles from `node-agent` (configurable; sensitive HTTP headers are redacted via the `SENSITIVE_HEADERS` env var)

Secrets are explicitly **not** watched by the kubewatch forwarder (`kubewatch.config.resource.secret: false`).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). For security issues, see [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
