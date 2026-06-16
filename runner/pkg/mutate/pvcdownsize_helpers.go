package mutate

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/nudgebee/nudgebee-agent/pkg/podexec"
)

func (m *Mutator) logger() *slog.Logger { return slog.Default() }

// getWorkloadByPVC finds the Deployment or StatefulSet that mounts pvcName,
// matching both pod-template volumes and StatefulSet volumeClaimTemplates
// (PVC name pattern {tpl}-{sts}-{ordinal}). Deployments are checked first.
// Returns (workload, kind, nil); a not-found is (nil, "", nil).
func (m *Mutator) getWorkloadByPVC(ctx context.Context, namespace, pvcName string) (any, string, error) {
	deps, err := m.Client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("rightsize_pvc: list deployments: %w", err)
	}
	for i := range deps.Items {
		if podSpecMountsPVC(&deps.Items[i].Spec.Template.Spec, pvcName) {
			return &deps.Items[i], "Deployment", nil
		}
	}
	stss, err := m.Client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("rightsize_pvc: list statefulsets: %w", err)
	}
	for i := range stss.Items {
		sts := &stss.Items[i]
		if podSpecMountsPVC(&sts.Spec.Template.Spec, pvcName) {
			return sts, "StatefulSet", nil
		}
		for _, tpl := range sts.Spec.VolumeClaimTemplates {
			prefix := fmt.Sprintf("%s-%s-", tpl.Name, sts.Name)
			if suffix, ok := strings.CutPrefix(pvcName, prefix); ok && isAllDigits(suffix) {
				return sts, "StatefulSet", nil
			}
		}
	}
	return nil, "", nil
}

func podSpecMountsPVC(spec *corev1.PodSpec, pvcName string) bool {
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == pvcName {
			return true
		}
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func labelSelectorString(sel *metav1.LabelSelector) string {
	if sel == nil {
		return ""
	}
	keys := make([]string, 0, len(sel.MatchLabels))
	for k := range sel.MatchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+sel.MatchLabels[k])
	}
	return strings.Join(parts, ",")
}

// scaleWorkloadTyped sets spec.replicas on a Deployment/StatefulSet via
// read-modify-update with conflict retries. Returns the prior replica count.
func (m *Mutator) scaleWorkloadTyped(ctx context.Context, kind, namespace, name string, replicas int32) (int32, error) {
	var old int32
	for attempt := 0; attempt < 3; attempt++ {
		var err error
		switch kind {
		case "Deployment":
			var d *appsv1.Deployment
			d, err = m.Client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				old = derefReplicas(d.Spec.Replicas)
				d.Spec.Replicas = &replicas
				_, err = m.Client.AppsV1().Deployments(namespace).Update(ctx, d, metav1.UpdateOptions{})
			}
		case "StatefulSet":
			var s *appsv1.StatefulSet
			s, err = m.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				old = derefReplicas(s.Spec.Replicas)
				s.Spec.Replicas = &replicas
				_, err = m.Client.AppsV1().StatefulSets(namespace).Update(ctx, s, metav1.UpdateOptions{})
			}
		default:
			return 0, fmt.Errorf("rightsize_pvc: unsupported kind %q for scale", kind)
		}
		if err == nil {
			return old, nil
		}
		if apierrors.IsConflict(err) && attempt < 2 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
			continue
		}
		return 0, fmt.Errorf("rightsize_pvc: scale %s %s/%s to %d: %w", kind, namespace, name, replicas, err)
	}
	return 0, fmt.Errorf("rightsize_pvc: scale %s %s/%s: exhausted retries", kind, namespace, name)
}

func derefReplicas(r *int32) int32 {
	if r == nil {
		return 1
	}
	return *r
}

// waitForPodsTerminated blocks until no pods match selector, polling every
// m.pollInterval() up to m.podTermTimeout().
func (m *Mutator) waitForPodsTerminated(ctx context.Context, namespace, selector string) error {
	deadline := time.Now().Add(m.podTermTimeout())
	for {
		pods, err := m.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return fmt.Errorf("rightsize_pvc: list pods %q: %w", selector, err)
		}
		if len(pods.Items) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("rightsize_pvc: timeout waiting for pods %q to terminate", selector)
		}
		if err := sleepCtx(ctx, m.pollInterval()); err != nil {
			return err
		}
	}
}

