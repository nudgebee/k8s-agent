# Contributing to nudgebee-agent

Thanks for your interest in contributing. This repo hosts the Helm chart for the NudgeBee Kubernetes agent.

## Ways to contribute

- Report bugs via [GitHub Issues](https://github.com/nudgebee/k8s-agent/issues).
- Propose features or enhancements via a feature-request issue before opening a large PR.
- Improve documentation (README, chart values, install script).
- Submit chart fixes, additional configuration knobs, or new templates.

For vulnerability reports, **do not open a public issue** — see [SECURITY.md](SECURITY.md).

## Development setup

Prerequisites:

- [Helm](https://helm.sh/docs/intro/install/) 3.12+
- [chart-testing](https://github.com/helm/chart-testing) (`ct`) for lint and install tests
- [yamllint](https://github.com/adrienverge/yamllint)
- A Kubernetes cluster for install tests ([kind](https://kind.sigs.k8s.io/) or [minikube](https://minikube.sigs.k8s.io/) works)

Clone and update subchart dependencies:

```bash
git clone https://github.com/nudgebee/k8s-agent.git
cd k8s-agent
helm dependency update charts/nudgebee-agent
```

## Making changes

1. Branch off `main`: `git checkout -b feat/short-description`.
2. Edit chart templates / values / docs.
3. Bump `version` in `charts/nudgebee-agent/Chart.yaml` if the change is user-visible.
4. Run lint locally before pushing:

   ```bash
   helm lint charts/nudgebee-agent
   ct lint --config .github/configs/ct.yaml
   ```

5. Render templates to sanity-check output:

   ```bash
   helm template test charts/nudgebee-agent --debug > /tmp/rendered.yaml
   ```

6. Commit using [Conventional Commits](https://www.conventionalcommits.org/) (e.g. `fix(chart): ...`, `feat(chart): ...`, `docs: ...`).
7. Open a PR against `main`. Fill out the PR template.

## CI

PRs run:

- `helm-dev-lint.yml` — `helm lint` + `ct lint`
- `helm-prod-test.yml` — install/uninstall test against an ephemeral cluster

Both must pass before merge.

## Releases

Releases are cut from `main` by the maintainers using `release.yml` (publishes a packaged chart to the GitHub Pages Helm repo at `https://nudgebee.github.io/k8s-agent/`).

## Code of Conduct

By participating you agree to abide by the [Code of Conduct](CODE_OF_CONDUCT.md).
