package triggers

import (
	"fmt"
	"log/slog"
	"strings"
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
	churn           *ChurnSuppressor      // frequency-based suppression for SuppressChurn matchers
	crChurn         *ChurnSuppressor      // the same for custom-resource matchers (APIGroup set), with their own thresholds
	logger          *slog.Logger          // optional; used to report churn classifications
}

// NewEngine builds an Engine with the given specs. agentStartTime should
// be `time.Now()` at process start — used by the resync grace window.
func NewEngine(specs []MatcherSpec, agentStartTime time.Time) *Engine {
	return &Engine{
		specs: specs,
		rl:    NewRateLimiter(0),
		churn: NewChurnSuppressor(0, 0, 0, 0),
		crChurn: NewChurnSuppressor(0, CustomResourceChurnThreshold,
			CustomResourceChurnWindow, CustomResourceChurnCooldown).MuteAtThreshold(),
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

// WithLogger returns the engine wired with a logger, used to announce when a
// resource is classified as churning. Optional: without it the suppression
// still happens, it just is not announced.
func (e *Engine) WithLogger(l *slog.Logger) *Engine {
	e.logger = l
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
func (e *Engine) fetchSubjectEvents(apiGroup, kind, namespace, name string) []EvidenceBlock {
	if e.eventsLister == nil || namespace == "" || name == "" || kind == "" {
		return nil
	}
	return recentEventsTable(EnrichContext{EventsLister: e.eventsLister},
		namespace, apiGroup, kind, name, "Recent "+kind+" events", recentEventsLimit)
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
		"", "", "Node", node, "Recent Node events on "+node, supplementaryEventsLimit)
}

// fetchNamespaceEvents builds a "Recent events in namespace <ns>" table
// listing every recent event in the namespace (no involvedObject
// filter). Surfaces sibling-pod failures and namespace-wide issues.
func (e *Engine) fetchNamespaceEvents(namespace string) []EvidenceBlock {
	if e.eventsLister == nil || namespace == "" {
		return nil
	}
	return recentEventsTable(EnrichContext{EventsLister: e.eventsLister},
		namespace, "", "", "", "Recent events in namespace "+namespace, supplementaryEventsLimit)
}

// Match runs every matcher against the event and returns one Match per
// fired trigger. A single event can fire several matchers (a Pod can be
// both ImagePullBackOff and CrashLoopBackOff).
//
// Filtering order per matcher:
//
//  1. Kind filter         (cheap, drops most events fast)
//  2. Operation filter    (cheap)
//  3. Recovery predicate  (healthy again → clear suppression, never fires)
//  4. Predicate           (does the actual K8s field walking)
//  5. Resync suppression  (creationTimestamp older than start+grace)
//  6. Rate limit          (fingerprint seen recently?)
//
// Order matters: the rate-limiter shouldn't be touched for events that
// don't satisfy the predicate, otherwise the LRU fills with garbage
// keys from non-matching events. The recovery check is the one exception
// — it touches the limiter for non-matching events by design — but it
// only ever removes keys, so it can't grow the LRU.
func (e *Engine) Match(ev IncomingK8sEvent) []Match {
	if ev.Obj == nil {
		return nil
	}
	if AgentOwned(ev.Obj) {
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
		// Recovery runs before the failure predicate: the two are
		// mutually exclusive, and a recovered object is the cheaper,
		// far more common case on a healthy cluster. Clearing the
		// limiter record here is what lets a workload that recovers and
		// breaks again inside the window get a second Finding instead
		// of being suppressed as a repeat of the first.
		if spec.RecoveryPredicate != nil && spec.RecoveryPredicate(ev.Obj, ev.OldObj) {
			if spec.FingerprintFn != nil {
				e.rl.Forget(spec.Name + ":" + spec.FingerprintFn(ev.Obj))
			}
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
		var muteNote EvidenceBlock
		if spec.SuppressChurn {
			churn := e.churn
			if spec.APIGroup != "" {
				churn = e.crChurn
			}
			d := churn.AllowDetail(spec.Name + ":" + fingerprint)
			if d.Classified && e.logger != nil {
				// Announced once per cooldown. A resource that silently stops
				// producing events looks identical to one that stopped
				// changing, which is an expensive ambiguity mid-incident.
				name, namespace, _, _ := SubjectFromObj(ev.Kind, ev.Obj)
				e.logger.Info("change suppressed: resource rewrites itself continuously",
					"matcher", spec.Name, "namespace", namespace, "name", name,
					"threshold", churn.threshold, "window", churn.window,
					"cooldown", churn.cooldown)
			}
			if !d.Allowed {
				continue
			}
			if d.Classified {
				// The mute started on this fire (custom resources), so the
				// Finding itself says so: the log line above is not where
				// anyone looking at the change history will look.
				muteNote = EvidenceBlock{
					"type": "markdown",
					"data": fmt.Sprintf("%d changes in %s: further changes to this object are not reported until %s.",
						churn.threshold, churn.window, d.MutedUntil.UTC().Format(time.RFC3339)),
				}
			}
		}

		name, namespace, lowerKind, node := SubjectFromObj(ev.Kind, ev.Obj)
		var extra []EvidenceBlock
		if spec.EnrichBlocks != nil {
			extra = spec.EnrichBlocks(ev.Obj, ev.OldObj, ec)
		}
		if muteNote != nil {
			extra = append(extra, muteNote)
		}
		// A custom resource is looked up by its bare kind within its group;
		// a built-in kind by its kind alone.
		eventsKind := ev.Kind
		if spec.SubjectKind != "" {
			eventsKind = spec.SubjectKind
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
		extra = append(extra, e.fetchSubjectEvents(spec.APIGroup, eventsKind, namespace, name)...)
		extra = append(extra, e.fetchNodeEvents(node)...)
		extra = append(extra, e.fetchNamespaceEvents(namespace)...)
		occurrenceKey := ""
		if spec.OccurrenceKeyFn != nil {
			occurrenceKey = spec.OccurrenceKeyFn(ev.Obj)
		}
		m := Match{
			Spec:             spec,
			Fingerprint:      fingerprint,
			OccurrenceKey:    occurrenceKey,
			Owner:            ResolveOwner(ev.Obj),
			SubjectName:      name,
			SubjectNamespace: namespace,
			SubjectKind:      lowerKind,
			SubjectNode:      node,
			ExtraBlocks:      extra,
		}
		if spec.APIGroup != "" {
			// Unattributed: no owner, so neither the subject owner nor the
			// service key can tie the change to a workload.
			m.Owner = OwnerRef{}
			m.SubjectDisplayKind = spec.SubjectKind
			m.Unattributed = true
		}
		matches = append(matches, m)
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

// HandlesKind reports whether any matcher could fire on an event of this
// kind. The intake uses it to drop kinds no matcher reads before it pays
// for decoding the objects or queueing them.
func (e *Engine) HandlesKind(kind string) bool {
	for i := range e.specs {
		if kindMatches(e.specs[i].Kind, kind) {
			return true
		}
	}
	return false
}

// Handles reports whether any matcher reads changes of this kind and
// operation. The runner's own watch uses it to skip encoding changes nothing
// would match: every built-in matcher reads updates only.
func (e *Engine) Handles(kind, operation string) bool {
	for i := range e.specs {
		if kindMatches(e.specs[i].Kind, kind) && operationMatches(e.specs[i].Operations, operation) {
			return true
		}
	}
	return false
}

// kindMatches compares a spec's Kind against the kind kubewatch reported.
// Case-insensitive: kubewatch's per-resource handlers are not consistent
// about casing across kinds ("Deployment" but "configmap"), and a spec
// that silently never fires because of a capital letter is invisible —
// the events just land in the unmatched counter. No two K8s kinds differ
// only by case, so folding cannot collide.
func kindMatches(specKind, eventKind string) bool {
	if specKind == "" || specKind == "Any" {
		return true
	}
	return strings.EqualFold(specKind, eventKind)
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

// agentManagedByLabel / agentManagedByValue mark objects the agent creates for
// its own work: scan Jobs and their pods (pkg/scanners) and profiler pods
// (pkg/podexec). Keep in sync with those packages.
const (
	agentManagedByLabel = "app.kubernetes.io/managed-by"
	agentManagedByValue = "nudgebee-agent"
)

// AgentOwned reports whether obj is one of the agent's own Jobs or pods. Their
// failures are not the cluster's problems: a scan that cannot pull its image or
// runs out of memory is reported through the scan's own result, and raising a
// finding for it as well would put the agent's housekeeping in the user's
// event stream (once per scan, on every agent watching the cluster).
func AgentOwned(obj map[string]any) bool {
	m, _ := obj["metadata"].(map[string]any)
	labels, _ := m["labels"].(map[string]any)
	v, _ := labels[agentManagedByLabel].(string)
	return v == agentManagedByValue
}
