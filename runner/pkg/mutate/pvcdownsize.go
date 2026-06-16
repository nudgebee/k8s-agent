// Package mutate — rightsize_pvc downsize migration.
//
// PVs cannot shrink in place, so downsizing a PVC means copying its data onto a
// smaller volume and repointing the workload. This is a faithful port of the
// legacy robusta volume_rightsize_analyzer downsize path. It is data-
// destructive; the ordering and the explicit point-of-no-return below are
// load-bearing — do not reorder.
//
// Strategy differs by workload kind:
//
//	Deployment  — single copy + rename. Create a new PVC ({pvc}-downsized-xxxx),
//	              copy data, repoint the Deployment's volume to it, then delete
//	              the original. The original PVC is the safety net until the
//	              workload runs on the new one (point of no return = repoint).
//
//	StatefulSet — name-preserving double copy. volumeClaimTemplate PVC names are
//	              fixed ({tpl}-{sts}-{ordinal}), so we copy original→temp, delete
//	              the original, recreate it at the new size, copy temp→original.
//	              Point of no return = deleting the original PVC; before that we
//	              flip the PV reclaim policy to Retain so a second copy survives.
//
// The data copy runs in a transient "ubuntu" mover pod (cp -a) in the
// workload's namespace, exec'd over SPDY. The agent ServiceAccount therefore
// needs: pods create/get/delete + pods/exec; pvc get/create/delete; pv
// get/patch/delete; deployments/statefulsets get/update/list; storageclasses
// get.
package mutate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
)

const (
	defaultMoverImage     = "ubuntu"
	pvcScalerAnnotation   = "NudgebeeDiskScaler" // key == value, marks PVCs we create
	defaultOpTimeout      = 300 * time.Second    // per-stage ceiling (pvc-delete, pod-running)
	defaultPodTermTimeout = 300 * time.Second
	defaultPollInterval   = 5 * time.Second
	defaultCopyTimeout    = 30 * time.Minute // used only when ctx carries no deadline
)

// moverImageName resolves the data-mover image: explicit override (tests) >
// MOVER_IMAGE env (air-gapped clusters that mirror images) > "ubuntu".
func (m *Mutator) moverImageName() string {
	if m.moverImageOverride != "" {
		return m.moverImageOverride
	}
	if v := os.Getenv("MOVER_IMAGE"); v != "" {
		return v
	}
	return defaultMoverImage
}

// copyTimeout bounds a single data copy. It uses the remaining ctx budget (the
// LongTaskTimeout the poller applies), minus headroom for the surrounding
// scale/cleanup, so a large volume isn't cut off at a fixed 5m.
func (m *Mutator) copyTimeout(ctx context.Context) time.Duration {
	if m.copyTimeoutOverride > 0 {
		return m.copyTimeoutOverride
	}
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl) - time.Minute; d > 0 {
			return d
		}
	}
	return defaultCopyTimeout
}

// Timeout accessors fall back to the defaults above; tests set the unexported
// Mutator fields to keep migration unit tests fast.
func (m *Mutator) opTimeout() time.Duration {
	if m.opTimeoutOverride > 0 {
		return m.opTimeoutOverride
	}
	return defaultOpTimeout
}

func (m *Mutator) podTermTimeout() time.Duration {
	if m.podTermTimeoutOverride > 0 {
		return m.podTermTimeoutOverride
	}
	return defaultPodTermTimeout
}

func (m *Mutator) pollInterval() time.Duration {
	if m.pollIntervalOverride > 0 {
		return m.pollIntervalOverride
	}
	return defaultPollInterval
}

// k8sInternalAnnotation matches keys we must not carry across (provisioner /
// binding bookkeeping). Mirrors the legacy regex set.
var k8sInternalAnnotation = regexp.MustCompile(`\.(beta\.)?kubernetes\.io(/.*)?$`)

// DownsizePVC migrates pvcName in namespace down to target. Returns a
// {success, message} map on success; an error (→ task FAILED) otherwise.
func (m *Mutator) DownsizePVC(ctx context.Context, namespace, pvcName string, target resource.Quantity) (any, error) {
	if m.Client == nil {
		return nil, errors.New("mutate: client not configured")
	}
	if m.exec == nil {
		return nil, errors.New("rightsize_pvc: downsize requires pod-exec capability (not configured on this agent)")
	}

	pvc, pv, err := m.validateDownsizePrereqs(ctx, namespace, pvcName)
	if err != nil {
		return nil, err
	}
	snap := newPVCSnapshot(pvc)

	_, kind, err := m.getWorkloadByPVC(ctx, namespace, pvcName)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "Deployment":
		if err := m.downsizeDeployment(ctx, namespace, pvcName, pvc, pv, target, snap); err != nil {
			return nil, err
		}
	case "StatefulSet":
		if err := m.downsizeStatefulSet(ctx, namespace, pvcName, pvc, pv, target, snap); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("rightsize_pvc: no Deployment or StatefulSet found using PVC %q", pvcName)
	}
	return map[string]any{
		"success": true,
		"message": fmt.Sprintf("pvc %s/%s downsized to %s", namespace, pvcName, target.String()),
	}, nil
}

