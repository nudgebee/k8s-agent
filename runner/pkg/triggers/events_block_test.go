package triggers

import (
	"context"
	"strings"
	"testing"
	"time"
)

// stubLister is the in-memory K8sEventsLister used in unit tests so we
// can exercise the engine's "fetch events for every match" behaviour
// without a real K8s client.
type stubLister struct {
	calls  []listCall
	events []K8sEvent
	err    error
}

type listCall struct{ namespace, kind, name string }

func (s *stubLister) ListEvents(_ context.Context, ns, kind, name string, _ int) ([]K8sEvent, error) {
	s.calls = append(s.calls, listCall{ns, kind, name})
	return s.events, s.err
}

// TestEngine_AppendsRecentEventsTablePerMatch is the headline guarantee:
// regardless of which matcher fires, the engine attaches a "Recent
// <Kind> events" table for the matched object so users see the
// kubelet-emitted reasons (BackOff / Killing / OOMKilling / FailedScheduling)
// in the same Finding card.
func TestEngine_AppendsRecentEventsTablePerMatch(t *testing.T) {
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod","ownerReferences":[{"kind":"ReplicaSet","name":"web","controller":true}]},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":5,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	stub := &stubLister{events: []K8sEvent{
		{Type: "Warning", Reason: "BackOff", Message: "Back-off restarting failed container", Count: 17,
			LastSeen: time.Now().Add(-30 * time.Second), Source: "kubelet"},
		{Type: "Warning", Reason: "Failed", Message: "Error: ImagePullBackOff", Count: 1,
			LastSeen: time.Now().Add(-2 * time.Minute), Source: "kubelet"},
	}}
	eng := NewEngine(Builtins(), time.Now().Add(-time.Hour)).WithEventsLister(stub)
	matches := eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: pod, OldObj: pod})
	if !contains(matchNames(matches), "pod_crash_loop") {
		t.Fatal("expected pod_crash_loop to fire")
	}
	if len(stub.calls) == 0 {
		t.Fatal("expected the engine to call ListEvents at least once for the matched Pod")
	}
	got := stub.calls[0]
	if got.namespace != "prod" || got.kind != "Pod" || got.name != "web-0" {
		t.Errorf("ListEvents called with (ns=%q,kind=%q,name=%q); want (prod,Pod,web-0)", got.namespace, got.kind, got.name)
	}
	// Confirm the events table block was appended to the match.
	var found bool
	for _, m := range matches {
		if m.Spec.Name != "pod_crash_loop" {
			continue
		}
		for _, b := range m.ExtraBlocks {
			if b["type"] != "table" {
				continue
			}
			data, _ := b["data"].(map[string]any)
			if name, _ := data["table_name"].(string); strings.HasPrefix(name, "Recent Pod events") {
				found = true
				rows, _ := data["rows"].([]any)
				if len(rows) != 2 {
					t.Errorf("rows = %d; want 2 (one per stub event)", len(rows))
				}
			}
		}
	}
	if !found {
		t.Error("matched Finding missing the 'Recent Pod events' table block")
	}
}

// TestEngine_AppendsNodeEventsTable: when the matched Pod has a node,
// the engine attaches a "Recent Node events" table so users see
// node-level issues (DiskPressure / NetworkUnavailable / kubelet
// warnings) alongside the primary finding.
func TestEngine_AppendsNodeEventsTable(t *testing.T) {
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"spec":{"nodeName":"gke-pool-a-9f4d"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":5,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	stub := &stubLister{events: []K8sEvent{
		{Type: "Warning", Reason: "BackOff", LastSeen: time.Now()},
	}}
	eng := NewEngine(Builtins(), time.Now().Add(-time.Hour)).WithEventsLister(stub)
	matches := eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: pod, OldObj: pod})
	if !contains(matchNames(matches), "pod_crash_loop") {
		t.Fatal("expected pod_crash_loop to fire")
	}
	var sawNodeCall bool
	for _, c := range stub.calls {
		if c.namespace == "" && c.kind == "Node" && c.name == "gke-pool-a-9f4d" {
			sawNodeCall = true
		}
	}
	if !sawNodeCall {
		t.Errorf("expected a ListEvents call with (ns=\"\",kind=\"Node\",name=\"gke-pool-a-9f4d\"); got %+v", stub.calls)
	}
	if !findTableByPrefix(matches, "pod_crash_loop", "Recent Node events on gke-pool-a-9f4d") {
		t.Error("matched Finding missing the 'Recent Node events on <node>' table block")
	}
}

