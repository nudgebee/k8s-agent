# NudgeBee agent operator

Helm-based operator for `charts/nudgebee-agent`, tracked in nudgebee/nudgebee-enterprise#40474.

## Decisions

- Lives in `operator/` in this repo. The chart is not copied into git: the image build
  takes it from `charts/nudgebee-agent`, so the two cannot drift.
- A Helm operator passes the custom resource `spec` to the chart as values. The typed
  fields therefore use the chart's own value paths, with no translation layer, and any
  other chart value is passed through.

## `NudgebeeAgent` spec (first version)

| Field | Chart value | Default |
|---|---|---|
| `spec.runner.nudgebee.authSecretKeyFrom.{name,key}` | same | required |
| `spec.runner.readOnly` | same | `true` |
| `spec.runner.enableWritePermissions` | same | `false` |
| `spec.nodeAgent.enabled` | same | chart default |
| `spec.clickhouse.enabled` | same | `true` |
| `spec.clickhouse.auth.existingSecret` | same | required while ClickHouse is enabled; must be `nudgebee-agent-clickhouse` |
| `spec.openshift.{enabled,createScc,createPrivilegedScc}` | same | `true` |

Status is what the Helm operator writes: `conditions` and `deployedRelease`.

## Constraints

- **Resource name.** The chart hardcodes `nudgebee-agent-clickhouse` in the collector
  settings, and the Helm release name comes from the custom resource name. Only the name
  `nudgebee-agent` is supported. The CRD does not enforce this yet.
- **Permissions.** The operator's service account must hold everything the chart grants,
  including SecurityContextConstraints and ClusterRoles, so `config/rbac/role.yaml` is
  regenerated from the chart.
- **Subchart.** `opentelemetry-collector` must be vendored before the image is built.

## Found on OpenShift Local

- The chart's random `rollme` annotation on the runner made every reconcile an upgrade.
  Added `runner.rolloutOnUpgrade` (default `true`, so Helm installs are unchanged); the
  operator forces it off, and the runner then restarts only when a value changes.
- The scaffold's operator pod, ClickHouse and the agent images need more than the
  default 35 GB OpenShift Local disk; 50 GB was used.
- With the chart's generated ClickHouse password, the operator upgraded the release every
  ~15s: its trial render cannot `lookup` the existing Secret, so a new password made the
  manifest differ. Decided: the user creates the Secret and names it in
  `spec.clickhouse.auth.existingSecret`, which the CRD requires. The Secret must be named
  `nudgebee-agent-clickhouse` with the key `admin-password`, because the collector's
  values reference both literally; another name leaves the collector unable to start.
- The same applies to anything else the chart looks up. `clickhouse.timeseries` falls back
  to the `kube-system` UID, so the CRD requires `clusterName` when it is enabled.

## Build and deploy

    helm dependency build charts/nudgebee-agent
    python3 operator/hack/gen-rbac.py      # needs PyYAML; rerun after chart changes
    make -C operator docker-build IMG=<image>
    make -C operator deploy IMG=<image>

Neither `make` target has been run to completion; testing used `docker build -f
operator/Dockerfile .` and `oc kustomize operator/config/default`.

## Checked on OpenShift Local (fresh install)

- A `NudgebeeAgent` resource installs the agent; the runner connects and traces reach ClickHouse.
- The release stays at revision 1 while idle, and a spec change makes exactly one upgrade.
- Deleting the resource removes the release. The hook job, the data volume and the Secrets
  you created stay, as with `helm uninstall`.

`operator-sdk run bundle` (the ticket's wording) needs the bundle from #40475.

## Not in this change

Bundle and CSV (#40475), CI publishing (#40479), digest pinning (#40477).
