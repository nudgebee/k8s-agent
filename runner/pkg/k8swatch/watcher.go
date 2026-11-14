// Package k8swatch feeds the trigger matchers from the runner's own
// informers, in place of kubewatch.
//
// kubewatch watches the cluster, caches what it sees, serializes every change
// and POSTs it to the runner's /api/handle, which decodes it again. The runner
// already caches most of the same objects for discovery. This package takes
// the changes straight from informers — sharing discovery's informer factory
// where the kinds overlap — and hands each one to the same matching path,
// in the same wire shape kubewatch sends, so the Findings and their evidence
// do not change.
package k8swatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Sink receives one change, as kubewatch's CloudEvent `data` object.
type Sink interface {
	HandleK8sChange(operation, kind string, data json.RawMessage)
}

// Metrics receives the watcher's counters. Optional.
type Metrics interface {
	OnK8sWatchEvent(kind, outcome string)
	SetK8sWatchPending(n int)
}

// Outcomes for Metrics.OnK8sWatchEvent.
const (
	OutcomeQueued    = "queued"    // queued for matching
	OutcomeCoalesced = "coalesced" // merged into an update already waiting for the same object
	OutcomeShed      = "shed"      // MaxPending objects already waiting; dropped
	// An update with an unchanged resourceVersion: a relist after a dropped
	// watch replays every object as one. Counted here and again under its
	// queue outcome, since it is matched like any other update.
	OutcomeUnchanged    = "unchanged"
	OutcomeBeforeStart  = "dropped_existing"
	OutcomeNoMatcher    = "dropped_no_matcher"   // no matcher reads this kind and operation
	OutcomeOutOfScope   = "dropped_out_of_scope" // outside Config.Namespace
	OutcomeSkipped      = "dropped_skipped"      // Config.Skip: a change every matcher discards
	OutcomeEncodeFailed = "encode_failed"
)

// Config sizes the watcher.
type Config struct {
	// Workers match changes in parallel; one object is never matched by two
	// workers at once. Default 8.
	Workers int
	// MaxPending bounds how many objects may have changes waiting. Repeated
	// updates to one object coalesce, so this bounds memory by object count,
	// not by change rate. Default 10000.
	MaxPending int
	// StartTime is when the process started. An object added with a creation
	// time before it is one the informer's initial list found, not a new one,
	// and is not sent as a create — kubewatch's rule.
	StartTime time.Time
	// Namespace limits the namespaced kinds to one namespace, as kubewatch's
	// `namespace` setting did. Cluster-scoped kinds (Nodes) are unaffected.
	// Empty means every namespace.
	Namespace string
	// Kinds limits the watched kinds (lower-case kind names, "service" for
	// Services), as kubewatch's `resource` switches did. Nil means every kind
	// a matcher reads.
	Kinds map[string]bool
	// Skip reports a change every matcher would discard (ConfigMaps in the
	// excluded namespaces), so it is not encoded and matched for nothing.
	Skip func(kind, namespace, name string) bool
	// SyncWarnAfter is how long a kind may take to sync before a warning
	// names it. Default 2 minutes.
	SyncWarnAfter time.Duration
}

// Handles reports whether any matcher reads changes of this kind and
// operation ("create", "update", "delete").
type Handles func(kind, operation string) bool

// Watcher registers informer handlers and drives the matching workers.
type Watcher struct {
	cfg     Config
	sink    Sink
	handles Handles
	metrics Metrics
	logger  *slog.Logger

	queue   workqueue.TypedInterface[string]
	mu      sync.Mutex
	pending map[string]*pendingChanges

	synced     []syncedKind
	startFuncs []func(stop <-chan struct{})

	shedSince atomic.Int64
	shedWarn  atomic.Int64
}