// validateDownsizePrereqs hard-fails unless the PVC is Bound, its PV exists and
// is not hostPath, and its StorageClass exists.
func (m *Mutator) validateDownsizePrereqs(ctx context.Context, namespace, pvcName string) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume, error) {
	pvc, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("rightsize_pvc: get pvc %s/%s: %w", namespace, pvcName, err)
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		return nil, nil, fmt.Errorf("rightsize_pvc: pvc %s/%s is %q, must be Bound to downsize", namespace, pvcName, pvc.Status.Phase)
	}
	if pvc.Spec.VolumeName == "" {
		return nil, nil, fmt.Errorf("rightsize_pvc: pvc %s/%s has no volumeName", namespace, pvcName)
	}
	pv, err := m.Client.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("rightsize_pvc: get pv %s: %w", pvc.Spec.VolumeName, err)
	}
	if pv.Spec.HostPath != nil {
		return nil, nil, fmt.Errorf("rightsize_pvc: pv %s is hostPath — downsize unsupported", pv.Name)
	}
	scName := ""
	if pvc.Spec.StorageClassName != nil {
		scName = *pvc.Spec.StorageClassName
	}
	if scName == "" {
		return nil, nil, fmt.Errorf("rightsize_pvc: pvc %s/%s has no storageClassName", namespace, pvcName)
	}
	if _, err := m.Client.StorageV1().StorageClasses().Get(ctx, scName, metav1.GetOptions{}); err != nil {
		return nil, nil, fmt.Errorf("rightsize_pvc: get storageclass %q: %w", scName, err)
	}
	return pvc, pv, nil
}

// downsizeDeployment: copy → repoint → delete original. Point of no return is
// the volume repoint at repointDeploymentPVC.
func (m *Mutator) downsizeDeployment(ctx context.Context, namespace, pvcName string, pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume, target resource.Quantity, snap pvcSnapshot) (retErr error) {
	dep, kind, err := m.getWorkloadByPVC(ctx, namespace, pvcName)
	if err != nil || kind != "Deployment" {
		return fmt.Errorf("rightsize_pvc: no Deployment found using PVC %q", pvcName)
	}
	deployment := dep.(*appsv1.Deployment)
	name := deployment.Name
	selector := labelSelectorString(deployment.Spec.Selector)
	newPVCName := fmt.Sprintf("%s-downsized-%s", pvcName, rand.String(8))

	var oldReplicas *int32
	newPVCCreated := false
	deploymentUpdated := false

	defer func() {
		if retErr == nil {
			return
		}
		if deploymentUpdated {
			// Past the point of no return: leave both PVCs in place; the
			// Deployment already runs on the new one. Manual cleanup only.
			m.logger().Error("rightsize_pvc: failure after Deployment repoint; original PVC retained for recovery",
				"deployment", name, "namespace", namespace, "new_pvc", newPVCName, "err", retErr)
			return
		}
		if newPVCCreated {
			_ = m.deletePVC(ctx, namespace, newPVCName)
		}
		if oldReplicas != nil {
			if _, err := m.scaleWorkloadTyped(ctx, "Deployment", namespace, name, *oldReplicas); err != nil {
				m.logger().Error("rightsize_pvc: CRITICAL: failed to restore replicas after rollback", "deployment", name, "err", err)
			}
		}
	}()

	// Phase A — non-destructive (original PVC untouched).
	r, err := m.scaleWorkloadTyped(ctx, "Deployment", namespace, name, 0)
	if err != nil {
		return err
	}
	oldReplicas = &r
	if err := m.waitForPodsTerminated(ctx, namespace, selector); err != nil {
		return err
	}
	if err := m.createPVCFromSpec(ctx, namespace, newPVCName, pvc, target); err != nil {
		return err
	}
	newPVCCreated = true
	if err := m.copyData(ctx, namespace, pvcName, newPVCName, "copier"); err != nil {
		return err
	}
	newPVCObj, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, newPVCName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("rightsize_pvc: re-read new pvc %s: %w", newPVCName, err)
	}
	if mm := snap.verifyParity(newPVCObj, target); len(mm) > 0 {
		return fmt.Errorf("rightsize_pvc: config parity check failed for new pvc %s: %v", newPVCName, mm)
	}

	// Phase B — repoint (point of no return).
	if err := m.repointDeploymentPVC(ctx, namespace, name, pvcName, newPVCName); err != nil {
		return err
	}
	deploymentUpdated = true
	if _, err := m.scaleWorkloadTyped(ctx, "Deployment", namespace, name, *oldReplicas); err != nil {
		return err
	}
	oldReplicas = nil // replicas restored; don't re-scale in rollback

	// Phase C — cleanup (workload is on the new PVC).
	if err := m.deletePVC(ctx, namespace, pvcName); err != nil {
		return err
	}
	if err := m.waitForPVCDeletion(ctx, namespace, pvcName); err != nil {
		return err
	}
	if pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimRetain {
		m.deletePVBestEffort(ctx, pv.Name)
	}
	return nil
}

