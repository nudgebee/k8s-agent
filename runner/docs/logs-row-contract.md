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
| resource `k8s.deployment.name` / `k8s.statefulset.name` / `k8s.daemonset.name` / `k8s.cronjob.name` / `k8s.job.name` | when owned | Exactly one, the top-level owner |
| resource `service.name` | yes | The owner's name; the pod name when there is no owner |
| log attribute `log.iostream` | yes | `stdout` or `stderr` |
| log attribute `log.file.path` | otel-daemonset | Source file |
| `TraceId` / `SpanId` | no | Empty unless the source extracts them |

Derived columns (do not send them): `namespace`, `workload`, `pod`, `container`,
`node`, `stream`, `level`.
