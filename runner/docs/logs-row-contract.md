# Logs row contract (otel_logs)

Every log collection mode (`otel-daemonset`, and `node-agent` once it qualifies,
see #40146) must produce OTel log records shaped like this. The table
(`runner/pkg/clickhouse/logs_schema.go`), the LogQL translator (#40145) and the
sort key all rely on it.

| Field | Required | Value |
|---|---|---|
| `Timestamp` | yes | Event time from the runtime's log line (CRI/Docker time), not read time |
| `Body` | yes | The message without the CRI/Docker prefix; multi-line entries joined with `\n` |
| `SeverityNumber` / `SeverityText` | when known | Set if the source parsed a level, else 0 / "" (the table derives `level` from the text) |
| resource `k8s.namespace.name` | yes | Pod namespace |
| resource `k8s.pod.name`, `k8s.pod.uid` | yes | Pod name and UID |
| resource `k8s.container.name` | yes | Container name |
| resource `k8s.node.name` | yes | Node the pod runs on |
| resource `k8s.deployment.name` / `k8s.statefulset.name` / `k8s.daemonset.name` / `k8s.cronjob.name` / `k8s.job.name` | when owned | The top-level owner is required when the pod is owned; intermediate owners (`k8s.replicaset.name`, or `k8s.job.name` under a cronjob) may also be present. The table picks the top-level one in the order deployment > statefulset > daemonset > cronjob > job > pod |
| resource `service.name` | yes | The top-level owner's name (same priority as above), else the pod name; set by the collector, overriding any label/annotation-derived value |
| log attribute `log.iostream` | yes | `stdout` or `stderr` |
| log attribute `log.file.path` | otel-daemonset | Source file |
| `TraceId` / `SpanId` | no | Empty unless the source extracts them |

Derived columns (do not send them): `namespace`, `workload`, `pod`, `container`,
`node`, `stream`, `level`.

At-least-once delivery: a retried insert can store the same record twice, so
readers must tolerate duplicate rows (deduplication is #40148).

Upgrades, retention changes and other operational behaviour are in
[logs-operations.md](logs-operations.md).