// TestEngine_AppendsNamespaceEventsTable: every match also gets a
// "Recent events in namespace <ns>" table showing every recent event
// in the same namespace — sibling-pod failures, namespace-wide issues.
func TestEngine_AppendsNamespaceEventsTable(t *testing.T) {
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":5,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	stub := &stubLister{events: []K8sEvent{
		{Type: "Warning", Reason: "BackOff", LastSeen: time.Now()},
	}}
	eng := NewEngine(Builtins(), time.Now().Add(-time.Hour)).WithEventsLister(stub)
	matches := eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: pod, OldObj: pod})
	if !contains(matchNames(matches), "pod_crash_loop") {
		t.Fatal("expected pod_crash_loop to fire")
	}
	var sawNsCall bool
	for _, c := range stub.calls {
		if c.namespace == "prod" && c.kind == "" && c.name == "" {
			sawNsCall = true
		}
	}
	if !sawNsCall {
		t.Errorf("expected a ListEvents call with (ns=\"prod\",kind=\"\",name=\"\"); got %+v", stub.calls)
	}
	if !findTableByPrefix(matches, "pod_crash_loop", "Recent events in namespace prod") {
		t.Error("matched Finding missing the 'Recent events in namespace <ns>' table block")
	}
}

// TestEngine_SkipsNodeTableWhenNoNode: the node table is omitted for
// matchers whose subject doesn't run on a node (Deployment babysitter
// updates, Job failures pre-Pod-creation, etc.). Confirms we don't ship
// an empty "Recent Node events on" block.
func TestEngine_SkipsNodeTableWhenNoNode(t *testing.T) {
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":5,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	stub := &stubLister{events: []K8sEvent{{Reason: "BackOff", LastSeen: time.Now()}}}
	eng := NewEngine(Builtins(), time.Now().Add(-time.Hour)).WithEventsLister(stub)
	eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: pod, OldObj: pod})
	for _, c := range stub.calls {
		if c.kind == "Node" {
			t.Errorf("ListEvents must NOT be called with kind=Node when the Pod has no nodeName; got %+v", c)
		}
	}
}

// findTableByPrefix scans the given matcher's ExtraBlocks for a table
// whose table_name starts with the given prefix.
func findTableByPrefix(matches []Match, matcherName, tablePrefix string) bool {
	for _, m := range matches {
		if m.Spec.Name != matcherName {
			continue
		}
		for _, b := range m.ExtraBlocks {
			if b["type"] != "table" {
				continue
			}
			data, _ := b["data"].(map[string]any)
			if name, _ := data["table_name"].(string); strings.HasPrefix(name, tablePrefix) {
				return true
			}
		}
	}
	return false
}

// TestEngine_NoListerNoCrash: when the engine wasn't wired with a
// K8sEventsLister (unit tests, agent without K8s client), Match must
// still return the match — just without the events table block.
func TestEngine_NoListerNoCrash(t *testing.T) {
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":5,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	eng := NewEngine(Builtins(), time.Now().Add(-time.Hour)) // no lister
	matches := eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: pod, OldObj: pod})
	if !contains(matchNames(matches), "pod_crash_loop") {
		t.Fatal("expected pod_crash_loop to fire even without a lister")
	}
}
