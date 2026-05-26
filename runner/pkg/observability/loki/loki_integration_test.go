//go:build integration

package loki

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Real Loki container test. Boots grafana/loki, waits for /ready, runs every
// Client method. Loki without ingested logs returns empty result sets, which
// is fine — we're testing the wire-protocol contract of our client, not Loki
// itself.
func TestClient_AgainstRealLoki(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test; -short")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	req := testcontainers.ContainerRequest{
		Image:        "grafana/loki:2.9.4",
		ExposedPorts: []string{"3100/tcp"},
		Cmd:          []string{"-config.file=/etc/loki/local-config.yaml"},
		WaitingFor:   wait.ForHTTP("/ready").WithPort("3100/tcp").WithStartupTimeout(90 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start loki: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	port, err := container.MappedPort(ctx, "3100/tcp")
	if err != nil {
		t.Fatal(err)
	}
	baseURL := fmt.Sprintf("http://%s:%s", host, port.Port())

	c := New(baseURL, &http.Client{Timeout: 10 * time.Second})

	t.Run("Labels_Empty", func(t *testing.T) {
		raw, err := c.Labels(ctx, "", "", "")
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, raw)
		if got["status"] != "success" {
			t.Errorf("status = %v", got["status"])
		}
	})

	t.Run("Query_NoLogs", func(t *testing.T) {
		// Empty Loki, but the client must still produce a valid request and
		// receive a parseable success response.
		raw, err := c.Query(ctx, `{job="x"}`, "", "10")
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, raw)
		if got["status"] != "success" {
			t.Errorf("status = %v", got["status"])
		}
	})

	t.Run("QueryRange_NoLogs", func(t *testing.T) {
		end := time.Now().UnixNano()
		start := end - int64(60*time.Second)
		raw, err := c.QueryRange(ctx,
			`{job="x"}`,
			fmt.Sprintf("%d", start),
			fmt.Sprintf("%d", end),
			"",
			"backward",
			"100",
		)
		if err != nil {
			t.Fatal(err)
		}
		got := decode(t, raw)
		if got["status"] != "success" {
			t.Errorf("status = %v", got["status"])
		}
	})

	t.Run("Handlers_DispatchEndToEnd", func(t *testing.T) {
		hs := Handlers(c)
		got, err := hs["loki_labels"](ctx, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		if string(got.(json.RawMessage)) == "" {
			t.Error("handler returned empty body")
		}
	})
}

func decode(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}
