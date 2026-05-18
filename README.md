# NudgeBee Kubernetes agent

Helm chart for the NudgeBee Kubernetes agent — connects your cluster to the NudgeBee backend.

## Install

```bash
helm repo add nudgebee-agent https://nudgebee.github.io/k8s-agent/
helm repo update
helm upgrade --install nudgebee-agent nudgebee-agent/nudgebee-agent \
  --namespace nudgebee-agent --create-namespace \
  --set runner.nudgebee.auth_secret_key="<your-auth-key>"
```

Or use `installation.sh` for an opinionated install with Prometheus auto-discovery:

```bash
curl -sSL https://raw.githubusercontent.com/nudgebee/k8s-agent/main/installation.sh | bash -s -- -a "<your-auth-key>"
```

## Documentation

See the [installation guide](https://app.nudgebee.com/help/docs/installation/agent/installation/) for full setup, configuration, and troubleshooting.
