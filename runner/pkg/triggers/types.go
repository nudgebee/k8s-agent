// Package triggers implements playbook-trigger matching on the agent
// side. Most kubewatch events match nothing and are silently dropped;
// the few that match a registered trigger emit a Finding with a
// specific aggregation_key (`report_crash_loop`, `pod_oom_killer_enricher`,
// `image_pull_backoff_reporter`, `job_failure`, `node_not_ready`, …).
//
// Matchers are declared as data-driven MatcherSpec values rather than
// hardcoded if-blocks so when stage 2.3 ships DB-stored playbook config
// (plan §5d), the agent's existing config-pull path can deliver the same
// struct shape. No matcher refactor needed at that point.
package triggers

import (
	"context"
	"time"
)

// IncomingK8sEvent is the agent-friendly shape of the
// IncomingK8sEventPayload. Built from the inner `data` dict kubewatch
// posts at /api/handle. obj is always present; oldObj is non-nil only
// for UPDATE operations.
type IncomingK8sEvent struct {
	Operation string         // "create" | "update" | "delete" — kubewatch's lowercase enum (kubewatch/pkg/handlers/cloudevent/cloudevent.go:159-167) and the K8sOperationType enum. Strict string match in operationMatches; do not capitalize.
	Kind      string         // "Pod" | "Job" | "Deployment" | ... | "Event"
	Obj       map[string]any // current resource (full UnstructuredContent)
	OldObj    map[string]any // previous resource on UPDATE; nil otherwise
}

// MatcherSpec declares one trigger predicate + the Finding it emits.
// Plan agent feedback: keep this purely data so stage 2.3 can deliver
// these from a DB-stored playbook config without touching matcher code.
type MatcherSpec struct {
	// Name is for logging + metrics labels. Should be stable across
	// versions (used as a metric label cardinality control).
	Name string

	// Kind filters by `IncomingK8sEvent.Kind`. Use "Any" to match every
	// kind (rarely needed; most matchers are kind-specific).
	Kind string

	// Operations filters by `IncomingK8sEvent.Operation`. Empty slice
	// matches every operation. job_failure / pod_oom_killed fire on
	// UPDATE only (transition detection); pod_crash_loop fires on
	// UPDATE because the waiting state is observed there too.
	Operations []string

	// Predicate is the semantic gate. Returns true to fire the matcher.
	// `obj` is always non-nil; `oldObj` is non-nil only on UPDATE.
	// Predicates should be pure functions of (obj, oldObj) — no I/O,
	// no clock reads (the engine handles rate-limiting + grace window).
	Predicate func(obj, oldObj map[string]any) bool

	// PredicateCtx (optional) replaces Predicate for matchers whose firing
	// decision needs cluster reads. It receives the same EnrichContext
	// EnrichBlocks gets, so the matcher can consult the wired listers
	// (service_no_endpoints resolves spec.selector against live pods —
	// not derivable from the watched Service object alone). When set,
	// Predicate is ignored. Implementations must treat nil context
	// helpers as "cannot decide" and return false — never fire on
	// missing data.
	PredicateCtx func(obj, oldObj map[string]any, ec EnrichContext) bool

	// AggregationKey is set on the emitted Finding. Determines how the
	// UI groups + dedupes. We use the legacy strings (so the UI's
	// per-aggregation_key handling stays unchanged).
	AggregationKey string

	// Priority is the Finding's `priority` value:
	// "HIGH" | "MEDIUM" | "INFO" | "LOW" | "DEBUG" (FindingSeverity).
	Priority string

	// FindingType is "issue" for failure conditions (crash, OOM),
	// "configuration_change" for diff-style triggers (babysitter etc.).
	// Maps to the FindingType enum.
	FindingType string

	// RateLimit suppresses repeat fires of the same fingerprint within
	// the window. 0 = no rate-limit. pod_oom_killed uses 1h; others
	// typically 5-10 min.
	RateLimit time.Duration

	// FingerprintFn extracts the dedup key from the matched obj. Plan
	// agent feedback: fingerprint MUST include a recurrence-bucket
	// dimension for repeating conditions (crash count bucket, time
	// bucket, etc.) — otherwise OOM-3-times-an-hour collapses to
	// one Finding forever. Each builtin matcher provides its own.
	FingerprintFn func(obj map[string]any) string

	// SuppressOnResync controls the restart grace window. When true,
	// the engine drops fires for objects whose creationTimestamp is
	// older than (agentStartTime - GraceWindow). The legacy runner got
	// this for free via informer resync semantics; we have to opt in
	// per matcher. Set true for "currently-active condition" predicates
	// (crash, OOM, image-pull) where a resync would re-fire every
	// already-broken Pod after every helm upgrade. Set false for
	// transition predicates (job_failure, node_not_ready) where the
	// transition itself is the signal.
	SuppressOnResync bool

	// EnrichBlocks (optional) runs after the Predicate fires and the
	// rate-limit gate passes. The returned blocks are attached to
	// Match.ExtraBlocks and end up as additional evidence on the
	// emitted Finding. Used today by `babysitter_change` to attach the
	// computed spec diff as a `diff` block, and by Pod-state matchers
	// to attach recent K8s events. The EnrichContext carries optional
	// helpers (K8s events lister, logger) — matchers ignore the ones
	// they don't need. Stage-2.2 enricher composers will replace this
	// for triggers whose enrichment requires server-side relay calls.
	EnrichBlocks func(obj, oldObj map[string]any, ec EnrichContext) []EvidenceBlock
}

