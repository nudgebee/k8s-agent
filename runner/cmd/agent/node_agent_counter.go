package main

import (
	"context"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// nodeAgentCountTTL is how long the node-agent pod list is reused, so service
// map builds do not each send a cluster-wide pod LIST to the API server.
const nodeAgentCountTTL = time.Minute

// nodeAgentCounter counts the running node-agent pods that started before a
// given time — the pods the OTel collector scrapes into ClickHouse (same
// label, any namespace) — so the service map's ClickHouse source can tell
// when some of them are missing.
func nodeAgentCounter(cs kubernetes.Interface) func(context.Context, time.Time) (int, error) {
	var (
		mu      sync.Mutex
		started []time.Time
		listed  time.Time
	)
	return func(ctx context.Context, startedBefore time.Time) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		if listed.IsZero() || time.Since(listed) >= nodeAgentCountTTL {
			pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{
				LabelSelector: "app=nudgebee-node-agent",
				FieldSelector: "status.phase=Running",
			})
			if err != nil {
				return 0, err
			}
			started = started[:0]
			for _, p := range pods.Items {
				if p.Status.StartTime != nil {
					started = append(started, p.Status.StartTime.Time)
				}
			}
			listed = time.Now()
		}
		n := 0
		for _, t := range started {
			if t.Before(startedBefore) {
				n++
			}
		}
		return n, nil
	}
}
