package k8swatch

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"

	"github.com/nudgebee/nudgebee-agent/pkg/kube"
	"github.com/nudgebee/nudgebee-agent/pkg/triggers"
)

// CustomResourceEntry is one custom resource an operator listed.
type CustomResourceEntry struct {
	Group     string
	Resource  string // plural resource name, e.g. "certificates"
	Version   string // empty: the API server's preferred version
	DiffPaths []string
}

// ResolvedCustomResource is an entry the API server serves and the runner may
// list, with what discovery said about it.
type ResolvedCustomResource struct {
	GVR        schema.GroupVersionResource
	Kind       string // e.g. "Certificate"
	Namespaced bool
	DiffPaths  []string
}

// QualifiedKind is the kind the watcher, the engine and the Finding use for
// this resource: "Certificate.cert-manager.io".
func (r ResolvedCustomResource) QualifiedKind() string {
	return triggers.QualifiedKind(r.Kind, r.GVR.Group)
}

// Custom-resource entry states, for the gauge.
const (
	CustomResourceWatching   = "watching"
	CustomResourceUnresolved = "unresolved" // not served, or the runner cannot list it
	CustomResourceRefused    = "refused"    // secret-bearing
)

// CustomResourceMetrics is implemented by a Metrics that also reports the
// custom-resource gauge and reflector errors. Optional.
type CustomResourceMetrics interface {
	SetK8sWatchCustomResources(state string, n int)
	OnK8sWatchCustomResourceError(kind string)
}

// ResolveCustomResources looks each entry up once, at runner start. An entry
// the API server does not serve, that the runner cannot list, or whose
// objects hold credentials is skipped with one Warn line; the others are
// returned. A CRD installed later, or a version removed by an operator
// upgrade, takes effect on the next runner restart: the engine's matchers are
// fixed when it is built, so entries cannot be added while it runs.
func ResolveCustomResources(ctx context.Context, disco discovery.DiscoveryInterface, dyn dynamic.Interface,
	namespace string, entries []CustomResourceEntry, metrics Metrics, logger *slog.Logger,
) []ResolvedCustomResource {
	if logger == nil {
		logger = slog.Default()
	}
	counts := map[string]int{CustomResourceWatching: 0, CustomResourceUnresolved: 0, CustomResourceRefused: 0}
	var out []ResolvedCustomResource
	seen := map[schema.GroupResource]bool{}
	for _, e := range entries {
		gr := schema.GroupResource{Group: e.Group, Resource: strings.ToLower(e.Resource)}
		if seen[gr] {
			continue
		}
		seen[gr] = true
		if gr.Group == "" || kube.IsSecretBearingResource(gr.Group, gr.Resource) {
			counts[CustomResourceRefused]++
			logger.Warn("k8s watch: custom resource refused: its objects can hold credentials (or it is a core kind)",
				"group", gr.Group, "resource", gr.Resource)
			continue
		}
		r, err := resolveOne(ctx, disco, dyn, namespace, gr, e)
		if err != nil {
			counts[CustomResourceUnresolved]++
			logger.Warn("k8s watch: custom resource not watched; it is picked up on the next runner restart once it is served and listable",
				"group", gr.Group, "resource", gr.Resource, "version", e.Version, "err", err)
			continue
		}
		counts[CustomResourceWatching]++
		out = append(out, r)
	}
	if m, ok := metrics.(CustomResourceMetrics); ok {
		for state, n := range counts {
			m.SetK8sWatchCustomResources(state, n)
		}
	}
	return out
}

type unresolvedError string

func (e unresolvedError) Error() string { return string(e) }

