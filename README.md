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
| `kubewatch` (forwarder) | Streams Kubernetes resource changes to the runner |
| `node-agent` (DaemonSet) | Per-node logs, profiles, and traces collector (eBPF-based) |
| `opentelemetry-collector` (subchart) | Receives OTLP signals from `node-agent` and exports to ClickHouse |
| `clickhouse` (StatefulSet) | Local store for traces / logs / metrics (7-day TTL by default), on the official ClickHouse image |
| Prometheus rules / ServiceMonitors | Default alerting + scrape config for `kube-prometheus-stack` users |

The runner connects out to `wss://relay.nudgebee.com/register` and `https://collector.nudgebee.com`. No inbound connectivity is required.

## Prerequisites

- Kubernetes 1.24+
- Helm 3.12+
- (Optional but recommended) [`kube-prometheus-stack`](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) — the chart ships `ServiceMonitor` and `PrometheusRule` resources by default
- A StorageClass for the ClickHouse volume — the cluster default is used unless you set one explicitly (see [Storage](#storage))
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

The script refuses to run on a cluster with no default StorageClass — pass `-C <storage-class>` instead, and the name is applied to every PVC it creates: ClickHouse, and the Prometheus (50Gi) and Loki (10Gi) volumes when it installs those stacks:

```bash
curl -sSL https://raw.githubusercontent.com/nudgebee/k8s-agent/main/installation.sh \
  | bash -s -- -a "<your-auth-key>" -C gp3
```

### Verifying chart signatures

Chart packages are signed with [cosign](https://github.com/sigstore/cosign)
keyless signing. Each GitHub release attaches a `<chart>.tgz.sigstore.json`
Sigstore bundle (signature + certificate + transparency-log entry) alongside
the chart tarball. To verify a downloaded package (requires cosign v3+):

```bash
VERSION=0.1.1
BASE="https://github.com/nudgebee/k8s-agent/releases/download/nudgebee-agent-${VERSION}"
curl -sSLO "${BASE}/nudgebee-agent-${VERSION}.tgz"
curl -sSLO "${BASE}/nudgebee-agent-${VERSION}.tgz.sigstore.json"

cosign verify-blob \
  --bundle "nudgebee-agent-${VERSION}.tgz.sigstore.json" \
  --certificate-identity-regexp "^https://github\.com/nudgebee/k8s-agent/\.github/workflows/release(-rc)?\.yml@.*" \
  --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
  "nudgebee-agent-${VERSION}.tgz"
```

### OpenShift

Install with the OpenShift values file as a cluster-admin (the chart creates
SecurityContextConstraints and ClusterRoles):

```bash
helm upgrade --install nudgebee-agent nudgebee-agent/nudgebee-agent \
  -n nudgebee-agent --create-namespace \
  -f https://raw.githubusercontent.com/nudgebee/k8s-agent/main/openshift-values.yaml \
  --set runner.nudgebee.auth_secret_key="<access_key>:<access_secret>"
```

[`openshift-values.yaml`](openshift-values.yaml) creates the SCCs, lets the
node-agent run under a privileged SCC, and turns off ClickHouse's fixed UID so
OpenShift can assign one. `installation.sh -o` sets the same
values. To run without any privileged workload, also pass
`--set openshift.createPrivilegedScc=false --set nodeAgent.enabled=false`.

### Node-agent metrics in ClickHouse (opt-in)

[`node-agent-metrics-values.yaml`](node-agent-metrics-values.yaml) makes the
bundled OpenTelemetry collector also scrape the node-agent's `/metrics` and
write the service-map metrics into ClickHouse (`otel_metrics_*` tables). It is
off by default and does not change Prometheus scraping:

```bash
helm upgrade nudgebee-agent nudgebee-agent/nudgebee-agent -n nudgebee-agent \
  --reuse-values \
  -f https://raw.githubusercontent.com/nudgebee/k8s-agent/main/node-agent-metrics-values.yaml
```

To turn it off, upgrade without this file and without `--reuse-values`.

### Upgrading from 0.1.31 or earlier

Charts up to 0.1.31 ran ClickHouse from the Bitnami subchart. Later charts run
the official ClickHouse image, same version (24.12.4), on the same StatefulSet
and the same volume: an upgrade restarts the ClickHouse pod once and keeps its
data, its Services and its password.

`--reuse-values` keeps the earlier release's values, including its Bitnami-based
ClickHouse image, and the chart refuses to render with it. Upgrade with
`--reset-then-reuse-values` (Helm 3.14+), which applies the new chart's defaults
under your own overrides:

```bash
helm upgrade nudgebee-agent nudgebee-agent/nudgebee-agent -n nudgebee-agent \
  --reset-then-reuse-values
```

On Helm older than 3.14, pass your values file instead (`-f my-values.yaml`,
without `--reuse-values`). A `--reuse-values` upgrade fails while rendering,
before anything is applied, so it puts no data at risk.

Before you upgrade:

- **Mirrored images.** The ClickHouse StatefulSet and the pre-upgrade schema
  job both use `ghcr.io/nudgebee/clickhouse:24.12.4-nb-1`. If you pull from a
  private mirror, mirror that image first and point `clickhouse.image` at it,
  or the upgrade stops at the pre-upgrade hook. The chart recognises a Bitnami
  image only by `bitnami` in the repository or `-debian-` in the tag, so a
  mirrored Bitnami image under another name is not caught when rendering.
- **One restart.** The upgrade restarts the ClickHouse pod once. The startup
  probe gives it up to 5 minutes to load the volume. Later chart releases do
  not restart it unless its image or configuration changes.
- **Password changes.** `clickhouse.auth.password` is used only when the
  `<release>-clickhouse` Secret is first created; changing it later does not
  change the password (the Bitnami image used to re-apply it on every start).
  To change the password, update `admin-password` in that Secret (or in your
  `clickhouse.auth.existingSecret`), then restart the ClickHouse pod, the
  runner and the OpenTelemetry collector, which read it at start.
- **OpenShift.** The upgrade has been checked by rendering only; try it on a
  non-production OpenShift cluster first.

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

# StorageClass for the PVCs the chart creates (see Storage below)
global:
  storageClass: ""

# The collector subchart can be disabled if not needed. ClickHouse is installed
# together with the collector.
opentelemetry-collector:
  enabled: true
```

Full configuration reference: [installation guide](https://app.nudgebee.com/help/docs/installation/agent/installation/).

### Storage

ClickHouse is the only component of *this chart* that claims persistent
storage — one PVC per replica, `50Gi` by default. Left unset, the PVC is
provisioned from the cluster's **default StorageClass**; on clusters that have
none, the PVC stays `Pending` and the ClickHouse pod never starts. Set the class
explicitly:

```yaml
global:
  # Applies to the ClickHouse PVC, the only one this chart creates by default.
  storageClass: "gp3"

clickhouse:
  persistence:
    size: 50Gi
    # Per-component alternative — only consulted when global.storageClass is empty.
    storageClass: ""
```

Or on the command line:

```bash
helm upgrade --install nudgebee-agent nudgebee-agent/nudgebee-agent \
  --namespace nudgebee-agent --create-namespace \
  --set runner.nudgebee.auth_secret_key="<your-auth-key>" \
  --set global.storageClass="gp3" \
  --set clickhouse.persistence.size="100Gi"
```

| Value | Effect |
| --- | --- |
| `""` (default) | `storageClassName` is omitted — the cluster's default StorageClass provisions the volume |
| `"<name>"` | PVCs request that StorageClass (e.g. `gp3`, `managed-csi`, `standard-rwo`) |
| `"-"` | `storageClassName: ""` — dynamic provisioning is disabled, so the PVC binds a pre-created PV |

The `"-"` form is a chart-value convention and works only for the values above,
not for the installer's `-C` flag (see below).

`global.storageClass` takes precedence over `clickhouse.persistence.storageClass`
when both are set. The StorageClass of an existing PVC is immutable, so changing
it on an already-installed release only affects PVCs created afterwards — delete
the old PVC (losing the local traces/logs, which have a 7-day TTL) to move
ClickHouse to a different class.

The stacks `installation.sh` installs alongside the agent claim volumes of their
own, and `-C` sets the class on those too:

| PVC | Size | Value the installer sets |
| --- | --- | --- |
| ClickHouse (this chart) | `50Gi` | `global.storageClass` |
| Prometheus (`kube-prometheus-stack`, via [`kube-prometheus-stack-values.yaml`](kube-prometheus-stack-values.yaml)) | `50Gi` | `prometheus.prometheusSpec.storageSpec.volumeClaimTemplate.spec.storageClassName` |
| Loki (`loki-stack`) | `10Gi` | `loki.persistence.storageClassName` |

Grafana and Alertmanager are left on their chart defaults (no PVC), so nothing
is set for them. Installing the chart directly with `helm` only creates the
ClickHouse PVC — the other two come from stacks the script installs for you.

Before installing anything, the script checks the StorageClass it is about to
use: an unknown `-C` name, or no `-C` on a cluster with no default class, is an
error that lists the classes that do exist. A PVC bound to a missing class only
sits `Pending`, so failing up front is the difference between a one-line message
and a silent stall.

`-C` takes a StorageClass **name** only — not the `"-"` form above, which the
Prometheus and Loki charts do not implement. To bind pre-created PVs, install the
chart directly with `--set global.storageClass=-` and install those stacks
yourself.

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
- Logs, traces, and profiles from `node-agent` (configurable; sensitive HTTP headers are redacted via the `SENSITIVE_HEADERS` env var)

Secrets are explicitly **not** watched by the kubewatch forwarder (`kubewatch.config.resource.secret: false`).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). For security issues, see [SECURITY.md](SECURITY.md).

## License

[Apache License 2.0](LICENSE).
