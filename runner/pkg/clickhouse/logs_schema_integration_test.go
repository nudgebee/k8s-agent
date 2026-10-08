//go:build integration

package clickhouse

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestOtelLogs_AgainstRealClickHouse(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "clickhouse/clickhouse-server:24.12",
			ExposedPorts: []string{"8123/tcp"},
			Env:          map[string]string{"CLICKHOUSE_SKIP_USER_SETUP": "1"},
			WaitingFor:   wait.ForHTTP("/ping").WithPort("8123/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "8123/tcp")
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}
	portNum, err := strconv.Atoi(port.Port())
	if err != nil {
		t.Fatalf("mapped port %q: %v", port.Port(), err)
	}
	c := New(Config{Host: host, Port: portNum, Database: "default"})

	// Fresh table is created and reports ready; a second call is a no-op.
	for i := 0; i < 2; i++ {
		if state, err := EnsureLogsTable(ctx, c, DefaultLogsRetention); state != LogsTableReady {
			t.Fatalf("call %d: want ready, got %v (%v)", i+1, state, err)
		}
	}

	// Insert with the exporter's exact column list (logs_insert.sql v0.157.0).
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000000000")
	insert := `INSERT INTO otel_logs (Timestamp, TraceId, SpanId, TraceFlags, SeverityText, SeverityNumber, ServiceName, Body, ResourceSchemaUrl, ResourceAttributes, ScopeSchemaUrl, ScopeName, ScopeVersion, ScopeAttributes, LogAttributes) FORMAT JSONEachRow
` + fmt.Sprintf(`{"Timestamp":"%[1]s","TraceId":"","SpanId":"","TraceFlags":0,"SeverityText":"","SeverityNumber":0,"ServiceName":"checkout","Body":"ERROR connection timeout to payments:8080","ResourceSchemaUrl":"","ResourceAttributes":{"k8s.namespace.name":"shop","k8s.pod.name":"checkout-7d9f8c-abcde","k8s.container.name":"app","k8s.node.name":"node-1","k8s.deployment.name":"checkout"},"ScopeSchemaUrl":"","ScopeName":"","ScopeVersion":"","ScopeAttributes":{},"LogAttributes":{"log.iostream":"stderr"}}
{"Timestamp":"%[1]s","TraceId":"","SpanId":"","TraceFlags":0,"SeverityText":"INFO","SeverityNumber":9,"ServiceName":"one-off","Body":"error budget report written","ResourceSchemaUrl":"","ResourceAttributes":{"k8s.namespace.name":"jobs","k8s.pod.name":"one-off","k8s.container.name":"main"},"ScopeSchemaUrl":"","ScopeName":"","ScopeVersion":"","ScopeAttributes":{},"LogAttributes":{"log.iostream":"stdout"}}
{"Timestamp":"%[1]s","TraceId":"","SpanId":"","TraceFlags":0,"SeverityText":"","SeverityNumber":0,"ServiceName":"web","Body":"Warning: slow upstream","ResourceSchemaUrl":"","ResourceAttributes":{"k8s.namespace.name":"shop","k8s.pod.name":"web-0","k8s.container.name":"nginx","k8s.statefulset.name":"web"},"ScopeSchemaUrl":"","ScopeName":"","ScopeVersion":"","ScopeAttributes":{},"LogAttributes":{"log.iostream":"stdout"}}
{"Timestamp":"%[1]s","TraceId":"","SpanId":"","TraceFlags":0,"SeverityText":"","SeverityNumber":0,"ServiceName":"api","Body":"request served in 12ms","ResourceSchemaUrl":"","ResourceAttributes":{"k8s.namespace.name":"zeta","k8s.pod.name":"api-5f6d7-xyz12","k8s.container.name":"api","k8s.deployment.name":"api"},"ScopeSchemaUrl":"","ScopeName":"","ScopeVersion":"","ScopeAttributes":{},"LogAttributes":{"log.iostream":"stdout"}}`, now)
	resp, err := http.Post(fmt.Sprintf("http://%s:%s/?database=default", host, port.Port()), "text/plain", strings.NewReader(insert))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("insert: status=%d body=%s", resp.StatusCode, body)
	}

	res, err := c.Query(ctx, "SELECT namespace, workload, pod, container, node, stream, level FROM otel_logs ORDER BY namespace, workload", nil)
	if err != nil || res.Error != nil {
		t.Fatalf("select: %v %v", err, res.Error)
	}
	want := [][]string{
		{"jobs", "one-off", "one-off", "main", "", "stdout", "info"},                      // no owner → pod name; SeverityNumber wins over "error" text
		{"shop", "checkout", "checkout-7d9f8c-abcde", "app", "node-1", "stderr", "error"}, // severity from text
		{"shop", "web", "web-0", "nginx", "", "stdout", "warning"},                        // statefulset owner; "Warning" text
		{"zeta", "api", "api-5f6d7-xyz12", "api", "", "stdout", "unknown"},                // no severity, neutral body
	}
	if len(res.Data) != len(want) {
		t.Fatalf("want %d rows, got %d: %v", len(want), len(res.Data), res.Data)
	}
	for i, row := range res.Data {
		if len(row) != len(want[i]) || len(row) != len(res.Columns) {
			t.Errorf("row %d: width %d, want %d (columns %d): %v", i, len(row), len(want[i]), len(res.Columns), row)
			continue
		}
		for j, v := range row {
			if fmt.Sprint(v) != want[i][j] {
				t.Errorf("row %d col %s: got %q want %q", i, res.Columns[j], v, want[i][j])
			}
		}
	}

	sk, err := c.Query(ctx, "SELECT sorting_key FROM system.tables WHERE database = 'default' AND name = 'otel_logs'", nil)
	if err != nil || sk == nil || sk.Error != nil {
		t.Fatalf("sorting key query: err=%v result=%+v", err, sk)
	}
	if len(sk.Data) == 0 || len(sk.Data[0]) == 0 {
		t.Fatalf("sorting key query returned no rows (table missing?): %+v", sk)
	}
	if got := fmt.Sprint(sk.Data[0][0]); got != "namespace, workload, toStartOfFiveMinutes(Timestamp), pod, container, Timestamp" {
		t.Errorf("sorting key = %q", got)
	}
}
