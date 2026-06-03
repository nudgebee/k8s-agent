package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Service drives all resource informers and posts changes through the Sink.
//
// Lifecycle (Run blocks):
//  1. Start the shared informer factory.
//  2. Wait for caches to sync.
//  3. Emit a full snapshot per registered resource type.
//  4. Start workers that consume the per-resource workqueues for incremental
//     updates (Add/Update/Delete events captured by the informer handlers).
//  5. Every Resync interval, re-emit a full snapshot.
//  6. Stop on ctx cancellation.
//
// Phase-4 note: today we only implement the Pod converter end-to-end. Other
// resource handlers (Deployment, StatefulSet, DaemonSet, ReplicaSet, Job,
// CronJob, Node, Namespace, Helm releases) follow the same shape; each one
// is its own follow-up so we can golden-diff per resource.
type Service struct {
	cs      kubernetes.Interface
	sink    *Sink
	factory informers.SharedInformerFactory
	resync  time.Duration
	logger  *slog.Logger

	handlers []*resourceHandler

	// Scalability options (see Options / SetOptions). Defaults are set in
	// NewService so the zero-config path stays wire-identical to before.
	snapshotBatching  bool
	batchSize         int
	incrementalBatch  int
	incrementalWindow time.Duration
	emitTombstones    bool

	// podIdx is the owner→representative-pod index, rebuilt at the start of
	// every snapshot and published atomically so the per-event incremental
	// workers can read it without locking. Never mutated after publish.
	podIdx atomic.Pointer[podIndex]
}

// Options carries the scalability tunables. All are optional; unset fields
// keep the historical (wire-identical) behavior.
type Options struct {
	// SnapshotBatching emits the full-load snapshot in BatchSize chunks using
	// the envelope batch fields. Requires collector support for batch
	// reassembly + deferred deletion-reconcile.
	SnapshotBatching bool
	BatchSize        int
	// IncrementalBatch coalesces up to N queued events into one incremental
	// envelope. <=1 keeps the one-item-per-event behavior.
	IncrementalBatch  int
	IncrementalWindow time.Duration
	// EmitTombstones emits a deleted:true item on resource deletion instead of
	// waiting for the next full snapshot. Requires collector support.
	EmitTombstones bool
}

// SetOptions applies scalability tunables. Call before Run().
func (s *Service) SetOptions(o Options) {
	s.snapshotBatching = o.SnapshotBatching
	if o.BatchSize > 0 {
		s.batchSize = o.BatchSize
	}
	if o.IncrementalBatch > 0 {
		s.incrementalBatch = o.IncrementalBatch
	}
	s.incrementalWindow = o.IncrementalWindow
	s.emitTombstones = o.EmitTombstones
}

// resourceHandler ties an informer to a workqueue and a converter.
type resourceHandler struct {
	typ       Type
	informer  cache.SharedIndexInformer
	queue     workqueue.TypedRateLimitingInterface[string]
	converter func(obj any) (wireItem any, ok bool)

	// tomb stashes converted tombstone items keyed by cache key, populated by
	// the informer DeleteFunc (which still has the deleted object) and drained
	// by the worker when it sees the key is gone from the indexer. Only used
	// when emitTombstones is on.
	tombMu sync.Mutex
	tomb   map[string]any
}

func (h *resourceHandler) stashTombstone(key string, item any) {
	h.tombMu.Lock()
	if h.tomb == nil {
		h.tomb = map[string]any{}
	}
	h.tomb[key] = item
	h.tombMu.Unlock()
}

func (h *resourceHandler) popTombstone(key string) (any, bool) {
	h.tombMu.Lock()
	defer h.tombMu.Unlock()
	item, ok := h.tomb[key]
	if ok {
		delete(h.tomb, key)
	}
	return item, ok
}

