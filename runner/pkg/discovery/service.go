package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
}

// resourceHandler ties an informer to a workqueue and a converter.
type resourceHandler struct {
	typ       Type
	informer  cache.SharedIndexInformer
	queue     workqueue.TypedRateLimitingInterface[string]
	converter func(obj any) (wireItem any, ok bool)
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
	factory := informers.NewSharedInformerFactory(cs, resync)
	return &Service{
		cs:      cs,
		sink:    sink,
		factory: factory,
		resync:  resync,
		logger:  logger,
	}
}

// RegisterPods wires the Pod informer + workqueue + converter. Call before Run().
func (s *Service) RegisterPods() {
	s.register(s.factory.Core().V1().Pods().Informer(), TypeService, convertPod)
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

// podLookup builds a `(namespace, labels.Selector) -> *Pod` closure
// backed by the Pod informer's indexer. Returns nil when the Pod
// informer hasn't been registered (RegisterPods must run first; the
// rest of RegisterAll already enforces that ordering).
//
// The closure scans namespace-scoped Pods and returns the first match
// with a non-empty PodStatus — running Pods carry QOSClass / PodIP /
// Conditions which are exactly what callers need. Returning the first
// match is fine: for Deployments / StatefulSets / DaemonSets all
// replicas share the same template, so qos_class is uniform per
// workload, ip is per-replica but the UI only renders one workload
// summary, and conditions reflect a single Pod's state.
func (s *Service) podLookup() podLookupFn {
	// SharedInformerFactory.Pods().Informer() is idempotent and returns
	// the same shared instance RegisterPods() wired up earlier, so this
	// works regardless of registration order.
	indexer := s.factory.Core().V1().Pods().Informer().GetIndexer()
	return func(namespace string, selector labels.Selector) *corev1.Pod {
		if indexer == nil || selector == nil {
			return nil
		}
		items, err := indexer.ByIndex(cache.NamespaceIndex, namespace)
		if err != nil {
			return nil
		}
		for _, raw := range items {
			pod, ok := raw.(*corev1.Pod)
			if !ok {
				continue
			}
			if !selector.Matches(labels.Set(pod.Labels)) {
				continue
			}
			// Prefer a Pod that's actually had status reported — qos_class
			// / ip / conditions stay empty on freshly-created Pods.
			if pod.Status.QOSClass != "" || pod.Status.PodIP != "" || len(pod.Status.Conditions) > 0 {
				return pod
			}
		}
		return nil
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
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { enqueue(queue, obj) },
		UpdateFunc: func(_, obj any) { enqueue(queue, obj) },
		DeleteFunc: func(obj any) { enqueue(queue, obj) },
	})
	s.handlers = append(s.handlers, &resourceHandler{
		typ:       typ,
		informer:  informer,
		queue:     queue,
		converter: converter,
	})
}

// Run blocks until ctx is done.
func (s *Service) Run(ctx context.Context) error {
	if len(s.handlers) == 0 {
		return errors.New("discovery: no handlers registered")
	}

	stopCh := make(chan struct{})
	go func() { <-ctx.Done(); close(stopCh) }()

	s.factory.Start(stopCh)

	// Wait for all caches to sync before emitting the first full snapshot.
	if !cache.WaitForCacheSync(stopCh, s.cacheSyncFuncs()...) {
		return errors.New("discovery: cache sync timed out")
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

func (s *Service) cacheSyncFuncs() []cache.InformerSynced {
	out := make([]cache.InformerSynced, 0, len(s.handlers))
	for _, h := range s.handlers {
		out = append(out, h.informer.HasSynced)
	}
	return out
}

// emitAllSnapshots posts one full-load envelope per resource type by walking
// each handler's indexer cache.
func (s *Service) emitAllSnapshots(ctx context.Context) error {
	// Group items by Type since multiple handlers can share a Type (e.g.
	// Pods + Deployments + StatefulSets all bucket under "service").
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

// workerLoop processes one queue item at a time. Each item becomes a tiny
// incremental update envelope (no batch metadata, so the collector treats it
// as INCREMENTAL — no cleanup operations).
func (s *Service) workerLoop(ctx context.Context, h *resourceHandler) {
	for {
		key, shutdown := h.queue.Get()
		if shutdown {
			return
		}
		s.processOne(ctx, h, key)
		h.queue.Done(key)
	}
}

func (s *Service) processOne(ctx context.Context, h *resourceHandler, key string) {
	raw, exists, err := h.informer.GetIndexer().GetByKey(key)
	if err != nil {
		h.queue.AddRateLimited(key)
		s.logger.Error("indexer get", "key", key, "err", err)
		return
	}
	if !exists {
		// Deletion: emit a tombstone marker so the collector can mark
		// resources inactive. Today the collector's incremental path doesn't
		// process deletions — full snapshots on resync handle them. We log
		// for now and skip.
		s.logger.Debug("resource deleted (will reconcile on next snapshot)", "key", key)
		return
	}
	item, ok := h.converter(raw)
	if !ok {
		return
	}
	env := &Envelope{Type: h.typ, Data: []any{item}}
	if err := s.sink.Post(ctx, env); err != nil {
		s.logger.Error("incremental post failed", "type", h.typ, "key", key, "err", err)
		// Don't AddRateLimited on transient backend errors — next periodic
		// snapshot will pick it up. Failed retries pile up otherwise.
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
// and `type=="Pod"` for the terminal-state
// branch.
func convertPod(obj any) (any, bool) {
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
	owners := make([]map[string]any, 0, len(p.OwnerReferences))
	for _, o := range p.OwnerReferences {
		owners = append(owners, map[string]any{"kind": o.Kind, "name": o.Name})
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
			"owner":      owners,
		},
	}, true
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
