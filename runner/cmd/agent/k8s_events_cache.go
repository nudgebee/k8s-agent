package main

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/nudgebee/nudgebee-agent/pkg/triggers"
)

// cachedEventsLister serves the evidence tables on a matched Finding from a
// shared Event informer instead of three API List calls per match.
//
// Under the live lister, N simultaneous failures cost 3N Lists against the
// API server, at the moment it is busiest, and the namespace-wide table had
// no field selector: served from the watch cache (ResourceVersion "0"), which
// ignores Limit, it returned every event in the namespace for each match. The
// informer is one watch, and a lookup is an in-memory index read.
//
// Memory: events are trimmed to the columns the table shows before they enter
// the cache (no managedFields, annotations, or long messages), so the cache
// costs a few hundred bytes per live event. Kubernetes expires events after an
// hour by default, which bounds the count.
type cachedEventsLister struct {
	inf  cache.SharedIndexInformer
	live triggers.K8sEventsLister // serves lookups until the informer has synced
}

const (
	// eventsByInvolvedIndex indexes events by lower(involvedObject.kind) +
	// "/" + involvedObject.name. Kind is folded because kubewatch reports
	// some kinds lower-case ("configmap"), which the case-sensitive field
	// selector of the live lister never matched.
	eventsByInvolvedIndex = "involved"
	// cachedEventMessageMax caps a cached event's message. The table shows
	// 200 characters.
	cachedEventMessageMax = 512
)

func involvedKey(kind, name string) string {
	return strings.ToLower(kind) + "/" + name
}

// newCachedEventsLister builds the informer; call Run to start it.
func newCachedEventsLister(cs kubernetes.Interface, live triggers.K8sEventsLister) (*cachedEventsLister, error) {
	inf := informers.NewSharedInformerFactory(cs, 0).Core().V1().Events().Informer()
	if err := inf.SetTransform(trimEvent); err != nil {
		return nil, err
	}
	// The factory informer already indexes by namespace (cache.NamespaceIndex).
	if err := inf.AddIndexers(cache.Indexers{
		eventsByInvolvedIndex: func(obj any) ([]string, error) {
			e, ok := obj.(*corev1.Event)
			if !ok {
				return nil, nil
			}
			return []string{involvedKey(e.InvolvedObject.Kind, e.InvolvedObject.Name)}, nil
		},
	}); err != nil {
		return nil, err
	}
	return &cachedEventsLister{inf: inf, live: live}, nil
}

// Run starts the informer and blocks until ctx is done.
func (l *cachedEventsLister) Run(ctx context.Context) {
	l.inf.Run(ctx.Done())
}

func (l *cachedEventsLister) ListEvents(ctx context.Context, namespace, kind, name string, limit int) ([]triggers.K8sEvent, error) {
	if !l.inf.HasSynced() {
		return l.live.ListEvents(ctx, namespace, kind, name, limit)
	}
	var (
		objs []any
		err  error
	)
	switch {
	case kind != "" && name != "":
		objs, err = l.inf.GetIndexer().ByIndex(eventsByInvolvedIndex, involvedKey(kind, name))
	case kind == "" && name == "" && namespace != "":
		objs, err = l.inf.GetIndexer().ByIndex(cache.NamespaceIndex, namespace)
	default:
		// Not a shape the engine asks for; keep the live behaviour.
		return l.live.ListEvents(ctx, namespace, kind, name, limit)
	}
	if err != nil {
		return nil, err
	}
	events := make([]triggers.K8sEvent, 0, len(objs))
	for _, o := range objs {
		e, ok := o.(*corev1.Event)
		if !ok {
			continue
		}
		// A subject lookup is scoped to the subject's namespace; a Node
		// lookup passes "" and takes events from every namespace.
		if namespace != "" && e.Namespace != namespace {
			continue
		}
		events = append(events, toAgentEvent(e))
	}
	sort.Slice(events, func(i, j int) bool {
		return events[i].LastSeen.After(events[j].LastSeen)
	})
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// trimEvent keeps only what toAgentEvent and the indexes read. Runs before an
// event enters the cache, so the full object is never retained.
func trimEvent(obj any) (any, error) {
	e, ok := obj.(*corev1.Event)
	if !ok {
		// Tombstones (cache.DeletedFinalStateUnknown) pass through as is.
		return obj, nil
	}
	out := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:              e.Name,
			Namespace:         e.Namespace,
			UID:               e.UID,
			ResourceVersion:   e.ResourceVersion,
			CreationTimestamp: e.CreationTimestamp,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      e.InvolvedObject.Kind,
			Namespace: e.InvolvedObject.Namespace,
			Name:      e.InvolvedObject.Name,
		},
		Type:                e.Type,
		Reason:              e.Reason,
		Message:             truncateUTF8(strings.TrimSpace(e.Message), cachedEventMessageMax),
		Count:               e.Count,
		Source:              corev1.EventSource{Component: e.Source.Component},
		ReportingController: e.ReportingController,
		FirstTimestamp:      e.FirstTimestamp,
		LastTimestamp:       e.LastTimestamp,
		EventTime:           e.EventTime,
	}
	if e.Series != nil {
		out.Series = &corev1.EventSeries{Count: e.Series.Count, LastObservedTime: e.Series.LastObservedTime}
	}
	return out, nil
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