func NewService(cs kubernetes.Interface, sink *Sink, resync time.Duration, logger *slog.Logger) *Service {
	if resync <= 0 {
		resync = 30 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	// One factory shared across all resources; each resource registers its
	// own informer + handler. SharedInformerFactory dedups list/watch traffic.
	// WithTransform strips fields no converter reads BEFORE objects enter the
	// cache — the single biggest per-object memory win at 100k+ pods. See
	// discoveryTransform; it is verified to preserve every field the converters
	// read, so it's safe to apply unconditionally.
	factory := informers.NewSharedInformerFactoryWithOptions(cs, resync,
		informers.WithTransform(discoveryTransform))
	return &Service{
		cs:               cs,
		sink:             sink,
		factory:          factory,
		resync:           resync,
		logger:           logger,
		batchSize:        1000, // default chunk size when snapshotBatching is on
		incrementalBatch: 1,    // default: one item per incremental envelope
	}
}

// RegisterPods wires the Pod informer + workqueue + converter. Call before Run().
//
// The converter is bound to the ReplicaSet informer's indexer so each
// Pod's `owner` field is resolved up to the controlling Deployment
// (RS.OwnerReferences) instead of being emitted as the immediate
// ReplicaSet. This is what keeps `k8s_pods.workload_type =
// "Deployment"` at the backend; without it every Deployment-owned pod
// gets recorded as workload_type="ReplicaSet" and the UI workload
// queries return empty.
func (s *Service) RegisterPods() {
	s.register(s.factory.Core().V1().Pods().Informer(), TypeService, newPodConverter(s.replicaSetLookup()))
}

// RegisterDeployments wires the Deployment informer. The converter is
// bound to the Pod informer's lister so workload records can carry the
// qos_class / ip / conditions fields read off a representative Pod —
// mirroring the legacy discovery emission path.
func (s *Service) RegisterDeployments() {
	s.register(s.factory.Apps().V1().Deployments().Informer(), TypeService, newDeploymentConverter(s.podLookup()))
}

// RegisterStatefulSets wires the StatefulSet informer.
func (s *Service) RegisterStatefulSets() {
	s.register(s.factory.Apps().V1().StatefulSets().Informer(), TypeService, newStatefulSetConverter(s.podLookup()))
}

// RegisterDaemonSets wires the DaemonSet informer.
func (s *Service) RegisterDaemonSets() {
	s.register(s.factory.Apps().V1().DaemonSets().Informer(), TypeService, newDaemonSetConverter(s.podLookup()))
}

// RegisterNodes wires the Node informer.
func (s *Service) RegisterNodes() {
	s.register(s.factory.Core().V1().Nodes().Informer(), TypeNode, convertNode)
}

// RegisterNamespaces wires the Namespace informer.
func (s *Service) RegisterNamespaces() {
	s.register(s.factory.Core().V1().Namespaces().Informer(), TypeNamespace, convertNamespace)
}

// RegisterReplicaSets wires the ReplicaSet informer (filters out replicas==0).
func (s *Service) RegisterReplicaSets() {
	s.register(s.factory.Apps().V1().ReplicaSets().Informer(), TypeService, newReplicaSetConverter(s.podLookup()))
}

// podLookup returns the workload-UID → representative-Pod closure used by the
// workload converters. It reads the snapshot-scoped index published in
// s.podIdx (built by buildPodIndex at the start of every snapshot), making
// workload conversion O(1) instead of the old O(workloads × pods-per-namespace)
// selector scan. On a miss (e.g. a workload created since the last snapshot, on
// the incremental path) it returns nil; qos_class/ip/conditions self-heal at
// the next snapshot once a Pod reports status.
func (s *Service) podLookup() podLookupFn {
	return func(uid types.UID) *corev1.Pod {
		idx := s.podIdx.Load()
		if idx == nil {
			return nil
		}
		return idx.byOwner[uid]
	}
}

// podIndex maps a workload UID to one representative status-bearing Pod owned
// by that workload (directly, or via a ReplicaSet for Deployment-owned pods).
// Built once per snapshot and published atomically; never mutated after publish.
type podIndex struct {
	byOwner map[types.UID]*corev1.Pod
}

// buildPodIndex walks the Pod cache once (O(pods)) and indexes each
// status-bearing Pod under every workload UID in its ownership chain — the
// immediate controller (ReplicaSet / StatefulSet / DaemonSet) AND, for
// ReplicaSet-owned pods, the controlling Deployment. This lets both a
// Deployment record and its current ReplicaSet record resolve a representative
// pod in O(1). First write per UID wins (any running replica is representative).
func (s *Service) buildPodIndex() *podIndex {
	idx := &podIndex{byOwner: make(map[types.UID]*corev1.Pod)}
	podInf := s.factory.Core().V1().Pods().Informer()
	rsIndexer := s.factory.Apps().V1().ReplicaSets().Informer().GetIndexer()
	for _, raw := range podInf.GetIndexer().List() {
		pod, ok := raw.(*corev1.Pod)
		if !ok {
			continue
		}
		// Only pods that have reported status carry qos_class / ip / conditions.
		if pod.Status.QOSClass == "" && pod.Status.PodIP == "" && len(pod.Status.Conditions) == 0 {
			continue
		}
		for _, uid := range workloadOwnerUIDs(pod, rsIndexer) {
			if _, exists := idx.byOwner[uid]; !exists {
				idx.byOwner[uid] = pod
			}
		}
	}
	return idx
}

// workloadOwnerUIDs returns the UIDs of the workload(s) a Pod belongs to: its
// immediate controller, plus the controlling Deployment when the immediate
// controller is a ReplicaSet (resolved via the RS cache — authoritative, no
// name-suffix heuristics). Returns nil for unowned pods.
func workloadOwnerUIDs(pod *corev1.Pod, rsIndexer cache.Indexer) []types.UID {
	ctrl, ok := controllerOwner(pod.OwnerReferences)
	if !ok {
		return nil
	}
	uids := []types.UID{ctrl.UID}
	if ctrl.Kind == "ReplicaSet" && rsIndexer != nil {
		if obj, exists, err := rsIndexer.GetByKey(pod.Namespace + "/" + ctrl.Name); err == nil && exists {
			if rs, ok := obj.(*appsv1.ReplicaSet); ok {
				if rsCtrl, ok := controllerOwner(rs.OwnerReferences); ok {
					uids = append(uids, rsCtrl.UID)
				}
			}
		}
	}
	return uids
}

// replicaSetLookup returns a `(namespace, name) -> *ReplicaSet` closure
// backed by the ReplicaSet informer's indexer. Used by the Pod
// converter to walk the Pod → ReplicaSet → Deployment owner chain at
// emit time. Returns nil when the RS isn't (yet) in cache — caller
// falls back to emitting the ReplicaSet ref unchanged, and the next
// pod-status event re-emits with the correct owner once
// WaitForCacheSync completes.
//
// As with podLookup, GetIndexer() is taken eagerly here:
// SharedInformerFactory.ReplicaSets().Informer() is idempotent, so the
// indexer is the same shared instance RegisterReplicaSets wires up
// regardless of registration order.
func (s *Service) replicaSetLookup() replicaSetLookupFn {
	indexer := s.factory.Apps().V1().ReplicaSets().Informer().GetIndexer()
	return func(namespace, name string) *appsv1.ReplicaSet {
		if indexer == nil {
			return nil
		}
		obj, ok, err := indexer.GetByKey(namespace + "/" + name)
		if err != nil || !ok {
			return nil
		}
		rs, _ := obj.(*appsv1.ReplicaSet)
		return rs
	}
}

// RegisterJobs wires the Job informer (under "job" type).
func (s *Service) RegisterJobs() {
	s.register(s.factory.Batch().V1().Jobs().Informer(), TypeJob, convertJob)
}

// RegisterCronJobs wires the CronJob informer (under "job" type).
func (s *Service) RegisterCronJobs() {
	s.register(s.factory.Batch().V1().CronJobs().Informer(), TypeJob, convertCronJob)
}

// RegisterHelmReleases wires the Helm-release detector — a Secret informer
// scoped via the global SharedInformerFactory's tweakListOptions. We DO NOT
// watch all secrets here; the operator must construct the factory with
// label-selector scoping. Use NewServiceForHelm() to get a factory that's
// pre-scoped, or construct your own factory with informers.WithTweakListOptions.
func (s *Service) RegisterHelmReleases() {
	s.register(s.factory.Core().V1().Secrets().Informer(), TypeHelmRelease, convertHelmReleaseSecret)
}

// RegisterAll wires every supported resource (excluding Helm — see
// RegisterHelmReleases for why that needs separate setup). Convenience for
// production deployments where the operator wants the full snapshot.
func (s *Service) RegisterAll() {
	s.RegisterPods()
	s.RegisterDeployments()
	s.RegisterStatefulSets()
	s.RegisterDaemonSets()
	s.RegisterReplicaSets()
	s.RegisterJobs()
	s.RegisterCronJobs()
	s.RegisterNodes()
	s.RegisterNamespaces()
}

// register is the common path for any resource type.
func (s *Service) register(informer cache.SharedIndexInformer, typ Type, converter func(any) (any, bool)) {
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
	h := &resourceHandler{
		typ:       typ,
		informer:  informer,
		queue:     queue,
		converter: converter,
	}
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { enqueue(queue, obj) },
		UpdateFunc: func(_, obj any) { enqueue(queue, obj) },
		DeleteFunc: func(obj any) {
			// When tombstones are enabled, capture a deleted:true item NOW —
			// the DeleteFunc still has the object; once the key is gone from the
			// indexer the worker can't reconstruct the wire shape (Kind etc.).
			// Use the same key function as enqueue so stash + dequeue keys match.
			if s.emitTombstones {
				if key, err := cache.MetaNamespaceKeyFunc(obj); err == nil {
					if item, ok := converter(unwrapDeleted(obj)); ok {
						if m, ok := item.(map[string]any); ok {
							m["deleted"] = true
						}
						h.stashTombstone(key, item)
					}
				}
			}
			enqueue(queue, obj)
		},
	})
	s.handlers = append(s.handlers, h)
}

