package scanners

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// copiedPullSecretLabel marks Secrets the agent copied for an image scan, so a
// stray one (e.g. left behind if the agent died between copy and ownerRef set)
// is identifiable for a sweep.
const copiedPullSecretLabel = "nudgebee.com/copied-pull-secret"

// resolveAndCopyPullSecrets makes the image-pull credentials of pod `ref`
// available in the scanner namespace so a scan Job can pull that workload's
// (private) image. It returns the names of the copied Secrets, all living in
// r.Namespace. Best-effort: a secret that can't be read/copied is logged and
// skipped rather than failing the whole scan.
//
// The "effective" pull secrets of a pod are its pod-level imagePullSecrets plus
// its ServiceAccount's imagePullSecrets — both are consulted by the kubelet, so
// both are needed to reproduce the pull.
func (r *Runner) resolveAndCopyPullSecrets(ctx context.Context, ref PodRef, jobName string) []*corev1.Secret {
	pod, err := r.Client.CoreV1().Pods(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		slog.Warn("image scan: cannot read target pod for pull secrets; scan may fail to pull a private image",
			"namespace", ref.Namespace, "pod", ref.Name, "error", err)
		return nil
	}

	// Gather the distinct secret names the kubelet would use for this pod.
	names := map[string]struct{}{}
	for _, s := range pod.Spec.ImagePullSecrets {
		if s.Name != "" {
			names[s.Name] = struct{}{}
		}
	}
	saName := pod.Spec.ServiceAccountName
	if saName == "" {
		saName = "default"
	}
	if sa, err := r.Client.CoreV1().ServiceAccounts(ref.Namespace).Get(ctx, saName, metav1.GetOptions{}); err == nil {
		for _, s := range sa.ImagePullSecrets {
			if s.Name != "" {
				names[s.Name] = struct{}{}
			}
		}
	} else {
		slog.Warn("image scan: cannot read target ServiceAccount for pull secrets",
			"namespace", ref.Namespace, "service_account", saName, "error", err)
	}

	copied := make([]*corev1.Secret, 0, len(names))
	for name := range names {
		src, err := r.Client.CoreV1().Secrets(ref.Namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			slog.Warn("image scan: cannot read pull secret", "namespace", ref.Namespace, "secret", name, "error", err)
			continue
		}
		// Only registry credentials — never copy arbitrary secrets even if a pod
		// happens to list one under imagePullSecrets.
		if src.Type != corev1.SecretTypeDockerConfigJson && src.Type != corev1.SecretTypeDockercfg {
			continue
		}
		dstName := copiedSecretName(jobName, ref.Namespace, name)
		dst := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dstName,
				Namespace: r.Namespace,
				Labels: map[string]string{
					managedByLabel:        managedByValue,
					copiedPullSecretLabel: "true",
				},
			},
			Type: src.Type,
			Data: src.Data,
		}
		created, err := r.Client.CoreV1().Secrets(r.Namespace).Create(ctx, dst, metav1.CreateOptions{})
		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Deterministic name → a prior attempt for this same Job. Reuse it.
				if existing, getErr := r.Client.CoreV1().Secrets(r.Namespace).Get(ctx, dstName, metav1.GetOptions{}); getErr == nil {
					copied = append(copied, existing)
				}
			} else {
				slog.Warn("image scan: cannot copy pull secret into scanner namespace",
					"secret", dstName, "scanner_namespace", r.Namespace, "error", err)
			}
			continue
		}
		copied = append(copied, created)
	}
	return copied
}

// ownReferencedSecrets points the copied Secrets at the Job so they are
// garbage-collected when the Job's TTL deletes it. The Job UID is only known
// after creation, hence this post-create step. Same namespace, so the
// ownerReference is valid. Operates on the objects returned by the copy step
// (their ResourceVersion is current), so no extra Get is needed. Best-effort: a
// failed update only risks a short-lived orphan that namespace cleanup removes.
func (r *Runner) ownReferencedSecrets(ctx context.Context, secrets []*corev1.Secret, job *batchv1.Job) {
	owner := metav1.OwnerReference{
		APIVersion:         "batch/v1",
		Kind:               "Job",
		Name:               job.Name,
		UID:                job.UID,
		BlockOwnerDeletion: ptr.To(true),
	}
	for _, s := range secrets {
		s.OwnerReferences = append(s.OwnerReferences, owner)
		if _, err := r.Client.CoreV1().Secrets(r.Namespace).Update(ctx, s, metav1.UpdateOptions{}); err != nil {
			slog.Warn("image scan: cannot set ownerReference on copied pull secret (will rely on sweep)",
				"secret", s.Name, "error", err)
		}
	}
}

// deleteCopiedSecrets removes copied Secrets, used when Job creation fails so we
// don't leak credentials. Best-effort.
func (r *Runner) deleteCopiedSecrets(ctx context.Context, secrets []*corev1.Secret) {
	for _, s := range secrets {
		if err := r.Client.CoreV1().Secrets(r.Namespace).Delete(ctx, s.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			slog.Warn("image scan: cannot delete copied pull secret after job-create failure",
				"secret", s.Name, "error", err)
		}
	}
}

// copiedSecretName is deterministic per (job, source secret) and DNS-1123 safe.
// The hash disambiguates two source secrets that share a name across namespaces
// and keeps the result bounded regardless of source name length.
func copiedSecretName(jobName, srcNamespace, srcName string) string {
	sum := sha256.Sum256([]byte(srcNamespace + "/" + srcName))
	return fmt.Sprintf("imgps-%s-%s", jobName, hex.EncodeToString(sum[:])[:8])
}
