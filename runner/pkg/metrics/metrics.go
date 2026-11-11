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

	KubewatchEvents    *prometheus.CounterVec   // labels: kind, outcome
	KubewatchHandle    prometheus.Histogram     // /api/handle time held from the sender
	KubewatchQueueWait prometheus.Histogram     // time an event waited for a worker
	KubewatchMatch     *prometheus.HistogramVec // labels: kind; predicates + evidence lookups
	K8sWatchEvents     *prometheus.CounterVec   // labels: kind, outcome; the runner's own watch (pkg/k8swatch)
	K8sWatchPending    prometheus.Gauge         // objects with changes waiting to be matched
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

	reg.KubewatchEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nudgebee_agent",
		Name:      "kubewatch_events_total",
		Help:      "kubewatch events received, by kind and outcome (matched, unmatched, ignored_kind, shed, invalid, panic).",
	}, []string{"kind", "outcome"})
	reg.KubewatchHandle = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "nudgebee_agent",
		Name:      "kubewatch_handle_seconds",
		Help:      "Time /api/handle holds the kubewatch request. kubewatch sends its next event of a type only after this returns.",
		Buckets:   []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, 1},
	})
	reg.KubewatchQueueWait = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "nudgebee_agent",
		Name:      "kubewatch_queue_wait_seconds",
		Help:      "Time a kubewatch event waited in the intake queue for a worker.",
		Buckets:   []float64{.001, .01, .1, .5, 1, 5, 15, 30, 60, 120},
	})
	reg.KubewatchMatch = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "nudgebee_agent",
		Name:      "kubewatch_match_seconds",
		Help:      "Time spent matching one kubewatch event: predicates plus the evidence lookups of any matcher that fired.",
		Buckets:   []float64{.0001, .001, .005, .01, .05, .1, .5, 1, 3, 10},
	}, []string{"kind"})

	reg.K8sWatchEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "nudgebee_agent",
		Name:      "k8s_watch_events_total",
		Help:      "Kubernetes changes seen by the runner's own watch, by kind and outcome (queued, coalesced, shed, dropped_existing, dropped_no_matcher, encode_failed; unchanged also counts relisted updates, which are queued too). Match outcomes are in kubewatch_events_total.",
	}, []string{"kind", "outcome"})
	reg.K8sWatchPending = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "nudgebee_agent",
		Name:      "k8s_watch_pending",
		Help:      "Objects with changes waiting to be matched by the runner's own watch.",
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
		reg.KubewatchEvents,
		reg.KubewatchHandle,
		reg.KubewatchQueueWait,
		reg.KubewatchMatch,
		reg.K8sWatchEvents,
		reg.K8sWatchPending,
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

// OnK8sWatchEvent satisfies k8swatch.Metrics.
func (r *Registry) OnK8sWatchEvent(kind, outcome string) {
	r.K8sWatchEvents.WithLabelValues(kind, outcome).Inc()
}

// SetK8sWatchPending satisfies k8swatch.Metrics.
func (r *Registry) SetK8sWatchPending(n int) { r.K8sWatchPending.Set(float64(n)) }

// OnKubewatchEvent satisfies alerts.KubewatchMetrics.
func (r *Registry) OnKubewatchEvent(kind, outcome string) {
	r.KubewatchEvents.WithLabelValues(kind, outcome).Inc()
}

// ObserveKubewatchHandle satisfies alerts.KubewatchMetrics.
func (r *Registry) ObserveKubewatchHandle(seconds float64) { r.KubewatchHandle.Observe(seconds) }

// ObserveKubewatchQueueWait satisfies alerts.KubewatchMetrics.
func (r *Registry) ObserveKubewatchQueueWait(seconds float64) { r.KubewatchQueueWait.Observe(seconds) }

// ObserveKubewatchMatch satisfies alerts.KubewatchMetrics.
func (r *Registry) ObserveKubewatchMatch(kind string, seconds float64) {
	r.KubewatchMatch.WithLabelValues(kind).Observe(seconds)
}

// RegisterKubewatchQueueDepth exposes the intake's queue depth as a gauge read
// at scrape time. Call once.
func (r *Registry) RegisterKubewatchQueueDepth(depth func() float64) {
	r.reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: "nudgebee_agent",
		Name:      "kubewatch_queue_depth",
		Help:      "kubewatch events waiting in the intake queue for a worker.",
	}, depth))
}

// OnRelayReconnect counts redial attempts after a session ends.
func (r *Registry) OnRelayReconnect() { r.RelayReconnects.Inc() }
