package alerts

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"runtime/debug"
	"strings"
	"time"
)

// kubewatch sends one event at a time per resource type and waits for the
// response before sending the next. net/http only sends the response when
// the handler returns, so anything the handler does inline — decoding both
// objects, running predicates, listing Kubernetes events for the evidence
// tables — is serial latency on kubewatch's queue, and every event queued
// behind it pins two full objects in kubewatch's memory. The intake below
// keeps /api/handle constant time: the handler reads a small header out of
// the payload, queues it, and returns; matching runs on a worker pool.

// KubewatchMetrics receives the kubewatch intake's counters and latencies.
// Optional; every method must be safe for concurrent use.
type KubewatchMetrics interface {
	// OnKubewatchEvent counts one received event by kind and outcome (one
	// of the KubewatchOutcome* values).
	OnKubewatchEvent(kind, outcome string)
	// ObserveKubewatchHandle records how long /api/handle held the sender.
	ObserveKubewatchHandle(seconds float64)
	// ObserveKubewatchQueueWait records how long an event waited for a worker.
	ObserveKubewatchQueueWait(seconds float64)
	// ObserveKubewatchMatch records predicate + enrichment time for one event.
	ObserveKubewatchMatch(kind string, seconds float64)
}

// Outcomes for KubewatchMetrics.OnKubewatchEvent.
const (
	KubewatchOutcomeMatched     = "matched"
	KubewatchOutcomeUnmatched   = "unmatched"
	KubewatchOutcomeIgnoredKind = "ignored_kind" // no matcher reads this kind
	KubewatchOutcomeShed        = "shed"         // queue full; dropped
	KubewatchOutcomeInvalid     = "invalid"      // unparseable payload
	KubewatchOutcomePanic       = "panic"        // a matcher panicked
)

// Intake defaults, used when SetKubewatchQueue is not called.
const (
	defaultKubewatchWorkers       = 8
	defaultKubewatchQueueSize     = 2048
	defaultKubewatchQueueMaxBytes = 64 << 20
)

// KindFilter is an optional TriggerEngine extension. When the engine
// implements it, events of a kind no matcher reads are dropped on the
// request path without being queued.
type KindFilter interface {
	HandlesKind(kind string) bool
}

type kubewatchJob struct {
	operation string
	kind      string
	data      json.RawMessage // the payload's `data` object, re-decoded by the worker
	queued    time.Time
}

// SetKubewatchQueue sizes the kubewatch intake: workers matching events in
// parallel, the total number of events allowed to wait, and the total bytes
// they may hold. Events over either bound are shed and counted. Values <= 0
// keep the default. Call once at construction, before serving.
func (f *Forwarder) SetKubewatchQueue(workers, size int, maxBytes int64) {
	if workers > 0 {
		f.kwWorkers = workers
	}
	if size > 0 {
		f.kwQueueSize = size
	}
	if maxBytes > 0 {
		f.kwMaxBytes = maxBytes
	}
}

// KubewatchQueueDepth returns the number of kubewatch events waiting for a
// worker. Wire to a Prometheus gauge in main.
func (f *Forwarder) KubewatchQueueDepth() int64 { return f.kwDepth.Load() }

// startKubewatchWorkers builds the shards on first use, so a Forwarder that
// never receives a kubewatch event never starts goroutines. Each shard is one
// worker draining its own channel: events for the same object always hash to
// the same shard, so they are matched in the order kubewatch sent them. The
// recovery predicate and the rate limiter are order-sensitive (a recovery
// processed before the failure it follows would re-arm a suppressed Finding).
func (f *Forwarder) startKubewatchWorkers() {
	workers := f.kwWorkers
	if workers <= 0 {
		workers = defaultKubewatchWorkers
	}
	size := f.kwQueueSize
	if size <= 0 {
		size = defaultKubewatchQueueSize
	}
	if f.kwMaxBytes <= 0 {
		f.kwMaxBytes = defaultKubewatchQueueMaxBytes
	}
	perShard := size / workers
	if perShard < 1 {
		perShard = 1
	}
	f.kwShards = make([]chan kubewatchJob, workers)
	for i := range f.kwShards {
		ch := make(chan kubewatchJob, perShard)
		f.kwShards[i] = ch
		go f.runKubewatchWorker(ch)
	}
}

