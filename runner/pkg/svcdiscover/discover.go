// Package svcdiscover finds in-cluster service URLs by label selector.
//
// Resolves Prometheus/AlertManager/Loki URLs at runtime by listing all
// services in the cluster against a list of well-known label selectors
// (kube-prometheus-stack, prometheus-server, prometheus-operator, …) and
// returning the first hit's `http://<name>.<namespace>.svc.<cluster-domain>:<port>`.
//
// Selectors are intentionally inclusive so a customer with a non-standard
// install (e.g. Thanos in front of Prometheus) is still autodetected. The
// agent's existing env-var path takes precedence — autodiscovery is only used
// when the relevant URL env (PROMETHEUS_URL / LOKI_URL / ALERTMANAGER_URL) is
// blank.
package svcdiscover

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Selectors is the ordered list of label selectors tried for each
// well-known service. We try them in order, returning the first hit.
var (
	// PrometheusSelectors mirrors PrometheusDiscovery.find_prometheus_url +
	// find_vm_url. Victoria Metrics selectors are tried
	// after the kube-prometheus stack hits so VM-only clusters are still
	// auto-detected.
	PrometheusSelectors = []string{
		"app=kube-prometheus-stack-prometheus",
		"app=prometheus,component=server,release!=kubecost",
		"app=prometheus-server",
		"app=prometheus-operator-prometheus",
		"app=prometheus-msteams",
		"app=rancher-monitoring-prometheus",
		"app=prometheus-prometheus",
		// prometheus-community/prometheus chart >= 25.x uses
		// app.kubernetes.io/name=prometheus + component=server labels.
		"app.kubernetes.io/name=prometheus,app.kubernetes.io/component=server",
		"app.kubernetes.io/component=query,app.kubernetes.io/name=thanos",
		"app.kubernetes.io/name=thanos-query",
		"app=thanos-query",
		"app=thanos-querier",
		// Victoria Metrics
		"app.kubernetes.io/name=vmsingle",
		"app.kubernetes.io/name=victoria-metrics-single",
		"app.kubernetes.io/name=vmselect",
		"app=vmselect",
	}
	// AlertManagerSelectors mirrors AlertManagerDiscovery.
	AlertManagerSelectors = []string{
		"app=kube-prometheus-stack-alertmanager",
		"app=prometheus,component=alertmanager",
		"app=prometheus-operator-alertmanager",
		"app=alertmanager",
		"app=rancher-monitoring-alertmanager",
		"app=prometheus-alertmanager",
		"operated-alertmanager=true",
		"app.kubernetes.io/name=alertmanager",
		"app.kubernetes.io/name=vmalertmanager",
	}
	// LokiSelectors mirrors GrafanaLokiDiscovery.
	LokiSelectors = []string{
		"app=loki",
		"app.kubernetes.io/instance=loki",
	}
	// OpencostSelectors mirrors OpenCostDiscovery.find_open_cost_url.
	OpencostSelectors = []string{
		"app=opencost",
		"app.kubernetes.io/name=opencost",
	}
)

// CacheTTL controls how long autodiscovery results (including misses) are
// cached. 1 hour by default.
const CacheTTL = time.Hour

// Discoverer finds service URLs by label, caching results.
type Discoverer struct {
	cs            kubernetes.Interface
	clusterDomain string

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	url     string
	expires time.Time
}

// New returns a Discoverer that uses the provided clientset. clusterDomain
// is appended after svc. — typically "cluster.local".
func New(cs kubernetes.Interface, clusterDomain string) *Discoverer {
	if clusterDomain == "" {
		clusterDomain = "cluster.local"
	}
	return &Discoverer{cs: cs, clusterDomain: clusterDomain, cache: map[string]cacheEntry{}}
}

// FindFirst tries each selector in order; returns the URL of the first match,
// or "" if none. Negative results are cached too so we don't keep listing
// services on every call.
func (d *Discoverer) FindFirst(ctx context.Context, selectors []string) string {
	return d.FindFirstPreferPort(ctx, selectors)
}

// FindFirstPreferPort is FindFirst with port preference: when a matched Service
// exposes multiple ports, the first port whose number appears in preferredPorts
// (in order) is used, falling back to the Service's first port. This matters for
// OpenCost, whose Service can expose both the cost-model API (9003, which serves
// /healthz) and a UI port (9090) — probing /healthz on the UI port reports an
// otherwise-healthy OpenCost as down. Results (including misses) are cached per
// (selectors, preferredPorts) for CacheTTL.
func (d *Discoverer) FindFirstPreferPort(ctx context.Context, selectors []string, preferredPorts ...int32) string {
	if d == nil || d.cs == nil {
		return ""
	}
	cacheKey := strings.Join(selectors, "|")
	for _, p := range preferredPorts {
		cacheKey += fmt.Sprintf("#%d", p)
	}
	now := time.Now()

	d.mu.Lock()
	if e, ok := d.cache[cacheKey]; ok && now.Before(e.expires) {
		d.mu.Unlock()
		return e.url
	}
	d.mu.Unlock()

	url := ""
	for _, sel := range selectors {
		if u := d.findOne(ctx, sel, preferredPorts); u != "" {
			url = u
			break
		}
	}

	d.mu.Lock()
	d.cache[cacheKey] = cacheEntry{url: url, expires: now.Add(CacheTTL)}
	d.mu.Unlock()
	return url
}

func (d *Discoverer) findOne(ctx context.Context, selector string, preferredPorts []int32) string {
	list, err := d.cs.CoreV1().Services("").List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil || len(list.Items) == 0 {
		return ""
	}
	svc := list.Items[0]
	if len(svc.Spec.Ports) == 0 {
		return ""
	}
	port := selectPort(svc.Spec.Ports, preferredPorts)
	return fmt.Sprintf("http://%s.%s.svc.%s:%d", svc.Name, svc.Namespace, d.clusterDomain, port)
}

// selectPort returns the first port whose number is in preferredPorts (in
// preference order); if none match — or preferredPorts is empty — it returns the
// Service's first port, preserving the original single-port behaviour.
func selectPort(ports []corev1.ServicePort, preferredPorts []int32) int32 {
	for _, want := range preferredPorts {
		for i := range ports {
			if ports[i].Port == want {
				return want
			}
		}
	}
	return ports[0].Port
}

// Coalesce returns the first non-empty value. Used at startup when wiring
// configured envs against autodiscovered URLs:
//
//	cfg.PrometheusURL = svcdiscover.Coalesce(cfg.PrometheusURL, d.FindFirst(ctx, svcdiscover.PrometheusSelectors))
func Coalesce(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