// unwrapDeleted returns the underlying object from a cache.DeletedFinalStateUnknown
// tombstone, or the object itself otherwise.
func unwrapDeleted(obj any) any {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		return d.Obj
	}
	return obj
}

// Run blocks until ctx is done.
func (s *Service) Run(ctx context.Context) error {
	if len(s.handlers) == 0 {
		return errors.New("discovery: no handlers registered")
	}

	stopCh := make(chan struct{})
	go func() { <-ctx.Done(); close(stopCh) }()

	s.factory.Start(stopCh)

	// Wait at the factory level (not just on handler-bound informers) so
	// auxiliary informers instantiated via lookup closures — replicaSetLookup,
	// podLookup — are synced before the initial snapshot. Otherwise, a caller
	// that wires RegisterPods without RegisterReplicaSets would race: the RS
	// informer would still be started (factory tracks any informer ever
	// requested), but a handler-only sync wait wouldn't include it, and the
	// first pod snapshot could fire with unresolved ReplicaSet owners.
	for typ, ok := range s.factory.WaitForCacheSync(stopCh) {
		if !ok {
			return fmt.Errorf("discovery: cache sync timed out for %v", typ)
		}
	}
	s.logger.Info("discovery caches synced", "handlers", len(s.handlers))

	// Initial full snapshot per resource type, then start workers + resync ticker.
	if err := s.emitAllSnapshots(ctx); err != nil {
		s.logger.Error("initial snapshot failed", "err", err)
	}

	var wg sync.WaitGroup
	for _, h := range s.handlers {
		wg.Add(1)
		go func(h *resourceHandler) {
			defer wg.Done()
			s.workerLoop(ctx, h)
		}(h)
	}

	// Periodic resync — re-emit full snapshots so the collector has a steady
	// "complete world view" cadence regardless of in-between deltas.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(s.resync)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.emitAllSnapshots(ctx); err != nil {
					s.logger.Error("periodic snapshot failed", "err", err)
				}
			}
		}
	}()

	<-ctx.Done()
	for _, h := range s.handlers {
		h.queue.ShutDown()
	}
	wg.Wait()
	return nil
}