// New builds a watcher. Register the sources, then call Run.
func New(cfg Config, sink Sink, handles Handles, metrics Metrics, logger *slog.Logger) *Watcher {
	if cfg.Workers <= 0 {
		cfg.Workers = 8
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = 10000
	}
	if cfg.SyncWarnAfter <= 0 {
		cfg.SyncWarnAfter = 2 * time.Minute
	}
	if cfg.StartTime.IsZero() {
		cfg.StartTime = time.Now()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Watcher{
		cfg:     cfg,
		sink:    sink,
		handles: handles,
		metrics: metrics,
		logger:  logger,
		queue:   workqueue.NewTyped[string](),
		pending: map[string]*pendingChanges{},
	}
}

type syncedKind struct {
	kind   string
	synced cache.InformerSynced
}

// source is one watched kind: kubewatch's `kind` and `apiVersion` strings for
// it, and its informer.
type source struct {
	kind       string
	apiVersion string
	informer   cache.SharedIndexInformer
}

// sourceDef builds a source's informer only when a matcher reads the kind:
// asking a factory for an informer creates it, and the factory then starts
// and caches it.
type sourceDef struct {
	kind       string
	apiVersion string
	informer   func() cache.SharedIndexInformer
}

// RegisterShared watches the kinds discovery also caches, on discovery's
// informer factory, so those objects are cached once. The factory's transform
// must keep every field the matchers and their evidence readers use; see
// discovery.TransformFunc. Kinds no matcher reads are skipped.
func (w *Watcher) RegisterShared(f informers.SharedInformerFactory) {
	w.register(
		sourceDef{"Pod", "v1", f.Core().V1().Pods().Informer},
		sourceDef{"Job", "batch/v1", f.Batch().V1().Jobs().Informer},
		sourceDef{"Node", "v1", f.Core().V1().Nodes().Informer},
		sourceDef{"Deployment", "apps/v1", f.Apps().V1().Deployments().Informer},
		sourceDef{"DaemonSet", "apps/v1", f.Apps().V1().DaemonSets().Informer},
		sourceDef{"StatefulSet", "apps/v1", f.Apps().V1().StatefulSets().Informer},
	)
	w.startFuncs = append(w.startFuncs, f.Start)
}

// RegisterOwn watches the kinds only the matchers read, on a factory of the
// watcher's own (ConfigMaps, Services, Ingresses), with OwnTransform applied
// and limited to Config.Namespace.
func (w *Watcher) RegisterOwn(cs kubernetes.Interface) {
	f := informers.NewSharedInformerFactoryWithOptions(cs, 0,
		informers.WithTransform(OwnTransform), informers.WithNamespace(w.cfg.Namespace))
	w.register(
		sourceDef{"ConfigMap", "v1", f.Core().V1().ConfigMaps().Informer},
		sourceDef{"Service", "v1", f.Core().V1().Services().Informer},
		sourceDef{"Ingress", "networking.k8s.io/v1", f.Networking().V1().Ingresses().Informer},
	)
	w.startFuncs = append(w.startFuncs, f.Start)
}

// rolloutGVR is the Argo Rollouts resource kubewatch watches.
var rolloutGVR = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}

// RegisterRollouts watches Argo Rollouts when the API serves them and the
// runner may list them. It returns false, and watches nothing, otherwise: an
// informer that cannot list never syncs. kubewatch sent these with apiVersion
// "v1alpha1", without the group; kept for parity.
func (w *Watcher) RegisterRollouts(ctx context.Context, cs kubernetes.Interface, dyn dynamic.Interface) bool {
	if dyn == nil || !w.handlesKind("Rollout") {
		return false
	}
	if _, err := cs.Discovery().ServerResourcesForGroupVersion(rolloutGVR.GroupVersion().String()); err != nil {
		w.logger.Info("k8s watch: Argo Rollouts not served; not watching them", "err", err)
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := dyn.Resource(rolloutGVR).Namespace(w.cfg.Namespace).List(probeCtx, metav1.ListOptions{Limit: 1}); err != nil {
		w.logger.Warn("k8s watch: cannot list Argo Rollouts; not watching them", "err", err)
		return false
	}
	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, w.cfg.Namespace, nil)
	w.register(sourceDef{"Rollout", rolloutGVR.Version, func() cache.SharedIndexInformer {
		inf := f.ForResource(rolloutGVR).Informer()
		if err := inf.SetTransform(OwnTransform); err != nil {
			w.logger.Warn("k8s watch: rollout transform not set", "err", err)
		}
		return inf
	}})
	w.startFuncs = append(w.startFuncs, f.Start)
	return true
}

func (w *Watcher) handlesKind(kind string) bool {
	if w.cfg.Kinds != nil && !w.cfg.Kinds[strings.ToLower(kind)] {
		return false
	}
	for _, op := range []string{"create", "update", "delete"} {
		if w.handles(kind, op) {
			return true
		}
	}
	return false
}

