package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRegistry_AllMetricsPreRegistered(t *testing.T) {
	r := New()
	// Touch each collector so it appears in the scrape (otherwise zero-value
	// Counters/Histograms with no labels won't materialize until first .Inc()).
	r.ActionsTotal.WithLabelValues("ping", "ok").Inc()
	r.ActionDuration.WithLabelValues("ping").Observe(0.01)
	r.AlertsForwarded.Inc()
	r.AlertsDropped.Inc()
	r.DiscoveryPosts.WithLabelValues("service", "true").Inc()
	r.DiscoveryErrors.WithLabelValues("service").Inc()
	r.RelayReconnects.Inc()
	r.RelayConnected.Set(1)

	want := []string{
		"nudgebee_agent_actions_total",
		"nudgebee_agent_action_duration_seconds",
		"nudgebee_agent_alerts_forwarded_total",
		"nudgebee_agent_alerts_dropped_total",
		"nudgebee_agent_discovery_posts_total",
		"nudgebee_agent_discovery_errors_total",
		"nudgebee_agent_relay_reconnects_total",
		"nudgebee_agent_relay_connected",
	}
	for _, name := range want {
		if testutil.CollectAndCount(r.reg, name) == 0 {
			t.Errorf("metric %s not present in registry", name)
		}
	}
}

func TestHandler_ServesPrometheusFormat(t *testing.T) {
	r := New()
	r.ActionsTotal.WithLabelValues("ping", "ok").Inc()

	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	s := string(body)
	if !strings.Contains(s, "nudgebee_agent_actions_total") {
		t.Errorf("response missing actions_total:\n%s", s[:min(500, len(s))])
	}
	if !strings.Contains(s, `action="ping"`) {
		t.Errorf("response missing labeled sample")
	}
}

func TestRegistry_GoAndProcessCollectors(t *testing.T) {
	r := New()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp, _ := http.Get(srv.URL)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	s := string(body)
	for _, want := range []string{"go_goroutines", "go_memstats_alloc_bytes"} {
		if !strings.Contains(s, want) {
			t.Errorf("response missing standard collector %q", want)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
