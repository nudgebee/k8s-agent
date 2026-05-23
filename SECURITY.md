# Security Policy

## Supported versions

We support the latest minor release of the `nudgebee-agent` Helm chart. Older releases receive fixes only at the maintainers' discretion.

| Version | Supported |
| ------- | --------- |
| Latest `0.x` | Yes |
| Older `0.x` | Best-effort |

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security problems.

Email `security@nudgebee.com` with:

- A description of the issue and its impact.
- Steps to reproduce (chart values, Kubernetes version, manifests if relevant).
- Any known mitigations.

You can also use [GitHub's private vulnerability reporting](https://github.com/nudgebee/k8s-agent/security/advisories/new) if you prefer.

We aim to:

- Acknowledge your report within 3 business days.
- Provide an initial assessment within 7 business days.
- Disclose and release a fix coordinated with the reporter.

## Scope

In scope:

- The Helm chart in `charts/nudgebee-agent/` (templates, values, defaults).
- The `installation.sh` bootstrap script.
- Default RBAC and service-account configuration shipped by the chart.

Out of scope (report upstream):

- Vulnerabilities in third-party subcharts (`opencost`, `clickhouse`, `opentelemetry-collector`) — report to those projects.
- Issues in the container images themselves (`nudgebee-agent`, `node-agent`, `kubewatch`) — these live in separate repos.