// createPVCFromSpec creates a PVC named pvcName cloning src's spec at the new
// size, clearing fields that bind it to the old PV. Preserves original labels
// and user annotations, adds the scaler marker annotation.
func (m *Mutator) createPVCFromSpec(ctx context.Context, namespace, pvcName string, src *corev1.PersistentVolumeClaim, size resource.Quantity) error {
	spec := *src.Spec.DeepCopy()
	if spec.Resources.Requests == nil {
		spec.Resources.Requests = corev1.ResourceList{}
	}
	spec.Resources.Requests[corev1.ResourceStorage] = size
	spec.VolumeName = ""
	spec.Selector = nil
	spec.DataSource = nil
	spec.DataSourceRef = nil

	annotations := filterUserAnnotations(src.Annotations)
	annotations[pvcScalerAnnotation] = pvcScalerAnnotation
	var labels map[string]string
	if len(src.Labels) > 0 {
		labels = make(map[string]string, len(src.Labels))
		for k, v := range src.Labels {
			labels[k] = v
		}
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        pvcName,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: spec,
	}
	if _, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("rightsize_pvc: create pvc %s/%s: %w", namespace, pvcName, err)
	}
	return nil
}

// copyData spins up a transient mover pod mounting src→/srcData and dst→/dstData,
// runs `cp -a`, verifies the COPY_SUCCESS marker + exit 0, then deletes the pod.
func (m *Mutator) copyData(ctx context.Context, namespace, srcPVC, dstPVC, role string) error {
	podName := fmt.Sprintf("%s-%s-%s", srcPVC, role, rand.String(8))
	pod := moverPod(podName, srcPVC, dstPVC)
	if _, err := m.Client.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("rightsize_pvc: create mover pod %s: %w", podName, err)
	}
	defer m.deletePodBestEffort(context.WithoutCancel(ctx), namespace, podName)

	if err := m.waitForPodRunning(ctx, namespace, podName); err != nil {
		return err
	}

	const cpCommand = `set -e; if [ -z "$(ls -A /srcData 2>/dev/null)" ]; then echo "COPY_SUCCESS: source is empty"; else timeout 300 cp -a /srcData/. /dstData/ && echo "COPY_SUCCESS: data copied"; fi`
	res, err := m.exec.Exec(ctx, &podexec.Request{
		Namespace: namespace,
		Pod:       podName,
		Container: "data-copier",
		Command:   []string{"/bin/sh", "-c", cpCommand},
		Timeout:   m.opTimeout(),
	})
	if err != nil {
		return fmt.Errorf("rightsize_pvc: exec cp in %s: %w", podName, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("rightsize_pvc: data copy %s→%s exit %d (stderr: %s)", srcPVC, dstPVC, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if !strings.Contains(res.Stdout, "COPY_SUCCESS") {
		return fmt.Errorf("rightsize_pvc: data copy %s→%s produced no COPY_SUCCESS marker (stdout: %s)", srcPVC, dstPVC, strings.TrimSpace(res.Stdout))
	}
	return nil
}

func moverPod(name, srcPVC, dstPVC string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "data-copier",
				Image:           moverImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/bin/bash", "-c", "sleep infinity"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "src-vol", MountPath: "/srcData"},
					{Name: "dst-vol", MountPath: "/dstData"},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "src-vol", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: srcPVC}}},
				{Name: "dst-vol", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: dstPVC}}},
			},
		},
	}
}

func (m *Mutator) waitForPodRunning(ctx context.Context, namespace, podName string) error {
	deadline := time.Now().Add(m.opTimeout())
	for {
		pod, err := m.Client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("rightsize_pvc: get mover pod %s: %w", podName, err)
		}
		switch pod.Status.Phase {
		case corev1.PodRunning:
			return nil
		case corev1.PodFailed:
			return fmt.Errorf("rightsize_pvc: mover pod %s entered Failed", podName)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("rightsize_pvc: mover pod %s not Running within %s (last phase %s)", podName, m.opTimeout(), pod.Status.Phase)
		}
		if err := sleepCtx(ctx, m.pollInterval()); err != nil {
			return err
		}
	}
}

