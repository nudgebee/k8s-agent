package main

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/nudgebee/nudgebee-agent/pkg/triggers"
)

// k8sEventsLister wraps a typed clientset to satisfy
// triggers.K8sEventsLister. Used by the engine to attach three event
// tables to every matched Finding (subject / node / namespace).
//
// Implementation notes:
//   - Builds an involvedObject field selector from non-empty kind/name.
//     Subject queries pass both; node queries pass kind="Node" + name
//     with namespace=""; namespace-wide queries pass kind="" + name=""
//     with a namespace.
//   - Refuses the unbounded case (namespace="" && kind="" && name=="").
//   - Caps the result client-side to `limit` rows. We sort by
//     LastTimestamp/EventTime descending so the most-recent kubelet
//     errors land on top.
//   - K8s has two Event APIs (v1 core + events.k8s.io/v1). They share
//     storage but the new API exposes richer fields (series, reportingController).
//     For our use-case (showing the most recent BackOff / OOMKilling /
//     etc. with timestamp + reason + message), the legacy API is
//     sufficient and supported on every cluster.
type k8sEventsLister struct {
	cs kubernetes.Interface
}

func newK8sEventsLister(cs kubernetes.Interface) triggers.K8sEventsLister {
	return &k8sEventsLister{cs: cs}
}

func (l *k8sEventsLister) ListEvents(ctx context.Context, namespace, kind, name string, limit int) ([]triggers.K8sEvent, error) {
	if l.cs == nil {
		return nil, nil
	}
	if namespace == "" && kind == "" && name == "" {
		return nil, nil
	}
	var parts []string
	if kind != "" {
		parts = append(parts, "involvedObject.kind="+kind)
	}
	if name != "" {
		parts = append(parts, "involvedObject.name="+name)
	}
	opts := metav1.ListOptions{
		// Pull a generous chunk; we sort + cap client-side.
		Limit: int64(limit * 2),
	}
	if len(parts) > 0 {
		opts.FieldSelector = strings.Join(parts, ",")
	}
	list, err := l.cs.CoreV1().Events(namespace).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	events := make([]triggers.K8sEvent, 0, len(list.Items))
	for i := range list.Items {
		events = append(events, toAgentEvent(&list.Items[i]))
	}
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// toAgentEvent extracts the columns the UI displays. Falls back across
// the various timestamp fields K8s carries (LastTimestamp / EventTime /
// FirstTimestamp / Series.LastObservedTime) — older kubelets fill
// LastTimestamp; newer event watchers prefer EventTime + Series.
func toAgentEvent(e *corev1.Event) triggers.K8sEvent {
	out := triggers.K8sEvent{
		Type:    e.Type,
		Reason:  e.Reason,
		Message: strings.TrimSpace(e.Message),
		Count:   e.Count,
		Source:  e.Source.Component,
	}
	if out.Source == "" {
		out.Source = e.ReportingController
	}
	out.FirstSeen = pickEventTime(e.FirstTimestamp.Time, e.EventTime.Time, e.CreationTimestamp.Time)
	out.LastSeen = pickEventTime(e.LastTimestamp.Time, e.EventTime.Time, e.CreationTimestamp.Time)
	if e.Series != nil {
		// The new event API records repeats in Series; LastObservedTime
		// is the most up-to-date timestamp.
		if !e.Series.LastObservedTime.Time.IsZero() {
			out.LastSeen = e.Series.LastObservedTime.Time
		}
		if e.Series.Count > 0 {
			out.Count = e.Series.Count
		}
	}
	return out
}

// pickEventTime returns the first non-zero timestamp from the candidates.
// Different K8s API versions / event paths populate different fields.
func pickEventTime(candidates ...time.Time) time.Time {
	for _, t := range candidates {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}
