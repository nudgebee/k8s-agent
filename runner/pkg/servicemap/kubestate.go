package servicemap

import (
	"errors"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// KubeCache reads the cluster's pods, workloads and services from the
// runner's informer cache (the one discovery and the k8s watch share), in
// place of the kube_* Prometheus queries.
type KubeCache struct {
	pods         corelisters.PodLister
	services     corelisters.ServiceLister
	deployments  appslisters.DeploymentLister
	statefulSets appslisters.StatefulSetLister
	daemonSets   appslisters.DaemonSetLister
	synced       []cache.InformerSynced
}

// NewKubeCache asks f for the informers the map reads. Call it before f is
// started: the factory starts only the informers asked for by then.
func NewKubeCache(f informers.SharedInformerFactory) *KubeCache {
	core, apps := f.Core().V1(), f.Apps().V1()
	k := &KubeCache{
		pods:         core.Pods().Lister(),
		services:     core.Services().Lister(),
		deployments:  apps.Deployments().Lister(),
		statefulSets: apps.StatefulSets().Lister(),
		daemonSets:   apps.DaemonSets().Lister(),
	}
	k.synced = []cache.InformerSynced{
		core.Pods().Informer().HasSynced,
		core.Services().Informer().HasSynced,
		apps.Deployments().Informer().HasSynced,
		apps.StatefulSets().Informer().HasSynced,
		apps.DaemonSets().Informer().HasSynced,
	}
	return k
}

var errKubeCacheNotSynced = errors.New("kubernetes cache not synced")

// rows returns what the seven kube_* queries return, labelled as
// kube-state-metrics labels them, so build reads them unchanged. The cache
// holds the cluster as it is now, not at the end of the window.
func (k *KubeCache) rows() (map[string][]promResult, error) {
	for _, ok := range k.synced {
		if !ok() {
			return nil, errKubeCacheNotSynced
		}
	}
	out := map[string][]promResult{}
	all := labels.Everything()

	pods, err := k.pods.List(all)
	if err != nil {
		return nil, err
	}
	for _, p := range pods {
		podRows(out, p)
	}

	services, err := k.services.List(all)
	if err != nil {
		return nil, err
	}
	for _, s := range services {
		out["kube_service_info"] = append(out["kube_service_info"], row(map[string]string{
			"namespace": s.Namespace, "service": s.Name, "cluster_ip": s.Spec.ClusterIP,
		}, 1))
	}

	deployments, err := k.deployments.List(all)
	if err != nil {
		return nil, err
	}
	for _, d := range deployments {
		out["kube_deployment_spec_replicas"] = append(out["kube_deployment_spec_replicas"],
			row(map[string]string{"namespace": d.Namespace, "deployment": d.Name}, replicas(d.Spec.Replicas)))
	}
	statefulSets, err := k.statefulSets.List(all)
	if err != nil {
		return nil, err
	}
	for _, s := range statefulSets {
		out["kube_statefulset_replicas"] = append(out["kube_statefulset_replicas"],
			row(map[string]string{"namespace": s.Namespace, "statefulset": s.Name}, replicas(s.Spec.Replicas)))
	}
	daemonSets, err := k.daemonSets.List(all)
	if err != nil {
		return nil, err
	}
	for _, d := range daemonSets {
		out["kube_daemonset_status_desired_number_scheduled"] = append(out["kube_daemonset_status_desired_number_scheduled"],
			row(map[string]string{"namespace": d.Namespace, "daemonset": d.Name}, float64(d.Status.DesiredNumberScheduled)))
	}
	return out, nil
}

// podRows adds a pod's kube_pod_info, kube_pod_labels and
// kube_pod_status_ready rows. Succeeded pods are left out of kube_pod_info,
// as the query does.
func podRows(out map[string][]promResult, p *corev1.Pod) {
	if p.Status.Phase != corev1.PodSucceeded {
		info := map[string]string{
			"namespace": p.Namespace, "pod": p.Name,
			"pod_ip": p.Status.PodIP, "host_ip": p.Status.HostIP,
		}
		if o := owner(p); o != nil {
			info["created_by_kind"], info["created_by_name"] = o.Kind, o.Name
		}
		out["kube_pod_info"] = append(out["kube_pod_info"], row(info, 1))
	}

	lbl := map[string]string{"namespace": p.Namespace, "pod": p.Name}
	for k, v := range p.Labels {
		lbl["label_"+sanitizeLabel(k)] = v
	}
	out["kube_pod_labels"] = append(out["kube_pod_labels"], row(lbl, 1))

	// kube-state-metrics has a series per condition status; the query keeps
	// condition="true", whose value is 1 only while the pod is ready.
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			ready := 0.0
			if c.Status == corev1.ConditionTrue {
				ready = 1
			}
			out["kube_pod_status_ready"] = append(out["kube_pod_status_ready"],
				row(map[string]string{"namespace": p.Namespace, "pod": p.Name, "condition": "true"}, ready))
		}
	}
}

// owner is the pod's controller, else its first owner, as kube-state-metrics
// reports created_by_*.
func owner(p *corev1.Pod) *metav1.OwnerReference {
	if c := metav1.GetControllerOf(p); c != nil {
		return c
	}
	if len(p.OwnerReferences) > 0 {
		return &p.OwnerReferences[0]
	}
	return nil
}

// replicas is spec.replicas, which the API server defaults to 1.
func replicas(r *int32) float64 {
	if r == nil {
		return 1
	}
	return float64(*r)
}

func row(metric map[string]string, v float64) promResult {
	return promResult{Metric: metric, Last: v, HasVal: true}
}

var invalidLabelChars = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// sanitizeLabel turns a Kubernetes label key into the Prometheus label name
// kube-state-metrics gives it ("app.kubernetes.io/name" → "app_kubernetes_io_name").
func sanitizeLabel(k string) string {
	s := invalidLabelChars.ReplaceAllString(k, "_")
	if s != "" && s[0] >= '0' && s[0] <= '9' {
		s = "_" + s
	}
	return s
}