// EnrichContext is the per-event context the engine threads into
// EnrichBlocks. Carries optional helpers a matcher may use to fetch
// extra evidence (recent K8s events, …). Fields are nilable — matchers
// must check before calling.
type EnrichContext struct {
	// EventsLister fetches recent K8s events for a namespaced or
	// cluster-scoped object. Set when the engine was wired with a K8s
	// client at startup; nil in unit tests, in which case event-table
	// blocks degrade to "events unavailable".
	EventsLister K8sEventsLister

	// ServiceBackends resolves a Service selector against live pods and
	// workload pod templates. Set when the engine was wired with a K8s
	// client at startup; nil in unit tests, in which case the
	// service_no_endpoints matcher never fires.
	ServiceBackends ServiceBackendsLister
}

// K8sEventsLister fetches recent K8s events for an object. The engine
// uses it to attach three tables to every match by default:
//
//   - subject events:   ListEvents(ns, kind, name, …) — the matched object
//   - node events:      ListEvents("", "Node", nodeName, …) — cluster-wide
//   - namespace events: ListEvents(ns, "", "", …) — no involvedObject filter
//
// Empty kind/name means "no filter on that dimension"; empty namespace
// means "cluster-wide" (Events("").List()). The implementation rejects
// only the (namespace="" && kind=="" && name=="") case to avoid an
// unbounded all-events sweep.
//
// Implementation lives in cmd/agent/main.go and wraps clientset.CoreV1().Events()
// with a short timeout. A nil lister is valid (tests, environments without a
// K8s client) — matchers handle it by emitting a placeholder event-table block.
type K8sEventsLister interface {
	ListEvents(ctx context.Context, namespace, kind, name string, limit int) ([]K8sEvent, error)
}

// ServiceBackendsLister resolves Service selectors against cluster state.
// Used by the service_no_endpoints matcher: whether any pod (or any
// workload pod template) matches the Service's spec.selector is cluster
// state, not derivable from the watched Service object alone.
//
// Implementation lives in cmd/agent/service_backends_lister.go and wraps
// the typed clientset. A nil lister is valid (tests, environments without
// a K8s client) — the matcher then never fires.
type ServiceBackendsLister interface {
	// AnyPodMatching reports whether any pod in the namespace carries
	// every label pair in selector. Pod phase is ignored — a crashing pod
	// still backs the Service's endpoints (its brokenness is another
	// matcher's concern).
	AnyPodMatching(ctx context.Context, namespace string, selector map[string]string) (bool, error)

	// AnyWorkloadTemplateMatching reports whether any workload
	// (Deployment / StatefulSet / DaemonSet) in the namespace has a pod
	// template whose labels carry every pair in selector. True means the
	// selector is wired to a real workload that currently has no pods
	// (scaled to zero, mid-rollout) — a scale state, not a
	// misconfiguration.
	AnyWorkloadTemplateMatching(ctx context.Context, namespace string, selector map[string]string) (bool, error)

	// ListPodLabels returns up to limit (name, labels) samples of pods in
	// the namespace, used as selector-vs-labels comparison evidence on
	// the emitted Finding.
	ListPodLabels(ctx context.Context, namespace string, limit int) ([]PodLabelSample, error)
}

// PodLabelSample is one pod's name + labels, for the "compare the
// configured selector against actual pod labels" evidence table.
type PodLabelSample struct {
	Name   string
	Labels map[string]string
}

// K8sEvent is the subset of corev1.Event the agent surfaces in
// evidence. Fields mirror what pod_events_enricher emits in its
// TableBlock columns (kubectl get events output).
type K8sEvent struct {
	Type      string    // "Warning" | "Normal"
	Reason    string    // "BackOff" | "OOMKilling" | "FailedScheduling" | …
	Message   string    // event.message
	Count     int32     // event.count (or series.count)
	FirstSeen time.Time // earliest observation
	LastSeen  time.Time // most recent observation
	Source    string    // event.source.component (kubelet, default-scheduler, …)
}

// EvidenceBlock is one structured-data item inside the Finding's
// evidence array. `type` selects the block kind ("markdown" | "json" |
// "table" | …), other fields are kind-specific.
//
// Open map rather than typed struct so new block kinds can be passed
// through without validation. The collector + UI know how to render
// each kind.
type EvidenceBlock map[string]any

// Match is what Engine.Match returns for one fired trigger. The caller
// (typically pkg/alerts.Forwarder) uses these fields to build a
// FindingEnvelope via the Builder. Multiple matchers can fire on one
// event — the engine returns all of them.
type Match struct {
	// Spec is the matcher that fired. Caller pulls AggregationKey /
	// Priority / FindingType from here.
	Spec *MatcherSpec

	// Fingerprint is the dedup key the matcher computed via FingerprintFn.
	Fingerprint string

	// Owner is the resolved top-level workload (Deployment / DaemonSet /
	// StatefulSet / Job). Empty when the obj has no owner refs (e.g. a
	// bare Pod) or when the chain doesn't terminate at a known kind.
	Owner OwnerRef

	// SubjectName / SubjectNamespace come from obj.metadata.
	SubjectName      string
	SubjectNamespace string
	SubjectKind      string // lowercased; "pod" | "deployment" | etc.
	SubjectNode      string // obj.spec.nodeName when present

	// ExtraBlocks is populated from MatcherSpec.EnrichBlocks (when set).
	// The Finding builder appends each one as a structured-data block
	// alongside the raw event JSON evidence. Empty for matchers that
	// don't define EnrichBlocks.
	ExtraBlocks []EvidenceBlock
}

// OwnerRef is the resolved top-level workload. Set on Match.Owner when
// the engine's owner-walk terminates at a recognized kind.
type OwnerRef struct {
	Name string
	Kind string // canonical lowercased kind, e.g. "deployment" / "daemonset"
}
