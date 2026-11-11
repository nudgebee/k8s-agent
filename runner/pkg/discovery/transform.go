package discovery

import (
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// discoveryTransform is the cache.TransformFunc applied to every discovery
// informer. It strips fields that NO converter in converters.go / service.go
// reads, cutting per-object heap at scale (100k+ pods). Invariants:
//
//   - never remove a field any converter reads (verified against converters.go)
//   - idempotent: may run more than once on the same object across resync;
//     assigning nil to an already-nil field is a no-op
//   - pass unknown types and cache.DeletedFinalStateUnknown through untouched
//     (return obj, nil) — never panic
//
// The transform mutates the object that lives in the shared store and is handed
// to every reader (converters + podLookup/replicaSetLookup), so it only ever
// DELETES fields nobody reads; it never reassigns a field a reader needs.
func discoveryTransform(obj any) (any, error) {
	switch o := obj.(type) {
	case *corev1.Pod:
		trimMeta(&o.ObjectMeta)
		// Pod converter reads: the WHOLE of Status (it ships as status_dict, which
		// the collector stores as meta.status_info), plus Spec.NodeName,
		// Spec.Containers[].{Name,Image}, OwnerReferences, Labels.
		//
		// Status.InitContainerStatuses and Status.EphemeralContainerStatuses used to
		// be stripped here, back when the converter only read a handful of Status
		// fields. They are readers now -- the pod-detail panel indexes
		// status_info.initContainerStatuses -- so stripping them would violate this
		// function's contract of only deleting what nobody reads. Measured on a
		// 420-pod cluster, keeping them costs 129 KiB (108 pods) and 1 KiB (1 pod)
		// in the informer store, against 458 KiB for the container statuses already
		// retained.
		o.Spec.InitContainers = nil
		o.Spec.EphemeralContainers = nil
		o.Spec.Volumes = nil     // pod converter emits no volumes (only templates do)
		o.Spec.Tolerations = nil // ditto tolerations
		for i := range o.Spec.Containers {
			trimContainer(&o.Spec.Containers[i])
		}
	case *appsv1.Deployment:
		trimMeta(&o.ObjectMeta)
		trimTemplate(&o.Spec.Template)
	case *appsv1.StatefulSet:
		trimMeta(&o.ObjectMeta)
		trimTemplate(&o.Spec.Template)
	case *appsv1.DaemonSet:
		trimMeta(&o.ObjectMeta)
		trimTemplate(&o.Spec.Template)
	case *appsv1.ReplicaSet:
		trimMeta(&o.ObjectMeta)
		trimTemplate(&o.Spec.Template)
	case *batchv1.Job:
		trimMeta(&o.ObjectMeta)
		trimTemplate(&o.Spec.Template)
	case *batchv1.CronJob:
		trimMeta(&o.ObjectMeta)
		trimTemplate(&o.Spec.JobTemplate.Spec.Template)
	case *corev1.Node:
		trimMeta(&o.ObjectMeta)
		// convertNode reads Addresses/Capacity/Allocatable/Conditions/NodeInfo
		// + Spec.Taints. These are large and unread:
		o.Status.Images = nil
		o.Status.VolumesInUse = nil
		o.Status.VolumesAttached = nil
	case *corev1.Namespace:
		trimMeta(&o.ObjectMeta)
	case *corev1.Secret:
		// Helm-release path: convertHelmReleaseSecret reads Data["release"];
		// keep Data, only drop the metadata bloat.
		trimMeta(&o.ObjectMeta)
	}
	// Unknown types / cache.DeletedFinalStateUnknown fall through unchanged.
	return obj, nil
}

// TransformFunc returns the transform for the informer factory discovery
// runs on. With forMatchers false it is discoveryTransform. With it true the
// factory is shared with the trigger matchers (pkg/k8swatch), whose config-
// change matcher diffs whole pod templates and whose evidence readers read
// them, so the transform keeps more:
//
//   - Deployments, StatefulSets, DaemonSets keep their pod template whole
//     (only managedFields goes). These are few next to Pods.
//   - Pods keep init and ephemeral containers, trimmed like regular ones: the
//     OOM matcher reads their memory requests and limits.
//
// Discovery's converters read named fields only, so its output is the same
// either way.
func TransformFunc(forMatchers bool) func(any) (any, error) {
	if !forMatchers {
		return discoveryTransform
	}
	return func(obj any) (any, error) {
		switch o := obj.(type) {
		case *appsv1.Deployment:
			trimMeta(&o.ObjectMeta)
			o.Spec.Template.ManagedFields = nil
			return o, nil
		case *appsv1.StatefulSet:
			trimMeta(&o.ObjectMeta)
			o.Spec.Template.ManagedFields = nil
			return o, nil
		case *appsv1.DaemonSet:
			trimMeta(&o.ObjectMeta)
			o.Spec.Template.ManagedFields = nil
			return o, nil
		case *corev1.Pod:
			init, eph := o.Spec.InitContainers, o.Spec.EphemeralContainers
			out, err := discoveryTransform(o)
			o.Spec.InitContainers = init
			for i := range o.Spec.InitContainers {
				trimContainer(&o.Spec.InitContainers[i])
			}
			o.Spec.EphemeralContainers = eph
			for i := range o.Spec.EphemeralContainers {
				trimEphemeralContainer(&o.Spec.EphemeralContainers[i])
			}
			return out, err
		}
		return discoveryTransform(obj)
	}
}

// trimEphemeralContainer is trimContainer for an ephemeral container, which
// carries the same fields under a different type.
func trimEphemeralContainer(c *corev1.EphemeralContainer) {
	c.Env = nil
	c.EnvFrom = nil
	c.VolumeMounts = nil
	c.VolumeDevices = nil
	c.LivenessProbe = nil
	c.ReadinessProbe = nil
	c.StartupProbe = nil
	c.Lifecycle = nil
}

// trimMeta drops the metadata fields no converter reads. ManagedFields is the
// biggest single win across every type and is never emitted on the wire.
//
// NOTE: annotations are deliberately NOT touched — the workload/node/namespace
// converters emit the annotation map (config.annotations) through
// wireAnnotations, which drops only the applied-copy keys, so removing any
// other key here would change the wire output. Keeping the transform strictly
// output-preserving is what lets it run unconditionally.
func trimMeta(m *metav1.ObjectMeta) {
	m.ManagedFields = nil
}

// trimTemplate strips container-level bloat from a PodTemplateSpec while
// preserving everything the workload/job converters read off the template:
// Containers[].{Name,Image,Resources} (jobDataDict reads Resources),
// Volumes, Tolerations, Affinity, NodeSelector, ServiceAccountName.
func trimTemplate(t *corev1.PodTemplateSpec) {
	t.ManagedFields = nil
	t.Spec.InitContainers = nil
	t.Spec.EphemeralContainers = nil
	for i := range t.Spec.Containers {
		trimContainer(&t.Spec.Containers[i])
	}
}

// trimContainer nils the heavy per-container fields no converter reads. Keeps
// Name, Image, Resources (used by jobDataDict), and Ports.
func trimContainer(c *corev1.Container) {
	c.Env = nil
	c.EnvFrom = nil
	c.VolumeMounts = nil
	c.VolumeDevices = nil
	c.LivenessProbe = nil
	c.ReadinessProbe = nil
	c.StartupProbe = nil
	c.Lifecycle = nil
}
