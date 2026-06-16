// Package mutate — rightsize_pvc (expand) and volume_delete.
//
// Both arrive only via the agent_task poller (trusted path).
//
// rightsize_pvc resizes a PVC. Expansion is in-place (verify the StorageClass
// allows expansion, patch spec.resources.requests.storage). Downsize cannot
// shrink a PV in place, so it routes to the copy-migration in pvcdownsize.go.
//
// volume_delete removes an unused PersistentVolume. The payload carries only
// `name` (no namespace) because the api-server's unused_pvc recommendation is a
// serialized PV manifest — `name` is the cluster-scoped PV name. Mirroring the
// legacy robusta action, we delete the bound PVC (resolved via the PV's
// spec.claimRef) first, then the PV itself; both deletes are 404-tolerant.
package mutate

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// RightsizePVC is the rightsize_pvc dispatcher: it compares the PVC's current
// provisioned size against the target and routes to in-place expansion or the
// downsize migration. size accepts the api-server's two wire forms
// "20.000000Gi" (%f) and "20Gi". No-op when already at the target.
func (m *Mutator) RightsizePVC(ctx context.Context, namespace, name, size string) (any, error) {
	if m.Client == nil {
		return nil, errors.New("mutate: client not configured")
	}
	if namespace == "" || name == "" || size == "" {
		return nil, errors.New("mutate: namespace, name and size required")
	}
	target, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("rightsize_pvc: invalid size %q: %w", size, err)
	}

	pvcs := m.Client.CoreV1().PersistentVolumeClaims(namespace)
	pvc, err := pvcs.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("rightsize_pvc: get pvc %s/%s: %w", namespace, name, err)
	}

	// Direction is decided on the provisioned capacity (what exists), matching
	// the legacy action; fall back to the request before the PVC is bound.
	current := pvc.Status.Capacity[corev1.ResourceStorage]
	if current.IsZero() {
		current = pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	}
	switch target.Cmp(current) {
	case 0:
		return map[string]any{
			"success": true,
			"message": fmt.Sprintf("pvc %s/%s already at %s", namespace, name, current.String()),
		}, nil
	case -1:
		// PVs can't shrink in place — copy data onto a smaller volume.
		return m.DownsizePVC(ctx, namespace, name, target)
	}

	// Expansion requires the StorageClass to allow it. An unset
	// StorageClassName means the default class / a statically-provisioned PV;
	// we can't resolve allowVolumeExpansion, so attempt the patch and let the
	// apiserver reject it if unsupported.
	if scName := pvc.Spec.StorageClassName; scName != nil && *scName != "" {
		sc, scErr := m.Client.StorageV1().StorageClasses().Get(ctx, *scName, metav1.GetOptions{})
		if scErr != nil {
			return nil, fmt.Errorf("rightsize_pvc: get storageclass %q: %w", *scName, scErr)
		}
		if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
			return nil, fmt.Errorf("rightsize_pvc: storageclass %q does not allow volume expansion", *scName)
		}
	}

	patch := fmt.Appendf(nil, `{"spec":{"resources":{"requests":{"storage":%q}}}}`, target.String())
	if _, err := pvcs.Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return nil, fmt.Errorf("rightsize_pvc: patch pvc %s/%s to %s: %w", namespace, name, target.String(), err)
	}
	return map[string]any{
		"success": true,
		"message": fmt.Sprintf("pvc %s/%s expansion to %s requested", namespace, name, target.String()),
	}, nil
}

// DeleteVolume deletes a PersistentVolume by (cluster-scoped) name, deleting
// the bound PVC first via the PV's spec.claimRef. Both deletes tolerate a
// missing object (already gone == success), matching the legacy action.
func (m *Mutator) DeleteVolume(ctx context.Context, pvName string) (any, error) {
	if m.Client == nil {
		return nil, errors.New("mutate: client not configured")
	}
	if pvName == "" {
		return nil, errors.New("mutate: pv name required")
	}

	pvs := m.Client.CoreV1().PersistentVolumes()
	pv, err := pvs.Get(ctx, pvName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return map[string]any{"success": true, "message": fmt.Sprintf("pv %s already deleted", pvName)}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("volume_delete: get pv %s: %w", pvName, err)
	}

	deletedPVC := ""
	if ref := pv.Spec.ClaimRef; ref != nil && ref.Name != "" {
		err := m.Client.CoreV1().PersistentVolumeClaims(ref.Namespace).Delete(ctx, ref.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("volume_delete: delete bound pvc %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		deletedPVC = ref.Namespace + "/" + ref.Name
	}

	if err := pvs.Delete(ctx, pvName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("volume_delete: delete pv %s: %w", pvName, err)
	}

	msg := fmt.Sprintf("pv %s deleted", pvName)
	if deletedPVC != "" {
		msg = fmt.Sprintf("pv %s and bound pvc %s deleted", pvName, deletedPVC)
	}
	return map[string]any{"success": true, "message": msg}, nil
}