func (w *Watcher) register(defs ...sourceDef) {
	for _, d := range defs {
		if !w.handlesKind(d.kind) {
			continue
		}
		s := source{kind: d.kind, apiVersion: d.apiVersion, informer: d.informer()}
		// Resync period 0: this handler never receives the factory's periodic
		// resync, which would replay every cached object as an update.
		_, err := s.informer.AddEventHandlerWithResyncPeriod(cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj any) { w.onAdd(s, obj) },
			UpdateFunc: func(oldObj, newObj any) { w.onUpdate(s, oldObj, newObj) },
			DeleteFunc: func(obj any) { w.onDelete(s, obj) },
		}, 0)
		if err != nil {
			w.logger.Warn("k8s watch: handler not registered", "kind", s.kind, "err", err)
			continue
		}
		w.synced = append(w.synced, syncedKind{s.kind, s.informer.HasSynced})
	}
}

func (w *Watcher) onAdd(s source, obj any) {
	m, err := meta.Accessor(obj)
	if err != nil {
		return
	}
	// The initial list delivers every existing object as an add. kubewatch
	// sent a create only for objects created after it started; same here.
	if !m.GetCreationTimestamp().After(w.cfg.StartTime) {
		w.count(s.kind, OutcomeBeforeStart)
		return
	}
	w.enqueue(s, "create", obj, nil)
}

func (w *Watcher) onUpdate(s source, oldObj, newObj any) {
	om, err1 := meta.Accessor(oldObj)
	nm, err2 := meta.Accessor(newObj)
	if err1 != nil || err2 != nil {
		return
	}
	// A relist after a dropped watch replays unchanged objects as updates.
	// kubewatch sent those to the matchers, and the crash-loop recovery reset
	// and rate limits are tuned on that, so they are matched too.
	if om.GetResourceVersion() == nm.GetResourceVersion() {
		w.count(s.kind, OutcomeUnchanged)
	}
	w.enqueue(s, "update", newObj, oldObj)
}

func (w *Watcher) onDelete(s source, obj any) {
	// A delete missed while the watch was down arrives as a tombstone that
	// wraps the last known state.
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	w.enqueue(s, "delete", obj, nil)
}

// change is one event waiting to be matched. obj and old point into the
// informer cache and are never modified.
type change struct {
	src       source
	operation string
	obj, old  any
}

// pendingChanges are the changes waiting for one object, in order. Updates
// that arrive back to back are merged: the first old object with the latest
// new one, which is what a diff across them should compare.
type pendingChanges struct {
	changes []change
}

// shedWarnInterval spaces the shed warnings: a shed loses alerts, so it must
// show at the default log level, but sheds come in floods.
const shedWarnInterval = 30 * time.Second

// warnShed logs a shed at Warn at most once per interval, with the number of
// changes shed since the previous warning.
func (w *Watcher) warnShed(key string) {
	n := w.shedSince.Add(1)
	now := time.Now().UnixNano()
	last := w.shedWarn.Load()
	if now-last < int64(shedWarnInterval) || !w.shedWarn.CompareAndSwap(last, now) {
		return
	}
	w.shedSince.Add(-n)
	w.logger.Warn("k8s watch: changes shed: too many objects waiting to be matched; raise K8S_WATCH_MAX_PENDING / K8S_WATCH_WORKERS if this persists",
		"shed_since_last_warning", n, "latest", key)
}

func (w *Watcher) enqueue(s source, operation string, obj, old any) {
	if !w.handles(s.kind, operation) {
		w.count(s.kind, OutcomeNoMatcher)
		return
	}
	if m, err := meta.Accessor(obj); err == nil {
		ns := m.GetNamespace()
		// Shared informers are cluster-wide; this applies the namespace limit
		// to them.
		if w.cfg.Namespace != "" && ns != "" && ns != w.cfg.Namespace {
			w.count(s.kind, OutcomeOutOfScope)
			return
		}
		if w.cfg.Skip != nil && w.cfg.Skip(s.kind, ns, m.GetName()) {
			w.count(s.kind, OutcomeSkipped)
			return
		}
	}
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		return
	}
	key = s.kind + "/" + key

	w.mu.Lock()
	p, ok := w.pending[key]
	if !ok {
		if len(w.pending) >= w.cfg.MaxPending {
			w.mu.Unlock()
			w.count(s.kind, OutcomeShed)
			w.warnShed(key)
			return
		}
		p = &pendingChanges{}
		w.pending[key] = p
	}
	outcome := OutcomeQueued
	if n := len(p.changes); n > 0 && operation == "update" && p.changes[n-1].operation == "update" {
		p.changes[n-1].obj = obj
		outcome = OutcomeCoalesced
	} else {
		p.changes = append(p.changes, change{src: s, operation: operation, obj: obj, old: old})
	}
	if w.metrics != nil {
		// Under the lock, so a later take() cannot be overwritten by this.
		w.metrics.SetK8sWatchPending(len(w.pending))
	}
	w.mu.Unlock()

	w.count(s.kind, outcome)
	// The queue holds each key once; a key added while a worker processes it
	// is processed again afterwards, so nothing waiting is missed.
	w.queue.Add(key)
}

