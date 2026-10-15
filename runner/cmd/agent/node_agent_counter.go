package main

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// nodeAgentCounter counts the running node-agent pods that started before a
// given time — the pods the OTel collector scrapes into ClickHouse (same
// label, any namespace) — so the service map's ClickHouse source can tell
// when some of them are missing.
func nodeAgentCounter(cs kubernetes.Interface) func(context.Context, time.Time) (int, error) {
	return func(ctx context.Context, startedBefore time.Time) (int, error) {
		pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			LabelSelector: "app=nudgebee-node-agent",
			FieldSelector: "status.phase=Running",
		})
		if err != nil {
			return 0, err
		}
		n := 0
		for _, p := range pods.Items {
			if p.Status.StartTime != nil && p.Status.StartTime.Time.Before(startedBefore) {
				n++
			}
		}
		return n, nil
	}
}
