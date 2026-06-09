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
		// Pod converter reads: Status.{Phase,QOSClass,PodIP,Conditions,
		// ContainerStatuses[].{Name,Ready,RestartCount}}, Spec.NodeName,
		// Spec.Containers[].{Name,Image}, OwnerReferences, Labels.
		o.Status.InitContainerStatuses = nil
		o.Status.EphemeralContainerStatuses = nil
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

// trimMeta drops the metadata fields no converter reads. ManagedFields is the
// biggest single win across every type and is never emitted on the wire.
//
// NOTE: annotations are deliberately NOT touched — the workload/node/namespace
// converters emit the full annotation map (config.annotations), so removing any
// annotation key (even the bulky last-applied-configuration) would change the
// wire output. Keeping the transform strictly output-preserving is what lets it
// run unconditionally; trimming annotations would require collector agreement.
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