// emitAllSnapshots refreshes the owner→pod index, then posts the full-load
// snapshot — either one envelope per type (legacy, wire-identical) or, when
// snapshotBatching is on, BatchSize chunks per type using the envelope batch
// fields. The index is rebuilt + published BEFORE conversion so converters see
// a consistent representative-pod view.
func (s *Service) emitAllSnapshots(ctx context.Context) error {
	s.podIdx.Store(s.buildPodIndex())
	if s.snapshotBatching {
		return s.emitBatched(ctx)
	}
	return s.emitSingle(ctx)
}

// emitSingle is the historical path: one full-load envelope per resource type,
// total_batches=1. Group items by Type since multiple handlers share a Type
// (Pods + Deployments + StatefulSets all bucket under "service").
func (s *Service) emitSingle(ctx context.Context) error {
	byType := map[Type][]any{}
	for _, h := range s.handlers {
		for _, raw := range h.informer.GetIndexer().List() {
			if item, ok := h.converter(raw); ok {
				byType[h.typ] = append(byType[h.typ], item)
			}
		}
	}
	batchID := fmt.Sprintf("snap-%d", time.Now().UnixNano())
	var firstErr error
	for typ, items := range byType {
		env := &Envelope{
			Type:          typ,
			Data:          items,
			FullLoad:      true,
			BatchID:       batchID,
			BatchSequence: 1,
			TotalBatches:  1,
			IsFirstBatch:  true,
			IsLastBatch:   true,
		}
		if err := s.sink.Post(ctx, env); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// emitBatched streams each Type's snapshot in BatchSize chunks so the agent
// never materializes the whole world in memory or in one POST. Requires the
// collector to reassemble batches by batch_id and defer its deletion-reconcile
// until is_last_batch (the collector's should_cleanup = is_last_batch).
func (s *Service) emitBatched(ctx context.Context) error {
	handlersByType := map[Type][]*resourceHandler{}
	var order []Type
	for _, h := range s.handlers {
		if _, seen := handlersByType[h.typ]; !seen {
			order = append(order, h.typ)
		}
		handlersByType[h.typ] = append(handlersByType[h.typ], h)
	}
	var firstErr error
	for _, typ := range order {
		if err := s.emitTypeBatched(ctx, typ, handlersByType[typ]); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// emitTypeBatched emits one Type's full-load snapshot in chunks. It flattens
// the type's handlers into one slice of raw cache pointers (cheap — no copies),
// then converts each exactly once while chunking. total_batches is advisory:
// the collector triggers its reconcile on is_last_batch (which the final chunk
// always carries), so we estimate it from the raw count rather than paying a
// second conversion pass to get an exact figure.
func (s *Service) emitTypeBatched(ctx context.Context, typ Type, hs []*resourceHandler) error {
	batchSize := s.batchSize
	if batchSize <= 0 {
		batchSize = 1000
	}

	// Flatten raw items (shared cache pointers, ~one word each) so we convert
	// only once below. The estimate may exceed the converted count (some
	// converters drop items, e.g. replicas==0 ReplicaSets); that's fine since
	// total_batches is advisory.
	type rawItem struct {
		raw any
		h   *resourceHandler
	}
	var items []rawItem
	for _, h := range hs {
		for _, raw := range h.informer.GetIndexer().List() {
			items = append(items, rawItem{raw: raw, h: h})
		}
	}
	totalEstimate := len(items)
	batchID := fmt.Sprintf("snap-%s-%d", typ, time.Now().UnixNano())

	chunk := make([]any, 0, batchSize)
	seq := 0
	var firstErr error
	flush := func(last bool) {
		seq++
		env := &Envelope{
			Type:          typ,
			Data:          append([]any(nil), chunk...),
			FullLoad:      true,
			BatchID:       batchID,
			BatchSequence: seq,
			TotalBatches:  maxInt(totalEstimate+batchSize-1, 0) / maxInt(batchSize, 1),
			IsFirstBatch:  seq == 1,
			IsLastBatch:   last,
		}
		if env.TotalBatches == 0 {
			env.TotalBatches = 1 // empty type still sends one authoritative envelope
		}
		if err := s.sink.Post(ctx, env); err != nil && firstErr == nil {
			firstErr = err
		}
		chunk = chunk[:0]
	}

	for i, it := range items {
		item, ok := it.h.converter(it.raw)
		if !ok {
			continue
		}
		chunk = append(chunk, item)
		if len(chunk) >= batchSize && i < len(items)-1 {
			flush(false)
			if firstErr != nil {
				// Abort WITHOUT sending a terminal is_last_batch envelope. The
				// collector triggers its stale-cleanup on is_last_batch
				// (should_cleanup = is_last_batch) and has no "aborted" concept,
				// so emitting a last batch here would reconcile against a partial
				// snapshot and mass-deactivate live resources. Stopping silently
				// leaves the already-sent batches' upserts in place; the next
				// resync re-runs the full sync (its is_first_batch restarts it).
				s.logger.Error("snapshot batch failed mid-stream; aborting without cleanup",
					"type", typ, "batch_id", batchID, "sent_batches", seq, "err", firstErr)
				return firstErr
			}
		}
	}
	// Final (or only / empty) chunk carries is_last_batch=true.
	flush(true)
	return firstErr
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// workerLoop drains the handler's workqueue, coalescing up to incrementalBatch
// currently-queued keys into a single incremental envelope (no batch metadata
// → the collector treats it as INCREMENTAL, no cleanup). With incrementalBatch
// <= 1 it stays one-item-per-envelope (wire-identical to the legacy path).
func (s *Service) workerLoop(ctx context.Context, h *resourceHandler) {
	for {
		key, shutdown := h.queue.Get()
		if shutdown {
			return
		}
		keys := []string{key}
		// Coalesce up to incrementalBatch distinct objects into one envelope.
		// With no window, drain only the backlog already queued (queue.Len() is
		// a hint; queue dedups by key so these are distinct objects). With a
		// window, wait up to incrementalWindow for more events to accumulate,
		// polling so we never block past the deadline.
		if s.incrementalBatch > 1 {
			var deadline time.Time
			if s.incrementalWindow > 0 {
				deadline = time.Now().Add(s.incrementalWindow)
			}
			for len(keys) < s.incrementalBatch {
				if h.queue.Len() == 0 {
					if s.incrementalWindow <= 0 || !time.Now().Before(deadline) {
						break
					}
					time.Sleep(5 * time.Millisecond)
					continue
				}
				k, sd := h.queue.Get()
				if sd {
					break
				}
				keys = append(keys, k)
			}
		}
		s.processBatch(ctx, h, keys)
		for _, k := range keys {
			h.queue.Done(k)
		}
	}
}

// processBatch converts the drained keys into one incremental envelope. Each
// Get is paired with a Done by the caller; here we manage Forget /
// AddRateLimited per key: Forget converted keys only on a successful POST,
// AddRateLimited on a retryable indexer error, Forget deletions/non-converts
// immediately.
func (s *Service) processBatch(ctx context.Context, h *resourceHandler, keys []string) {
	items := make([]any, 0, len(keys))
	converted := make([]string, 0, len(keys))
	for _, key := range keys {
		raw, exists, err := h.informer.GetIndexer().GetByKey(key)
		if err != nil {
			h.queue.AddRateLimited(key)
			s.logger.Error("indexer get", "key", key, "err", err)
			continue
		}
		if !exists {
			// Deletion. With tombstones enabled, the DeleteFunc stashed a
			// deleted:true item we can emit so the collector marks the resource
			// inactive immediately; otherwise the next full snapshot reconciles.
			if s.emitTombstones {
				if tomb, ok := h.popTombstone(key); ok {
					items = append(items, tomb)
				}
			} else {
				s.logger.Debug("resource deleted (will reconcile on next snapshot)", "key", key)
			}
			h.queue.Forget(key)
			continue
		}
		item, ok := h.converter(raw)
		if !ok {
			h.queue.Forget(key)
			continue
		}
		items = append(items, item)
		converted = append(converted, key)
	}
	if len(items) == 0 {
		return
	}
	env := &Envelope{Type: h.typ, Data: items}
	if err := s.sink.Post(ctx, env); err != nil {
		s.logger.Error("incremental post failed", "type", h.typ, "count", len(items), "err", err)
		// Don't AddRateLimited on backend errors — the next periodic snapshot
		// picks it up; failed retries pile up otherwise.
		return
	}
	for _, key := range converted {
		h.queue.Forget(key) // clear any backoff from a prior indexer-get error
	}
}

// enqueue puts a MetaNamespaceKey for obj on the queue, ignoring tombstones.
func enqueue(q workqueue.TypedRateLimitingInterface[string], obj any) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	q.Add(key)
}

// convertPod produces the service-of-type-Pod dict. Same wire shape as
// Deployment/StatefulSet/etc. — the collector reads pods through the
// same `_process_discovery` path, keying off `service_key`
// and `type=="Pod"` for the terminal-state branch.
//
// This is the no-lookup variant kept for backward-compat / tests; the
// production `RegisterPods` path wires `newPodConverter(rsLookup)` so
// the owner field is resolved up to the controlling Deployment via the
// ReplicaSet informer. Without the lookup, a Pod owned by a ReplicaSet
// is emitted with `owner = [{kind: "ReplicaSet", ...}]` — the backend
// then keys its workload tables to the RS rather than the Deployment.
func convertPod(obj any) (any, bool) {
	return newPodConverter(nil)(obj)
}

// newPodConverter binds a ReplicaSet lookup so the Pod's `owner` field
// can be resolved up one hop to the controlling Deployment. See
// ownerInfosWithRSLookup for the resolution rule; without rsLookup the
// emitted owner remains the immediate ReplicaSet, which breaks all
// downstream workload-keyed views (k8s_pods.workload_type stays
// "ReplicaSet" forever).
func newPodConverter(rsLookup replicaSetLookupFn) func(any) (any, bool) {
	return func(obj any) (any, bool) {
		p, ok := obj.(*corev1.Pod)
		if !ok {
			return nil, false
		}
		containers := make([]map[string]any, 0, len(p.Spec.Containers))
		for _, c := range p.Spec.Containers {
			containers = append(containers, map[string]any{
				"name":  c.Name,
				"image": c.Image,
			})
		}
		// Pods don't have a "ready replica" semantic — use ready-container count
		// for ready_pods and 1 for total_pods (it's a single pod). The UI uses
		// these for service-detail rollups.
		readyContainers := int32(0)
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				readyContainers++
			}
		}
		return map[string]any{
			"name":             p.Name,
			"namespace":        p.Namespace,
			"type":             "Pod",
			"service_key":      p.Namespace + "/Pod/" + p.Name,
			"resource_version": parseResourceVersion(p.ObjectMeta),
			"creation_time":    p.CreationTimestamp.UTC().Format(time.RFC3339),
			"update_time":      time.Now().UTC().UnixMilli(),
			"deleted":          false,
			"classification":   "None",
			"total_pods":       1,
			"ready_pods":       readyContainers,
			"is_helm_release":  isHelmRelease(p.Labels, p.Annotations),
			"node_name":        p.Spec.NodeName,
			"status":           string(p.Status.Phase),
			"restart_count":    podRestartCounts(p),
			"status_dict":      nil,
			"config": map[string]any{
				"labels":     nonNilLabels(p.Labels),
				"containers": containers,
				"owner":      ownerInfosWithRSLookup(p.OwnerReferences, p.Namespace, rsLookup),
			},
		}, true
	}
}

// podRestartCounts produces the per-container restart_count map.
func podRestartCounts(p *corev1.Pod) map[string]int32 {
	out := make(map[string]int32, len(p.Status.ContainerStatuses))
	for _, cs := range p.Status.ContainerStatuses {
		out[cs.Name] = cs.RestartCount
	}
	return out
}

func parseResourceVersion(m metav1.ObjectMeta) int64 {
	// Best-effort; collector treats it as monotonic per (namespace, name).
	var n int64
	for _, c := range m.ResourceVersion {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int64(c-'0')
	}
	return n
}
