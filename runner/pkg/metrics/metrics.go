// Package metrics exposes Prometheus metrics for the agent itself —
// dispatch counters, latency histograms, alert-forward drop counts.
//
// Mounted on the same HTTP server as alerts (default :5000) at /metrics
// so the chart's existing PodMonitor scrape path works unchanged.
package metrics

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds the agent's metrics. One per process.
type Registry struct {
	reg *prometheus.Registry

	ActionsTotal    *prometheus.CounterVec   // labels: action, status
	ActionDuration  *prometheus.HistogramVec // labels: action
	AlertsForwarded prometheus.Counter
	AlertsDropped   prometheus.Counter
	DiscoveryPosts  *prometheus.CounterVec // labels: type, full_load
	DiscoveryErrors *prometheus.CounterVec // labels: type
	ForwardShed     *prometheus.CounterVec // labels: source (kubewatch|alertmanager|relay)
	RelayReconnects prometheus.Counter
	RelayConnected  prometheus.Gauge
}

// New builds the registry and pre-registers all collectors.
func New() *Registry {
	r := prometheus.NewRegistry()
	// Standard Go runtime + process metrics, namespaced.
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{Namespace: "nudgebee_agent"}),
	)
	reg := &Registry{reg: r}
	reg.ActionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nudgebee_agent",
		Name:      "actions_total",
		Help:      "Total actions dispatched, labeled by name and final status.",
	}, []string{"action", "status"})
	reg.ActionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "nudgebee_agent",
		Name:      "action_duration_seconds",
		Help:      "Time taken to handle an action.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"action"})
	reg.AlertsForwarded = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "nudgebee_agent",
		Name:      "alerts_forwarded_total",
		Help:      "AlertManager webhooks successfully forwarded to backend.",
	})
	reg.AlertsDropped = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "nudgebee_agent",
		Name:      "alerts_dropped_total",
		Help:      "AlertManager webhooks dropped because backend forward failed.",
	})
	reg.DiscoveryPosts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nudgebee_agent",
		Name:      "discovery_posts_total",
		Help:      "Discovery envelopes posted to backend.",
	}, []string{"type", "full_load"})
	reg.DiscoveryErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nudgebee_agent",
		Name:      "discovery_errors_total",
		Help:      "Discovery envelope POSTs that failed.",
	}, []string{"type"})
	reg.ForwardShed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nudgebee_agent",
		Name:      "forward_shed_total",
		Help:      "Inbound events/handlers dropped because the bounded worker pool was saturated.",
	}, []string{"source"})
	reg.RelayReconnects = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "nudgebee_agent",
		Name:      "relay_reconnects_total",
		Help:      "Relay WS reconnect attempts.",
	})
	reg.RelayConnected = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "nudgebee_agent",
		Name:      "relay_connected",
		Help:      "1 when the relay WS is open, 0 otherwise.",
	})

	r.MustRegister(
		reg.ActionsTotal,
		reg.ActionDuration,
		reg.AlertsForwarded,
		reg.AlertsDropped,
		reg.DiscoveryPosts,
		reg.DiscoveryErrors,
		reg.ForwardShed,
		reg.RelayReconnects,
		reg.RelayConnected,
	)
	return reg
}

// Handler returns the http.Handler to mount at /metrics.
func (r *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{Registry: r.reg})
}

// Gatherer returns the underlying registry; useful for tests that want to
// scrape directly without the HTTP layer.
func (r *Registry) Gatherer() prometheus.Gatherer { return r.reg }

// OnAction satisfies dispatch.Metrics.
func (r *Registry) OnAction(action, status string) {
	r.ActionsTotal.WithLabelValues(action, status).Inc()
}

// OnActionDuration satisfies dispatch.Metrics.
func (r *Registry) OnActionDuration(action string, seconds float64) {
	r.ActionDuration.WithLabelValues(action).Observe(seconds)
}

// OnDiscoveryPost satisfies discovery.SinkMetrics. full_load separates the
// periodic snapshot traffic from the per-event incremental traffic, which is
// the split you need to tell "the agent is re-sending the world" from "the
// cluster is actually churning".
func (r *Registry) OnDiscoveryPost(typ string, fullLoad bool) {
	r.DiscoveryPosts.WithLabelValues(typ, strconv.FormatBool(fullLoad)).Inc()
}

// OnDiscoveryError satisfies discovery.SinkMetrics.
func (r *Registry) OnDiscoveryError(typ string) {
	r.DiscoveryErrors.WithLabelValues(typ).Inc()
}

// OnAlertForwarded / OnAlertDropped record backend forward outcomes for
// AlertManager webhooks and kubewatch-derived Findings.
func (r *Registry) OnAlertForwarded() { r.AlertsForwarded.Inc() }
func (r *Registry) OnAlertDropped()   { r.AlertsDropped.Inc() }

// OnRelayConnected drives the relay_connected gauge from the WS session
// lifecycle. Without it the gauge sits at its zero value forever, which reads
// as "relay down" on a perfectly healthy agent.
func (r *Registry) OnRelayConnected(connected bool) {
	if connected {
		r.RelayConnected.Set(1)
		return
	}
	r.RelayConnected.Set(0)
}

// OnRelayReconnect counts redial attempts after a session ends.
func (r *Registry) OnRelayReconnect() { r.RelayReconnects.Inc() }