func (m *Mutator) repointDeploymentPVC(ctx context.Context, namespace, name, oldPVC, newPVC string) error {
	for attempt := 0; attempt < 5; attempt++ {
		d, err := m.Client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("rightsize_pvc: get deployment %s: %w", name, err)
		}
		found := false
		for i := range d.Spec.Template.Spec.Volumes {
			v := &d.Spec.Template.Spec.Volumes[i]
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == oldPVC {
				v.PersistentVolumeClaim.ClaimName = newPVC
				found = true
			}
		}
		if !found {
			return fmt.Errorf("rightsize_pvc: deployment %s has no volume referencing pvc %s", name, oldPVC)
		}
		_, err = m.Client.AppsV1().Deployments(namespace).Update(ctx, d, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}
		if apierrors.IsConflict(err) && attempt < 4 {
			if err := sleepCtx(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("rightsize_pvc: repoint deployment %s pvc: %w", name, err)
	}
	return fmt.Errorf("rightsize_pvc: repoint deployment %s: exhausted retries", name)
}

func (m *Mutator) patchPVReclaim(ctx context.Context, pvName string, policy corev1.PersistentVolumeReclaimPolicy) error {
	patch := fmt.Appendf(nil, `{"spec":{"persistentVolumeReclaimPolicy":%q}}`, string(policy))
	if _, err := m.Client.CoreV1().PersistentVolumes().Patch(ctx, pvName, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("rightsize_pvc: patch pv %s reclaim to %s: %w", pvName, policy, err)
	}
	return nil
}

func (m *Mutator) deletePVC(ctx context.Context, namespace, name string) error {
	if err := m.Client.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("rightsize_pvc: delete pvc %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (m *Mutator) waitForPVCDeletion(ctx context.Context, namespace, name string) error {
	deadline := time.Now().Add(m.opTimeout())
	for {
		_, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("rightsize_pvc: poll pvc %s/%s deletion: %w", namespace, name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("rightsize_pvc: timeout waiting for pvc %s/%s deletion", namespace, name)
		}
		if err := sleepCtx(ctx, m.pollInterval()); err != nil {
			return err
		}
	}
}

// deletePVBestEffort logs but never fails — an orphaned Released PV is left for
// manual cleanup rather than aborting an otherwise-complete migration.
func (m *Mutator) deletePVBestEffort(ctx context.Context, name string) {
	if err := m.Client.CoreV1().PersistentVolumes().Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		m.logger().Warn("rightsize_pvc: failed to delete orphaned PV; manual cleanup may be needed", "pv", name, "err", err)
	}
}

func (m *Mutator) deletePodBestEffort(ctx context.Context, namespace, name string) {
	if err := m.Client.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		m.logger().Warn("rightsize_pvc: failed to delete mover pod", "pod", name, "err", err)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// ---- parity snapshot ----

type pvcSnapshot struct {
	labels          map[string]string
	userAnnotations map[string]string
	accessModes     []string
	storageClass    string
	volumeMode      string
}

func newPVCSnapshot(pvc *corev1.PersistentVolumeClaim) pvcSnapshot {
	s := pvcSnapshot{
		labels:          map[string]string{},
		userAnnotations: filterUserAnnotations(pvc.Annotations),
	}
	for k, v := range pvc.Labels {
		s.labels[k] = v
	}
	for _, am := range pvc.Spec.AccessModes {
		s.accessModes = append(s.accessModes, string(am))
	}
	sort.Strings(s.accessModes)
	if pvc.Spec.StorageClassName != nil {
		s.storageClass = *pvc.Spec.StorageClassName
	}
	if pvc.Spec.VolumeMode != nil {
		s.volumeMode = string(*pvc.Spec.VolumeMode)
	}
	return s
}

// verifyParity compares metadata/spec of the new PVC against the snapshot and
// the expected size. Returns the list of mismatches (empty == parity holds).
func (s pvcSnapshot) verifyParity(newPVC *corev1.PersistentVolumeClaim, expected resource.Quantity) []string {
	var mm []string
	for k, v := range s.labels {
		if newPVC.Labels[k] != v {
			mm = append(mm, fmt.Sprintf("label %q: want %q got %q", k, v, newPVC.Labels[k]))
		}
	}
	newAnnots := filterUserAnnotations(newPVC.Annotations)
	for k, v := range s.userAnnotations {
		if newAnnots[k] != v {
			mm = append(mm, fmt.Sprintf("annotation %q: want %q got %q", k, v, newAnnots[k]))
		}
	}
	var newModes []string
	for _, am := range newPVC.Spec.AccessModes {
		newModes = append(newModes, string(am))
	}
	sort.Strings(newModes)
	if strings.Join(newModes, ",") != strings.Join(s.accessModes, ",") {
		mm = append(mm, fmt.Sprintf("accessModes: want %v got %v", s.accessModes, newModes))
	}
	newSC := ""
	if newPVC.Spec.StorageClassName != nil {
		newSC = *newPVC.Spec.StorageClassName
	}
	if newSC != s.storageClass {
		mm = append(mm, fmt.Sprintf("storageClassName: want %q got %q", s.storageClass, newSC))
	}
	newVM := ""
	if newPVC.Spec.VolumeMode != nil {
		newVM = string(*newPVC.Spec.VolumeMode)
	}
	if newVM != s.volumeMode {
		mm = append(mm, fmt.Sprintf("volumeMode: want %q got %q", s.volumeMode, newVM))
	}
	got := newPVC.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.Cmp(expected) != 0 {
		mm = append(mm, fmt.Sprintf("storage: want %s got %s", expected.String(), got.String()))
	}
	return mm
}

func filterUserAnnotations(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if k == pvcScalerAnnotation || k8sInternalAnnotation.MatchString(k) {
			continue
		}
		out[k] = v
	}
	return out
}
