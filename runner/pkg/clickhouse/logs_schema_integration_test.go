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
		if state, err := EnsureLogsTable(ctx, c, DefaultLogsRetention, discardLogger()); state != LogsTableReady {
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

	t.Run("retention follows config", func(t *testing.T) {
		before := tableInfo(ctx, t, c, "default", LogsTable)
		t.Logf("engine_full at 72h: %s", before.engineFull)
		if !strings.Contains(before.engineFull, "TTL toDateTime(Timestamp) + toIntervalHour(72) ") {
			t.Fatalf("created at 72h, engine_full = %s", before.engineFull)
		}

		logger, logs := bufLogger()
		if state, err := EnsureLogsTable(ctx, c, 24*time.Hour, logger); state != LogsTableReady {
			t.Fatalf("ensure at 24h: %v (%v)", state, err)
		}
		changed := tableInfo(ctx, t, c, "default", LogsTable)
		t.Logf("engine_full at 24h: %s", changed.engineFull)
		if !strings.Contains(changed.engineFull, "TTL toDateTime(Timestamp) + toIntervalHour(24) ") {
			t.Fatalf("engine_full after 24h = %s", changed.engineFull)
		}
		if !strings.Contains(logs.String(), `"old_retention":"72h0m0s"`) || !strings.Contains(logs.String(), `"new_retention":"24h0m0s"`) {
			t.Errorf("retention change not logged:\n%s", logs.String())
		}

		// Same retention again: no ALTER. metadata_modification_time has
		// second resolution, so wait past it to make a second ALTER visible.
		time.Sleep(1100 * time.Millisecond)
		logger, logs = bufLogger()
		if state, err := EnsureLogsTable(ctx, c, 24*time.Hour, logger); state != LogsTableReady {
			t.Fatalf("ensure at 24h again: %v (%v)", state, err)
		}
		again := tableInfo(ctx, t, c, "default", LogsTable)
		if again != changed {
			t.Errorf("second ensure changed the table:\nbefore %+v\nafter  %+v", changed, again)
		}
		if strings.Contains(logs.String(), "retention") {
			t.Errorf("second ensure logged a retention change:\n%s", logs.String())
		}
	})

	// The 0.1.27 gateway created otel_logs with the exporter's own schema
	// (create_schema defaulted to true), so upgrades start from this shape.
	t.Run("empty exporter-shaped table is renamed and replaced", func(t *testing.T) {
		lc := legacyDatabase(ctx, t, c, "legacy_empty", 0)
		if state, err := EnsureLogsTable(ctx, lc, DefaultLogsRetention, discardLogger()); state != LogsTableReady {
			t.Fatalf("want ready, got %v (%v)", state, err)
		}
		if got := tablesIn(ctx, t, lc, "legacy_empty"); got != "otel_logs,otel_logs_legacy" {
			t.Errorf("tables = %s; want otel_logs,otel_logs_legacy", got)
		}
		cols, err := columnsOf(ctx, lc, LogsTable)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cols["namespace"]; !ok {
			t.Errorf("new otel_logs lacks namespace: %v", cols)
		}
	})

	t.Run("exporter-shaped table with rows is left alone", func(t *testing.T) {
		lc := legacyDatabase(ctx, t, c, "legacy_rows", 1)
		state, err := EnsureLogsTable(ctx, lc, DefaultLogsRetention, discardLogger())
		if state != LogsTableLegacyShape {
			t.Fatalf("want legacy shape, got %v (%v)", state, err)
		}
		t.Logf("legacy-shape error: %v", err)
		if got := tablesIn(ctx, t, lc, "legacy_rows"); got != "otel_logs" {
			t.Errorf("tables = %s; want otel_logs only", got)
		}
	})
}

type chTableInfo struct {
	engineFull, metadataModified string
}

func tableInfo(ctx context.Context, t *testing.T, c *Client, db, table string) chTableInfo {
	t.Helper()
	res, err := c.Query(ctx, fmt.Sprintf("SELECT engine_full, toString(metadata_modification_time) FROM system.tables WHERE database = '%s' AND name = '%s'", db, table), nil)
	if err != nil || res.Error != nil {
		t.Fatalf("system.tables: %v %v", err, res.Error)
	}
	if len(res.Data) != 1 || len(res.Data[0]) != 2 {
		t.Fatalf("system.tables: want 1 row of 2, got %v", res.Data)
	}
	return chTableInfo{fmt.Sprint(res.Data[0][0]), fmt.Sprint(res.Data[0][1])}
}

func tablesIn(ctx context.Context, t *testing.T, c *Client, db string) string {
	t.Helper()
	res, err := c.Query(ctx, fmt.Sprintf("SELECT arrayStringConcat(groupArray(name), ',') FROM (SELECT name FROM system.tables WHERE database = '%s' ORDER BY name)", db), nil)
	if err != nil || res.Error != nil || len(res.Data) != 1 || len(res.Data[0]) != 1 {
		t.Fatalf("list tables: %v %+v", err, res)
	}
	return fmt.Sprint(res.Data[0][0])
}

// legacyDatabase creates db holding an otel_logs in the exporter's own shape
// (its 15 insert columns, the v0.157 sort key) with `rows` rows, and returns
// a client for db.
func legacyDatabase(ctx context.Context, t *testing.T, c *Client, db string, rows int) *Client {
	t.Helper()
	if err := c.Exec(ctx, "CREATE DATABASE "+db); err != nil {
		t.Fatal(err)
	}
	lc := *c
	lc.Database = db
	if err := lc.Exec(ctx, fmt.Sprintf(exporterShapedLogsDDL, db)); err != nil {
		t.Fatalf("legacy DDL: %v", err)
	}
	for i := 0; i < rows; i++ {
		if err := lc.Exec(ctx, fmt.Sprintf("INSERT INTO %s.otel_logs (Timestamp, ServiceName, Body) VALUES (now64(9), 'legacy', 'row %d')", db, i)); err != nil {
			t.Fatalf("legacy insert: %v", err)
		}
	}
	return &lc
}

const exporterShapedLogsDDL = `CREATE TABLE %s.otel_logs
(
    Timestamp DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    TraceId String CODEC(ZSTD(1)),
    SpanId String CODEC(ZSTD(1)),
    TraceFlags UInt8 CODEC(ZSTD(1)),
    SeverityText LowCardinality(String) CODEC(ZSTD(1)),
    SeverityNumber UInt8 CODEC(ZSTD(1)),
    ServiceName LowCardinality(String) CODEC(ZSTD(1)),
    Body String CODEC(ZSTD(1)),
    ResourceSchemaUrl LowCardinality(String) CODEC(ZSTD(1)),
    ResourceAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    ScopeSchemaUrl LowCardinality(String) CODEC(ZSTD(1)),
    ScopeName String CODEC(ZSTD(1)),
    ScopeVersion LowCardinality(String) CODEC(ZSTD(1)),
    ScopeAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1)),
    LogAttributes Map(LowCardinality(String), String) CODEC(ZSTD(1))
)
ENGINE = MergeTree
PARTITION BY toDate(Timestamp)
ORDER BY (toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp)
TTL toDateTime(Timestamp) + toIntervalDay(3)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1`
