package main

import (
	"context"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/nudgebee/nudgebee-agent/pkg/alerts"
)

// nodeLocator wraps a typed clientset to satisfy alerts.NodeLocator. It answers
// "which node is this node-exporter series about" for the alert forwarder.
//
// Implementation notes:
//   - NodeForPod is a single GET on the pod; node-exporter pods are long-lived
//     DaemonSet members, so this is both cheap and exact.
//   - NodeForIP has to list nodes, which is the expensive one, so the address
//     to name map is cached for nodeIPCacheTTL. A miss forces one refresh, and
//     that refresh is serialized: alerts arrive on a goroutine pool, so a burst
//     for an address the cache has never seen would otherwise have every
//     goroutine list concurrently — one API call per alert. Callers that wait
//     read the result rather than listing again.
//   - Every method returns "" rather than an error: the caller's contract is
//     that an unresolved node leaves the alert untouched, and an alert is not
//     worth failing over a lookup.
type nodeLocator struct {
	cs kubernetes.Interface

	// refreshMu serializes the node LIST itself. Alerts are handled on a
	// goroutine pool, so without it a burst for an address we have never seen
	// has every goroutine miss the cache and list concurrently — one call per
	// alert, which is the opposite of what the cache is for.
	refreshMu sync.Mutex

	// mu guards the map below and is only ever held around it, never across
	// the API call.
	mu        sync.Mutex
	ipToNode  map[string]string
	cachedAt  time.Time
	refreshed bool
}

// nodeIPCacheTTL bounds how stale the address map may be. Nodes join and leave
// on autoscaling, and a recycled address pointing at the wrong node is worse
// than no answer, so this is deliberately short.
const nodeIPCacheTTL = 2 * time.Minute

func newNodeLocator(cs kubernetes.Interface) alerts.NodeLocator {
	return &nodeLocator{cs: cs}
}

func (l *nodeLocator) NodeForPod(ctx context.Context, namespace, podName string) string {
	if l.cs == nil || strings.TrimSpace(podName) == "" || strings.TrimSpace(namespace) == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	pod, err := l.cs.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil || pod == nil {
		return ""
	}
	return pod.Spec.NodeName
}

func (l *nodeLocator) NodeForIP(ctx context.Context, ip string) string {
	if l.cs == nil || strings.TrimSpace(ip) == "" {
		return ""
	}
	if node := l.lookupIP(ip, false); node != "" {
		return node
	}
	// Miss: refresh once, then answer from the fresh map.
	if !l.refreshIPs(ctx) {
		return ""
	}
	return l.lookupIP(ip, true)
}

// lookupIP reads the cached map. When afterRefresh is false an expired cache
// counts as a miss, so the caller refreshes.
func (l *nodeLocator) lookupIP(ip string, afterRefresh bool) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ipToNode == nil {
		return ""
	}
	if !afterRefresh && time.Since(l.cachedAt) > nodeIPCacheTTL {
		return ""
	}
	return l.ipToNode[ip]
}

// refreshIPs rebuilds the address map, at most one LIST at a time. Returns
// true when the map is fresh afterwards — including when another goroutine did
// the work while this one waited for the lock, since that result is exactly
// what this call would have fetched.
//
// A caller whose address is still missing from a fresh map gets "" from the
// lookup that follows; it does not list again, which is what keeps a storm of
// alerts for an unknown address from listing nodes per alert.
func (l *nodeLocator) refreshIPs(ctx context.Context) bool {
	l.refreshMu.Lock()
	defer l.refreshMu.Unlock()

	l.mu.Lock()
	alreadyFresh := l.refreshed && time.Since(l.cachedAt) <= nodeIPCacheTTL
	l.mu.Unlock()
	if alreadyFresh {
		return true
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// ResourceVersion="0" serves the LIST from the apiserver watch cache rather
	// than a quorum read from etcd, matching how k8sEventsLister lists.
	nodes, err := l.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{ResourceVersion: "0"})
	if err != nil || nodes == nil {
		return false
	}
	fresh := make(map[string]string, len(nodes.Items))
	for i := range nodes.Items {
		node := &nodes.Items[i]
		for _, addr := range node.Status.Addresses {
			switch addr.Type {
			case corev1.NodeInternalIP, corev1.NodeExternalIP, corev1.NodeHostName:
				if addr.Address != "" {
					fresh[addr.Address] = node.Name
				}
			}
		}
	}

	l.mu.Lock()
	l.ipToNode = fresh
	l.cachedAt = time.Now()
	l.refreshed = true
	l.mu.Unlock()
	return true
}
