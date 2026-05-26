//go:build integration

package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Real Prometheus container test. Spins up prom/prometheus, waits for /-/ready,
// runs every Client method against it, and asserts the response is parseable
// Prometheus JSON. Containerized only for the dependency we exercise.
//
// Prereq: a working Docker daemon (`docker version` succeeds). Build tag
// `integration` keeps this out of the default `go test ./...` run.
func TestClient_AgainstRealPrometheus(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; -short")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Minimal config: scrape itself.
	const cfg = `global:
  scrape_interval: 1s
scrape_configs:
  - job_name: prometheus
    static_configs:
      - targets: ['localhost:9090']
`
	req := testcontainers.ContainerRequest{
		Image:        "prom/prometheus:v3.0.0",
		ExposedPorts: []string{"9090/tcp"},
		Cmd: []string{
			"--config.file=/etc/prometheus/prometheus.yml",
			"--web.enable-lifecycle",
		},
		Files: []testcontainers.ContainerFile{{
			Reader:            strings.NewReader(cfg),
			ContainerFilePath: "/etc/prometheus/prometheus.yml",
			FileMode:          0o644,
		}},
		WaitingFor: wait.ForHTTP("/-/ready").WithPort("9090/tcp").WithStartupTimeout(60 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start prometheus: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := container.MappedPort(ctx, "9090/tcp")
	if err != nil {
		t.Fatal(err)
	}
	baseURL := fmt.Sprintf("http://%s:%s", host, port.Port())

	c := New(baseURL, &http.Client{Timeout: 10 * time.Second})

	// Wait for at least one scrape to happen so `up` returns a value.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := c.Query(ctx, "up", "", "")
		if err == nil && hasResult(raw) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	t.Run("Query", func(t *testing.T) {
		raw, err := c.Query(ctx, "up", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if !hasResult(raw) {
			t.Errorf("Query returned no results: %s", raw)
		}
	})

	t.Run("QueryRange", func(t *testing.T) {
		// Just-started Prometheus may have only 0-1 samples; assert the
		// envelope is well-formed rather than insisting on populated results
		// (would make the test flaky).
		end := time.Now().UTC()
		start := end.Add(-5 * time.Second)
		raw, err := c.QueryRange(ctx,
			"up",
			start.Format(time.RFC3339),
			end.Format(time.RFC3339),
			"1s",
			"",
		)
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, raw)
		if got["status"] != "success" {
			t.Errorf("QueryRange status = %v; want success", got["status"])
		}
	})

	t.Run("Labels", func(t *testing.T) {
		raw, err := c.Labels(ctx, "", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, raw)
		labels, _ := got["data"].([]any)
		if !contains(labels, "__name__") {
			t.Errorf("expected __name__ in labels: %v", labels)
		}
	})

	t.Run("LabelValues", func(t *testing.T) {
		raw, err := c.LabelValues(ctx, "job", "", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, raw)
		vals, _ := got["data"].([]any)
		if !contains(vals, "prometheus") {
			t.Errorf("expected job=prometheus in label values: %v", vals)
		}
	})

	t.Run("Series", func(t *testing.T) {
		raw, err := c.Series(ctx, []string{`up`}, "", "")
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, raw)
		series, _ := got["data"].([]any)
		if len(series) == 0 {
			t.Errorf("Series returned empty: %s", raw)
		}
	})

	t.Run("Alerts", func(t *testing.T) {
		raw, err := c.Alerts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, raw)
		// No rules configured, so alerts.data.alerts is empty — but the
		// envelope structure must be there.
		if got["status"] != "success" {
			t.Errorf("Alerts status = %v; want success", got["status"])
		}
	})

	t.Run("Handlers_DispatchEndToEnd", func(t *testing.T) {
		hs := Handlers(c)
		got, err := hs["prometheus_query"](ctx, map[string]any{"query": "up"})
		if err != nil {
			t.Fatal(err)
		}
		if !hasResult(got.(json.RawMessage)) {
			t.Errorf("handler returned no results")
		}
	})
}

func hasResult(raw json.RawMessage) bool {
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	if got["status"] != "success" {
		return false
	}
	data, _ := got["data"].(map[string]any)
	res, _ := data["result"].([]any)
	return len(res) > 0
}

func decode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

func contains(haystack []any, needle string) bool {
	for _, h := range haystack {
		if s, _ := h.(string); s == needle {
			return true
		}
	}
	return false
}