// take removes and returns the changes waiting for key.
func (w *Watcher) take(key string) []change {
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.pending[key]
	delete(w.pending, key)
	if w.metrics != nil {
		w.metrics.SetK8sWatchPending(len(w.pending))
	}
	if p == nil {
		return nil
	}
	return p.changes
}

// Run starts the informers and matches changes until ctx is done. Workers
// start at once, not after every cache syncs: a kind that cannot be listed
// (an RBAC gap) never syncs, and must not hold back the others' changes.
// Objects from the initial lists are dropped as existing either way.
func (w *Watcher) Run(ctx context.Context) error {
	if len(w.synced) == 0 {
		w.logger.Info("k8s watch: no kinds to watch")
		<-ctx.Done()
		return nil
	}
	for _, start := range w.startFuncs {
		start(ctx.Done())
	}
	for _, k := range w.synced {
		go w.reportSync(ctx, k)
	}

	var wg sync.WaitGroup
	for range w.cfg.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for w.processNext() {
			}
		}()
	}
	<-ctx.Done()
	w.queue.ShutDown()
	wg.Wait()
	return nil
}

// reportSync logs when a kind's cache syncs, and warns when it has not after
// SyncWarnAfter: its changes are not seen until it does.
func (w *Watcher) reportSync(ctx context.Context, k syncedKind) {
	warnCtx, cancel := context.WithTimeout(ctx, w.cfg.SyncWarnAfter)
	defer cancel()
	if cache.WaitForCacheSync(warnCtx.Done(), k.synced) {
		w.logger.Info("k8s watch: cache synced", "kind", k.kind)
		return
	}
	if ctx.Err() != nil {
		return
	}
	w.logger.Warn("k8s watch: cache not synced; changes of this kind are not matched until it does (check the runner can list and watch it)",
		"kind", k.kind, "after", w.cfg.SyncWarnAfter)
	if cache.WaitForCacheSync(ctx.Done(), k.synced) {
		w.logger.Info("k8s watch: cache synced", "kind", k.kind)
	}
}

func (w *Watcher) processNext() bool {
	key, quit := w.queue.Get()
	if quit {
		return false
	}
	defer w.queue.Done(key)
	for _, c := range w.take(key) {
		data, err := encode(c)
		if err != nil {
			w.count(c.src.kind, OutcomeEncodeFailed)
			w.logger.Warn("k8s watch: change not encoded; dropped", "key", key, "err", err)
			continue
		}
		w.sink.HandleK8sChange(c.operation, c.src.kind, data)
	}
	return true
}

func (w *Watcher) count(kind, outcome string) {
	if w.metrics != nil {
		w.metrics.OnK8sWatchEvent(strings.ToLower(kind), outcome)
	}
}

// payload is kubewatch's CloudEventMessageData, field for field and in the
// same order, so the evidence the runner stores is what kubewatch's was.
type payload struct {
	Operation   string `json:"operation"`
	Kind        string `json:"kind"`
	ClusterUID  string `json:"clusterUid"`
	Description string `json:"description"`
	APIVersion  string `json:"apiVersion"`
	Obj         any    `json:"obj"`
	OldObj      any    `json:"oldObj"`
}

func encode(c change) (json.RawMessage, error) {
	ns, name := "", ""
	if m, err := meta.Accessor(c.obj); err == nil {
		ns, name = m.GetNamespace(), m.GetName()
	}
	p := payload{
		Operation:   c.operation,
		Kind:        c.src.kind,
		Description: description(c.src.kind, ns, name, c.operation),
		APIVersion:  c.src.apiVersion,
		Obj:         c.obj,
		OldObj:      c.old,
	}
	return json.Marshal(p)
}

// description is kubewatch's Event.Message for the kinds watched here: its
// per-kind cases match lower-case or synthetic kinds that never reach it, so
// every change takes the default format.
func description(kind, namespace, name, operation string) string {
	reason := map[string]string{"create": "Created", "update": "Updated", "delete": "Deleted"}[operation]
	return fmt.Sprintf("A `%s` in namespace `%s` has been `%s`:\n`%s`", kind, namespace, reason, name)
}
