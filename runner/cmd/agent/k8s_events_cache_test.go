package main

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/nudgebee/nudgebee-agent/pkg/triggers"
)

func testEvent(ns, name, kind, objName, reason string, last time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubelet"}},
			Annotations:   map[string]string{"a": "b"},
		},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: objName, Namespace: ns},
		Type:           corev1.EventTypeWarning,
		Reason:         reason,
		Message:        "  " + reason + " happened  ",
		Count:          3,
		Source:         corev1.EventSource{Component: "kubelet", Host: "node-1"},
		LastTimestamp:  metav1.NewTime(last),
	}
}

// liveStub records that a lookup fell through to the live lister.
type liveStub struct{ calls int }

func (s *liveStub) ListEvents(context.Context, string, string, string, int) ([]triggers.K8sEvent, error) {
	s.calls++
	return []triggers.K8sEvent{{Reason: "live"}}, nil
}

func startCachedLister(t *testing.T, objs ...*corev1.Event) (*cachedEventsLister, *liveStub) {
	t.Helper()
	cs := fake.NewSimpleClientset()
	for _, e := range objs {
		if _, err := cs.CoreV1().Events(e.Namespace).Create(context.Background(), e, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	live := &liveStub{}
	l, err := newCachedEventsLister(cs, live)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go l.Run(ctx)
	if !cache.WaitForCacheSync(ctx.Done(), l.inf.HasSynced) {
		t.Fatal("events cache never synced")
	}
	return l, live
}

func reasons(evs []triggers.K8sEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Reason)
	}
	return out
}

func TestCachedEventsLister_LookupShapes(t *testing.T) {
	now := time.Now()
	l, live := startCachedLister(t,
		testEvent("prod", "e1", "Pod", "web-0", "BackOff", now.Add(-1*time.Minute)),
		testEvent("prod", "e2", "Pod", "web-0", "Pulled", now.Add(-3*time.Minute)),
		testEvent("prod", "e3", "Pod", "web-1", "Killing", now.Add(-2*time.Minute)),
		testEvent("other", "e4", "Pod", "web-0", "Elsewhere", now),
		testEvent("default", "e5", "Node", "node-1", "NodeNotReady", now),
		testEvent("prod", "e6", "ConfigMap", "cfg", "Updated", now),
	)
	ctx := context.Background()

	// Subject: one object, its namespace only, newest first.
	got, err := l.ListEvents(ctx, "prod", "Pod", "web-0", 30)
	if err != nil {
		t.Fatal(err)
	}
	if r := strings.Join(reasons(got), ","); r != "BackOff,Pulled" {
		t.Errorf("subject events = %s; want BackOff,Pulled", r)
	}

	// Node: cluster-wide by involved object.
	got, _ = l.ListEvents(ctx, "", "Node", "node-1", 10)
	if r := strings.Join(reasons(got), ","); r != "NodeNotReady" {
		t.Errorf("node events = %s; want NodeNotReady", r)
	}

	// Namespace-wide, capped to the limit, newest first.
	got, _ = l.ListEvents(ctx, "prod", "", "", 2)
	if r := strings.Join(reasons(got), ","); r != "Updated,BackOff" {
		t.Errorf("namespace events = %s; want Updated,BackOff", r)
	}

	// kubewatch reports some kinds lower-case.
	got, _ = l.ListEvents(ctx, "prod", "configmap", "cfg", 10)
	if len(got) != 1 {
		t.Errorf("lower-case kind lookup = %v; want the ConfigMap event", reasons(got))
	}

	if live.calls != 0 {
		t.Errorf("live lister called %d times after sync; want 0", live.calls)
	}
}

func TestCachedEventsLister_TrimsCachedEvents(t *testing.T) {
	ev := testEvent("prod", "e1", "Pod", "web-0", "BackOff", time.Now())
	ev.Message = strings.Repeat("é", cachedEventMessageMax) // 2 bytes per rune
	l, _ := startCachedLister(t, ev)

	items := l.inf.GetStore().List()
	if len(items) != 1 {
		t.Fatalf("cache holds %d events; want 1", len(items))
	}
	got := items[0].(*corev1.Event)
	if len(got.ManagedFields) != 0 || len(got.Annotations) != 0 || got.Source.Host != "" {
		t.Errorf("cached event not trimmed: managedFields=%d annotations=%v host=%q",
			len(got.ManagedFields), got.Annotations, got.Source.Host)
	}
	if len(got.Message) > cachedEventMessageMax || !strings.HasPrefix(ev.Message, got.Message) {
		t.Errorf("message not truncated on a rune boundary: %d bytes", len(got.Message))
	}
	if got.Reason != "BackOff" || got.Count != 3 || got.Source.Component != "kubelet" || got.LastTimestamp.IsZero() {
		t.Errorf("trim dropped a displayed column: %+v", got)
	}
}

func TestCachedEventsLister_FallsBackUntilSynced(t *testing.T) {
	live := &liveStub{}
	l, err := newCachedEventsLister(fake.NewSimpleClientset(), live)
	if err != nil {
		t.Fatal(err)
	}
	// Not started: never synced.
	got, _ := l.ListEvents(context.Background(), "prod", "Pod", "web-0", 30)
	if live.calls != 1 || len(got) != 1 || got[0].Reason != "live" {
		t.Errorf("unsynced lookup = %v (live calls %d); want the live lister's answer", reasons(got), live.calls)
	}
}
