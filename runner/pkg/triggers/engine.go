package triggers

import (
	"time"
)

// DefaultGraceWindow is the cushion subtracted from agentStartTime to
// decide whether an obj is "old enough to be a resync re-fire". Plan-
// agent recommended 120s — the legacy runner got this implicitly via
// informer resync semantics; we have to add it explicitly because we
// run matchers on every kubewatch UPDATE including resync.
const DefaultGraceWindow = 2 * time.Minute

// Engine ties matchers + rate-limiter + restart grace-window into one
// dispatch loop. One Engine per agent process; concurrent-safe (the
// rate-limiter has its own mutex; spec list is read-only after build).
type Engine struct {
	specs           []MatcherSpec
	rl              *RateLimiter
	startTime       time.Time
	graceWindow     time.Duration
	now             func() time.Time      // injectable for tests
	eventsLister    K8sEventsLister       // optional; threaded into EnrichBlocks
	serviceBackends ServiceBackendsLister // optional; threaded into PredicateCtx + EnrichBlocks
}

// NewEngine builds an Engine with the given specs. agentStartTime should
// be `time.Now()` at process start — used by the resync grace window.
func NewEngine(specs []MatcherSpec, agentStartTime time.Time) *Engine {
	return &Engine{
		specs:       specs,
		rl:          NewRateLimiter(0),
		startTime:   agentStartTime,
		graceWindow: DefaultGraceWindow,
		now:         time.Now,
	}
}

// WithEventsLister returns the engine wired with a K8s events lister.
// Call once at boot from main.go after building the typed clientset.
// Once set, every successful match gets a "Recent <Kind> events" table
// appended automatically (engine-side default evidence) and EnrichBlocks
// receives a non-nil EventsLister via EnrichContext for matcher-specific
// uses.
func (e *Engine) WithEventsLister(l K8sEventsLister) *Engine {
	e.eventsLister = l
	return e
}

// WithServiceBackendsLister returns the engine wired with a Service-
// backends lister. Call once at boot from main.go after building the
// typed clientset. PredicateCtx matchers (service_no_endpoints) receive
// it via EnrichContext; without it they never fire.
func (e *Engine) WithServiceBackendsLister(l ServiceBackendsLister) *Engine {
	e.serviceBackends = l
	return e
}

// enrichContext builds the per-event context threaded into PredicateCtx
// and EnrichBlocks. Fields are nil when the corresponding lister wasn't
// wired at boot (unit tests / no K8s client).
func (e *Engine) enrichContext() EnrichContext {
	return EnrichContext{
		EventsLister:    e.eventsLister,
		ServiceBackends: e.serviceBackends,
	}
}

// fetchSubjectEvents builds a "Recent <Kind> events" table for the
// matched object. Skipped when the lister is unwired (unit tests) or
// when the namespace/name pair can't be resolved.
//
// Centralizing this in the engine — rather than per-matcher EnrichBlocks
// — lets every matcher (current + future) inherit the evidence without
// repeating the same recentEventsTable call.
func (e *Engine) fetchSubjectEvents(kind, namespace, name string) []EvidenceBlock {
	if e.eventsLister == nil || namespace == "" || name == "" || kind == "" {
		return nil
	}
	return recentEventsTable(EnrichContext{EventsLister: e.eventsLister},
		namespace, kind, name, "Recent "+kind+" events", recentEventsLimit)
}

// fetchNodeEvents builds a "Recent Node events on <node>" table for the
// node the matched subject runs on. Surfaces node-level problems
// (DiskPressure, NetworkUnavailable, kubelet warnings) alongside the
// primary finding so users see "is the node sick?" without leaving the
// card. Skipped when the subject has no resolvable node.
func (e *Engine) fetchNodeEvents(node string) []EvidenceBlock {
	if e.eventsLister == nil || node == "" {
		return nil
	}
	return recentEventsTable(EnrichContext{EventsLister: e.eventsLister},
		"", "Node", node, "Recent Node events on "+node, supplementaryEventsLimit)
}

// fetchNamespaceEvents builds a "Recent events in namespace <ns>" table
// listing every recent event in the namespace (no involvedObject
// filter). Surfaces sibling-pod failures and namespace-wide issues.
func (e *Engine) fetchNamespaceEvents(namespace string) []EvidenceBlock {
	if e.eventsLister == nil || namespace == "" {
		return nil
	}
	return recentEventsTable(EnrichContext{EventsLister: e.eventsLister},
		namespace, "", "", "Recent events in namespace "+namespace, supplementaryEventsLimit)
}

