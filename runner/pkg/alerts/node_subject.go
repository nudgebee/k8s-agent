package alerts

import (
	"context"
	"net"
	"strings"
)

// NodeLocator resolves the node a per-node exporter's series came from. The
// implementation lives in cmd/agent (a typed clientset wrapper) so this package
// stays free of a kubernetes dependency, matching triggers.K8sEventsLister.
//
// Both methods return "" when they cannot answer — an unresolved node leaves
// the alert's subject exactly as it arrived rather than half-rewritten.
type NodeLocator interface {
	// NodeForPod returns the node a pod is scheduled on.
	NodeForPod(ctx context.Context, namespace, podName string) string
	// NodeForIP returns the node whose internal address is ip.
	NodeForIP(ctx context.Context, ip string) string
}

// nodeScopedExporterJobs are prometheus `job` values whose targets are one per
// node, so every sample describes the node rather than the exporter that
// reported it.
var nodeScopedExporterJobs = map[string]bool{
	"node-exporter": true,
	"node_exporter": true,
}

// isNodeScopedExporterJob reports whether a `job` label names a per-node
// exporter. Substring-tolerant because installs helm-prefix the job name
// ("victoria-prometheus-node-exporter").
//
// This is the mirror of the guard already here for the `job`-as-subject case
// (isScrapeExporter): there we refuse to let an exporter's name BE the subject;
// here we use the fact that it is a per-node exporter to find the real one.
func isNodeScopedExporterJob(job string) bool {
	lower := strings.ToLower(strings.TrimSpace(job))
	if lower == "" {
		return false
	}
	if nodeScopedExporterJobs[lower] {
		return true
	}
	return strings.Contains(lower, "node-exporter") || strings.Contains(lower, "node_exporter")
}

// nodeNameFromLabels returns a node name the alert already carries. `instance`
// is excluded on purpose: prometheus writes it as `<host>:<port>`, a scrape
// target rather than a node name. hostFromInstance translates that separately.
func nodeNameFromLabels(labels map[string]string) string {
	for _, key := range []string{"node", "kubernetes_node", "instance_node"} {
		if v := strings.TrimSpace(labels[key]); v != "" {
			return v
		}
	}
	return ""
}

// hostFromInstance strips the port from a prometheus `instance` label.
// net.SplitHostPort rather than cutting at the first colon, because an IPv6
// target is "[fe80::1]:9100" and a bare IPv6 address is all colons.
func hostFromInstance(instance string) string {
	instance = strings.TrimSpace(instance)
	if instance == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(instance); err == nil {
		return host
	}
	return instance
}

// resolveNodeSubject returns the node a node-scoped alert is about, or "" to
// leave the alert's subject alone.
//
// Node-exporter series carry the exporter's own `pod`, `namespace` and
// `daemonset` labels, so alertSubject resolves them to the exporter — a
// NodeSystemSaturation lands on `victoria-prometheus-node-exporter-skqnn`
// instead of the saturated node. The `job` label is what tells the two apart:
// a per-node exporter's series is about a node, whatever else rode along.
//
// Resolution is most-reliable-first:
//  1. a `node` label the rule kept;
//  2. the exporter pod -> its node. The label being rejected as the subject is
//     the best pointer to the right answer: that pod runs on the node the
//     metric describes;
//  3. the `instance` host -> the node with that address.
//
// The locator may be nil (no typed k8s client at boot); then only step 1 can
// answer, and everything else keeps today's behaviour. The backend applies the
// same correction on ingest, so an agent that cannot resolve here is covered
// there.
func resolveNodeSubject(ctx context.Context, labels map[string]string, locator NodeLocator) string {
	if len(labels) == 0 {
		return ""
	}
	if !isNodeScopedExporterJob(labels["job"]) {
		return ""
	}
	if node := nodeNameFromLabels(labels); node != "" {
		return node
	}
	if locator == nil {
		return ""
	}
	if pod := strings.TrimSpace(labels["pod"]); pod != "" {
		namespace := strings.TrimSpace(labels["namespace"])
		if node := locator.NodeForPod(ctx, namespace, pod); node != "" {
			return node
		}
	}
	if host := hostFromInstance(labels["instance"]); host != "" {
		if node := locator.NodeForIP(ctx, host); node != "" {
			return node
		}
	}
	return ""
}