func resolveOne(ctx context.Context, disco discovery.DiscoveryInterface, dyn dynamic.Interface,
	namespace string, gr schema.GroupResource, e CustomResourceEntry,
) (ResolvedCustomResource, error) {
	version := e.Version
	if version == "" {
		groups, err := disco.ServerGroups()
		if err != nil {
			return ResolvedCustomResource{}, err
		}
		for _, g := range groups.Groups {
			if g.Name == gr.Group {
				version = g.PreferredVersion.Version
				break
			}
		}
		if version == "" {
			return ResolvedCustomResource{}, unresolvedError("API group not served")
		}
	}
	gv := schema.GroupVersion{Group: gr.Group, Version: version}
	list, err := disco.ServerResourcesForGroupVersion(gv.String())
	if err != nil {
		return ResolvedCustomResource{}, err
	}
	var found *metav1.APIResource
	for i := range list.APIResources {
		if list.APIResources[i].Name == gr.Resource {
			found = &list.APIResources[i]
			break
		}
	}
	if found == nil {
		return ResolvedCustomResource{}, unresolvedError("resource not served in " + gv.String())
	}
	// The watch's informers are limited to one namespace when K8S_WATCH_NAMESPACE
	// is set; a cluster-scoped resource has no namespaced list, so its informer
	// would never sync (and the chart grants only a Role in that namespace).
	if !found.Namespaced && namespace != "" {
		return ResolvedCustomResource{}, unresolvedError("cluster-scoped resource; it cannot be watched while the watch is limited to namespace " + namespace)
	}
	gvr := gv.WithResource(gr.Resource)
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ri := dyn.Resource(gvr)
	var lerr error
	if found.Namespaced {
		_, lerr = ri.Namespace(namespace).List(probeCtx, metav1.ListOptions{Limit: 1})
	} else {
		_, lerr = ri.List(probeCtx, metav1.ListOptions{Limit: 1})
	}
	if lerr != nil {
		return ResolvedCustomResource{}, lerr
	}
	paths := e.DiffPaths
	if len(paths) == 0 {
		paths = triggers.CustomResourceDiffPaths
	}
	return ResolvedCustomResource{GVR: gvr, Kind: found.Kind, Namespaced: found.Namespaced, DiffPaths: paths}, nil
}

// CustomResourceTransform returns the cache transform for one custom
// resource: apiVersion, kind, metadata without managedFields and the
// applied-copy annotations, and the diffed fields — nothing else. What is not
// cached is never encoded, so it never reaches a Finding's evidence; status in
// particular is not kept.
func CustomResourceTransform(diffPaths []string) cache.TransformFunc {
	keep := map[string]bool{"apiVersion": true, "kind": true, "metadata": true}
	for _, p := range diffPaths {
		keep[p] = true
	}
	applied := triggers.AppliedCopyAnnotations()
	return func(obj any) (any, error) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			// Tombstones pass through as is.
			return obj, nil
		}
		for k := range u.Object {
			if !keep[k] {
				delete(u.Object, k)
			}
		}
		unstructured.RemoveNestedField(u.Object, "metadata", "managedFields")
		if ann := u.GetAnnotations(); len(ann) > 0 {
			changed := false
			for _, a := range applied {
				if _, ok := ann[a]; ok {
					delete(ann, a)
					changed = true
				}
			}
			if changed {
				u.SetAnnotations(ann)
			}
		}
		return u, nil
	}
}

// watchErrorWarnInterval spaces the warnings for a custom resource whose
// watch keeps failing (its version was removed by an operator upgrade).
const watchErrorWarnInterval = 5 * time.Minute

// RegisterCustomResources watches the resolved custom resources, each on its
// own informer with CustomResourceTransform, under its qualified kind. The
// K8S_WATCH_KINDS filter does not apply: it names built-in kinds.
func (w *Watcher) RegisterCustomResources(dyn dynamic.Interface, resolved []ResolvedCustomResource) {
	if dyn == nil || len(resolved) == 0 {
		return
	}
	f := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, w.cfg.Namespace, nil)
	for _, r := range resolved {
		kind := r.QualifiedKind()
		if !w.handles(kind, "update") {
			continue
		}
		var lastWarn atomic.Int64
		w.registerSource(sourceDef{kind, r.GVR.GroupVersion().String(), func() cache.SharedIndexInformer {
			inf := f.ForResource(r.GVR).Informer()
			if err := inf.SetTransform(CustomResourceTransform(r.DiffPaths)); err != nil {
				w.logger.Warn("k8s watch: custom resource transform not set; not watching it", "kind", kind, "err", err)
				return nil
			}
			_ = inf.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
				if m, ok := w.metrics.(CustomResourceMetrics); ok {
					m.OnK8sWatchCustomResourceError(strings.ToLower(kind))
				}
				now := time.Now().UnixNano()
				last := lastWarn.Load()
				if now-last < int64(watchErrorWarnInterval) || !lastWarn.CompareAndSwap(last, now) {
					return
				}
				w.logger.Warn("k8s watch: custom resource watch failing; if its version was removed, restart the runner to resolve it again",
					"kind", kind, "version", r.GVR.Version, "err", err)
			})
			return inf
		}})
	}
	w.startFuncs = append(w.startFuncs, f.Start)
}