// Match runs every matcher against the event and returns one Match per
// fired trigger. A single event can fire several matchers (a Pod can be
// both ImagePullBackOff and CrashLoopBackOff).
//
// Filtering order per matcher:
//
//  1. Kind filter        (cheap, drops most events fast)
//  2. Operation filter   (cheap)
//  3. Predicate          (does the actual K8s field walking)
//  4. Resync suppression (creationTimestamp older than start+grace)
//  5. Rate limit         (fingerprint seen recently?)
//
// Order matters: the rate-limiter shouldn't be touched for events that
// don't satisfy the predicate, otherwise the LRU fills with garbage
// keys from non-matching events.
func (e *Engine) Match(ev IncomingK8sEvent) []Match {
	if ev.Obj == nil {
		return nil
	}
	matches := make([]Match, 0, 1)
	ec := e.enrichContext()
	for i := range e.specs {
		spec := &e.specs[i]
		if !kindMatches(spec.Kind, ev.Kind) {
			continue
		}
		if !operationMatches(spec.Operations, ev.Operation) {
			continue
		}
		// PredicateCtx (cluster-read matchers) takes precedence over the
		// pure Predicate. A spec with neither never fires.
		if spec.PredicateCtx != nil {
			if !spec.PredicateCtx(ev.Obj, ev.OldObj, ec) {
				continue
			}
		} else if spec.Predicate == nil || !spec.Predicate(ev.Obj, ev.OldObj) {
			continue
		}
		if spec.SuppressOnResync && e.suppressedByResync(ev.Obj) {
			continue
		}
		fingerprint := ""
		if spec.FingerprintFn != nil {
			fingerprint = spec.FingerprintFn(ev.Obj)
		}
		if !e.rl.Allow(spec.Name+":"+fingerprint, spec.RateLimit) {
			continue
		}

		name, namespace, lowerKind, node := SubjectFromObj(ev.Kind, ev.Obj)
		var extra []EvidenceBlock
		if spec.EnrichBlocks != nil {
			extra = spec.EnrichBlocks(ev.Obj, ev.OldObj, ec)
		}
		// Default evidence: three K8s events tables per match —
		//   1. subject events (Pod / Deployment / Job / ...)
		//   2. node events (when the subject runs on a node)
		//   3. namespace events (every recent event in the namespace)
		//
		// The node + namespace tables surface adjacent problems alongside
		// the primary finding (sick node, sibling-pod failures) so users
		// don't have to leave the Finding card to triage. All three are
		// skipped when the lister isn't wired (unit tests / no K8s client).
		extra = append(extra, e.fetchSubjectEvents(ev.Kind, namespace, name)...)
		extra = append(extra, e.fetchNodeEvents(node)...)
		extra = append(extra, e.fetchNamespaceEvents(namespace)...)
		matches = append(matches, Match{
			Spec:             spec,
			Fingerprint:      fingerprint,
			Owner:            ResolveOwner(ev.Obj),
			SubjectName:      name,
			SubjectNamespace: namespace,
			SubjectKind:      lowerKind,
			SubjectNode:      node,
			ExtraBlocks:      extra,
		})
	}
	return matches
}

// suppressedByResync returns true when the obj's creationTimestamp is
// far enough in the past that this event is almost certainly a kubewatch
// resync of an already-known object — not a fresh state change. Without
// this, every helm rollout fires a Finding for every Pod currently in a
// bad state.
func (e *Engine) suppressedByResync(obj map[string]any) bool {
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		return false
	}
	cts, _ := meta["creationTimestamp"].(string)
	if cts == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, cts)
	if err != nil {
		return false // non-fatal; just don't suppress
	}
	// Suppress when the object is older than (agentStart - graceWindow).
	// i.e., the obj existed before the agent started + 2 min.
	return t.Before(e.startTime.Add(-e.graceWindow))
}

func kindMatches(specKind, eventKind string) bool {
	if specKind == "" || specKind == "Any" {
		return true
	}
	return specKind == eventKind
}

func operationMatches(specOps []string, eventOp string) bool {
	if len(specOps) == 0 {
		return true
	}
	for _, op := range specOps {
		if op == eventOp {
			return true
		}
	}
	return false
}
