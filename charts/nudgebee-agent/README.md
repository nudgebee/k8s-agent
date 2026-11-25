# NudgeBee Agent

The agent runs in your cluster, collects Kubernetes state, events, metrics, logs and traces, and sends them to the NudgeBee backend. It connects out to `relay.nudgebee.com` and `collector.nudgebee.com`; no inbound access is needed.

## Prerequisites

- Kubernetes 1.24+ or Red Hat OpenShift
- Helm 3.12+
- A StorageClass for the ClickHouse volume
- A NudgeBee account and agent auth key (<https://nudgebee.com>)

## Install

```bash
helm repo add nudgebee-agent https://nudgebee.github.io/k8s-agent/
helm upgrade --install nudgebee-agent nudgebee-agent/nudgebee-agent \
  -n nudgebee-agent --create-namespace \
  --set runner.nudgebee.auth_secret_key="<your-auth-key>"
```

## Install on OpenShift

Install as a cluster-admin: the chart creates SecurityContextConstraints and ClusterRoles. The node-agent runs under a privileged SecurityContextConstraints.

```bash
helm upgrade --install nudgebee-agent nudgebee-agent/nudgebee-agent \
  -n nudgebee-agent --create-namespace \
  -f https://raw.githubusercontent.com/nudgebee/k8s-agent/main/openshift-values.yaml \
  --set runner.nudgebee.auth_secret_key="<your-auth-key>"
```

To install without any privileged workload, add `--set openshift.createPrivilegedScc=false --set nodeAgent.enabled=false`.

## Verify

```bash
helm test nudgebee-agent -n nudgebee-agent
```

## Upgrade and uninstall

```bash
helm repo update
helm upgrade nudgebee-agent nudgebee-agent/nudgebee-agent -n nudgebee-agent --reset-then-reuse-values  # Helm 3.14+
helm uninstall nudgebee-agent -n nudgebee-agent
```

ClickHouse volumes are not deleted on uninstall.

## Common settings

| Value | Default | Effect |
| --- | --- | --- |
| `runner.nudgebee.auth_secret_key` | `""` | Agent auth key |
| `runner.readOnly` | `false` | Install a strictly read-only role |
| `runner.enableWritePermissions` | `false` | Let the agent change workloads |
| `globalConfig.prometheus_url` | `""` | Prometheus to query; empty means auto-discover |
| `global.storageClass` | `""` | StorageClass for the chart's volumes |
| `nodeAgent.enabled` | `true` | Per-node logs, traces and profiles collector |
| `openshift.enabled` | `false` | Create the OpenShift resources |
| `tests.image` | UBI 9 minimal | Image of the `helm test` pod |

All settings are in `values.yaml`. Full guide: <https://docs.nudgebee.com/docs/installation/agent/installation/>