// enqueueKubewatch queues one event for matching. Returns false when the
// event was shed because the shard is full or the byte budget is spent.
func (f *Forwarder) enqueueKubewatch(job kubewatchJob, key string) bool {
	f.kwOnce.Do(f.startKubewatchWorkers)
	n := int64(len(job.data))
	if f.kwBytes.Add(n) > f.kwMaxBytes {
		f.kwBytes.Add(-n)
		return false
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	shard := f.kwShards[int(h.Sum32())%len(f.kwShards)]
	f.kwDepth.Add(1)
	select {
	case shard <- job:
		return true
	default:
		f.kwDepth.Add(-1)
		f.kwBytes.Add(-n)
		return false
	}
}

func (f *Forwarder) runKubewatchWorker(ch <-chan kubewatchJob) {
	for job := range ch {
		f.kwDepth.Add(-1)
		f.kwBytes.Add(-int64(len(job.data)))
		if f.KubewatchMetrics != nil {
			f.KubewatchMetrics.ObserveKubewatchQueueWait(time.Since(job.queued).Seconds())
		}
		f.matchKubewatch(job)
	}
}

// matchKubewatch decodes the objects, runs the trigger engine (predicates and
// the evidence-table lookups), and hands any matches to the forward pool.
func (f *Forwarder) matchKubewatch(job kubewatchJob) {
	kindLabel := strings.ToLower(job.kind)
	// The handler used to run matchers under net/http's per-request panic
	// recovery. On a worker an unrecovered panic would take the runner down.
	defer func() {
		if r := recover(); r != nil {
			f.recordKubewatch(kindLabel, KubewatchOutcomePanic)
			f.Logger.Error("kubewatch: matcher panicked; event dropped",
				"kind", job.kind, "operation", job.operation, "panic", r, "stack", string(debug.Stack()))
		}
	}()

	var inner struct {
		Obj    map[string]any `json:"obj"`
		OldObj map[string]any `json:"oldObj"`
	}
	if err := json.Unmarshal(job.data, &inner); err != nil {
		f.recordKubewatch(kindLabel, KubewatchOutcomeInvalid)
		f.Logger.Warn("kubewatch: inner data parse failed; dropping", "err", err)
		return
	}

	start := time.Now()
	matches := f.Engine.MatchK8sEvent(job.operation, job.kind, inner.Obj, inner.OldObj)
	if f.KubewatchMetrics != nil {
		f.KubewatchMetrics.ObserveKubewatchMatch(kindLabel, time.Since(start).Seconds())
	}
	if len(matches) == 0 {
		f.k8sUnmatched.Add(1)
		f.recordKubewatch(kindLabel, KubewatchOutcomeUnmatched)
		return
	}
	f.recordKubewatch(kindLabel, KubewatchOutcomeMatched)

	// One Finding per match. The same kubewatch event can fire multiple
	// matchers (e.g., a Pod that's both ImagePullBackOff and CrashLoopBackOff)
	// — each produces its own Finding with its own aggregation_key. Acquire the
	// pool slot ONCE per event (not per match) so a multi-finding event stays
	// atomic and ordered.
	f.spawnForward("kubewatch", func() {
		for i := range matches {
			env, err := f.builder.FromMatchedTrigger(matches[i], job.data)
			if err != nil {
				f.recordDrop("kubewatch_matcher_"+matches[i].MatcherName, err)
				continue
			}
			if err := f.forward(context.Background(), env); err != nil {
				f.recordDrop("kubewatch_matcher_"+matches[i].MatcherName, err)
				continue
			}
			f.recordForward()
		}
	})
}

func (f *Forwarder) recordKubewatch(kind, outcome string) {
	if f.KubewatchMetrics != nil {
		f.KubewatchMetrics.OnKubewatchEvent(kind, outcome)
	}
}

// kubewatchShardKey identifies the object an event is about, so all events
// for one object land on one shard.
func kubewatchShardKey(kind, namespace, name string) string {
	return strings.ToLower(kind) + "/" + namespace + "/" + name
}