// downsizeStatefulSet: copy original→temp, delete original, recreate at new
// size, copy temp→original. Point of no return is deleting the original PVC;
// the PV reclaim policy is flipped to Retain beforehand so its data survives.
func (m *Mutator) downsizeStatefulSet(ctx context.Context, namespace, pvcName string, pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume, target resource.Quantity, snap pvcSnapshot) (retErr error) {
	sts, kind, err := m.getWorkloadByPVC(ctx, namespace, pvcName)
	if err != nil || kind != "StatefulSet" {
		return fmt.Errorf("rightsize_pvc: no StatefulSet found using PVC %q", pvcName)
	}
	statefulSet := sts.(*appsv1.StatefulSet)
	name := statefulSet.Name
	selector := labelSelectorString(statefulSet.Spec.Selector)
	tempPVCName := fmt.Sprintf("%s-tmp-%s", pvcName, rand.String(8))
	origReclaim := pv.Spec.PersistentVolumeReclaimPolicy

	var oldReplicas *int32
	reclaimPatched := false
	tempPVCCreated := false
	originalPVCDeleted := false

	defer func() {
		if retErr == nil {
			return
		}
		if originalPVCDeleted {
			// Past the point of no return. Two safe copies remain: the temp
			// PVC and the retained (Released) PV. Do NOT delete them, do NOT
			// restore reclaim, do NOT scale up — the workload must stay down.
			m.logger().Error("rightsize_pvc: CRITICAL: original PVC deleted but downsize incomplete; data safe on temp PVC and retained PV — manual recovery required",
				"statefulset", name, "namespace", namespace, "pvc", pvcName, "temp_pvc", tempPVCName, "pv", pv.Name, "err", retErr)
			return
		}
		if tempPVCCreated {
			_ = m.deletePVC(ctx, namespace, tempPVCName)
		}
		if reclaimPatched {
			if err := m.patchPVReclaim(ctx, pv.Name, origReclaim); err != nil {
				m.logger().Error("rightsize_pvc: failed to restore PV reclaim policy on rollback", "pv", pv.Name, "err", err)
			}
		}
		if oldReplicas != nil {
			if _, err := m.scaleWorkloadTyped(ctx, "StatefulSet", namespace, name, *oldReplicas); err != nil {
				m.logger().Error("rightsize_pvc: CRITICAL: failed to restore replicas after rollback", "statefulset", name, "err", err)
			}
		}
	}()

	// Phase A — non-destructive.
	r, err := m.scaleWorkloadTyped(ctx, "StatefulSet", namespace, name, 0)
	if err != nil {
		return err
	}
	oldReplicas = &r
	if err := m.waitForPodsTerminated(ctx, namespace, selector); err != nil {
		return err
	}
	if origReclaim != corev1.PersistentVolumeReclaimRetain {
		if err := m.patchPVReclaim(ctx, pv.Name, corev1.PersistentVolumeReclaimRetain); err != nil {
			return err
		}
		reclaimPatched = true
	}
	if err := m.createPVCFromSpec(ctx, namespace, tempPVCName, pvc, target); err != nil {
		return err
	}
	tempPVCCreated = true
	if err := m.copyData(ctx, namespace, pvcName, tempPVCName, "cp1"); err != nil {
		return err
	}

	// Phase B — destructive (point of no return).
	if err := m.deletePVC(ctx, namespace, pvcName); err != nil {
		return err
	}
	if err := m.waitForPVCDeletion(ctx, namespace, pvcName); err != nil {
		return err
	}
	originalPVCDeleted = true
	if err := m.createPVCFromSpec(ctx, namespace, pvcName, pvc, target); err != nil {
		return err
	}
	if err := m.copyData(ctx, namespace, tempPVCName, pvcName, "cp2"); err != nil {
		return err
	}
	finalPVC, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("rightsize_pvc: re-read pvc %s: %w", pvcName, err)
	}
	if mm := snap.verifyParity(finalPVC, target); len(mm) > 0 {
		return fmt.Errorf("rightsize_pvc: config parity check failed for pvc %s: %v", pvcName, mm)
	}
	if _, err := m.scaleWorkloadTyped(ctx, "StatefulSet", namespace, name, *oldReplicas); err != nil {
		return err
	}
	oldReplicas = nil

	// Phase C — cleanup.
	if err := m.deletePVC(ctx, namespace, tempPVCName); err != nil {
		return err
	}
	if err := m.waitForPVCDeletion(ctx, namespace, tempPVCName); err != nil {
		return err
	}
	tempPVCCreated = false
	m.deletePVBestEffort(ctx, pv.Name) // orphaned Released PV
	return nil
}
