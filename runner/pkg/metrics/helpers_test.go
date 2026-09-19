package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRegistry_AllMetricsPreRegistered touches every collector by hand, so it
// passes whether or not anything in the agent ever calls them — which is how
// discovery_posts_total, alerts_forwarded_total and relay_connected shipped
// declared, documented and permanently zero. These tests assert the helpers
// the production code actually calls, and the values they produce.

func TestOnDiscoveryPost_LabelsSplitSnapshotFromIncremental(t *testing.T) {
	r := New()
	r.OnDiscoveryPost("service", true)
	r.OnDiscoveryPost("service", true)
	r.OnDiscoveryPost("service", false)
	r.OnDiscoveryPost("node", false)

	expected := `
# HELP nudgebee_agent_discovery_posts_total Discovery envelopes posted to backend.
# TYPE nudgebee_agent_discovery_posts_total counter
nudgebee_agent_discovery_posts_total{full_load="false",type="node"} 1
nudgebee_agent_discovery_posts_total{full_load="false",type="service"} 1
nudgebee_agent_discovery_posts_total{full_load="true",type="service"} 2
`
	if err := testutil.CollectAndCompare(r.reg, strings.NewReader(expected),
		"nudgebee_agent_discovery_posts_total"); err != nil {
		t.Error(err)
	}
}

func TestOnDiscoveryError_CountsPerType(t *testing.T) {
	r := New()
	r.OnDiscoveryError("service")
	r.OnDiscoveryError("service")
	r.OnDiscoveryError("job")

	if got := testutil.ToFloat64(r.DiscoveryErrors.WithLabelValues("service")); got != 2 {
		t.Errorf("discovery_errors_total{type=service} = %v; want 2", got)
	}
	if got := testutil.ToFloat64(r.DiscoveryErrors.WithLabelValues("job")); got != 1 {
		t.Errorf("discovery_errors_total{type=job} = %v; want 1", got)
	}
}

// relay_connected reported 0 on a healthy, actively-dispatching agent because
// nothing ever set it. The gauge has to track both edges.
func TestOnRelayConnected_TracksBothEdges(t *testing.T) {
	r := New()
	if got := testutil.ToFloat64(r.RelayConnected); got != 0 {
		t.Fatalf("relay_connected starts at %v; want 0", got)
	}
	r.OnRelayConnected(true)
	if got := testutil.ToFloat64(r.RelayConnected); got != 1 {
		t.Errorf("relay_connected after connect = %v; want 1", got)
	}
	r.OnRelayConnected(false)
	if got := testutil.ToFloat64(r.RelayConnected); got != 0 {
		t.Errorf("relay_connected after disconnect = %v; want 0", got)
	}
}

func TestAlertAndReconnectCounters(t *testing.T) {
	r := New()
	r.OnAlertForwarded()
	r.OnAlertForwarded()
	r.OnAlertDropped()
	r.OnRelayReconnect()

	if got := testutil.ToFloat64(r.AlertsForwarded); got != 2 {
		t.Errorf("alerts_forwarded_total = %v; want 2", got)
	}
	if got := testutil.ToFloat64(r.AlertsDropped); got != 1 {
		t.Errorf("alerts_dropped_total = %v; want 1", got)
	}
	if got := testutil.ToFloat64(r.RelayReconnects); got != 1 {
		t.Errorf("relay_reconnects_total = %v; want 1", got)
	}
}
