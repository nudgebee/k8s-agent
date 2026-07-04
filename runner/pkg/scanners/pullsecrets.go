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
)

// copiedPullSecretLabel marks Secrets the agent copied for an image scan, so a
// stray one (e.g. left behind if the agent died between copy and ownerRef set)
// is identifiable for a sweep.
const copiedPullSecretLabel = "nudgebee.com/copied-pull-secret"

// resolvePullSecrets reads the effective image-pull credentials of pod `ref` and
// returns Secret objects (in r.Namespace) ready to be created — it does NOT
// create them. Creation is deferred to createOwnedSecrets so each copy is
// stamped with an ownerReference to the scan Job at creation time; the agent has
// `create` but not `update` on secrets, so the reference can't be attached
// afterwards. Best-effort: a source secret that can't be read is logged and
// skipped rather than failing the whole scan.
//
// The "effective" pull secrets of a pod are its pod-level imagePullSecrets plus
// its ServiceAccount's imagePullSecrets — both are consulted by the kubelet, so
// both are needed to reproduce the pull.
func (r *Runner) resolvePullSecrets(ctx context.Context, ref PodRef, jobName string) []*corev1.Secret {
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

	out := make([]*corev1.Secret, 0, len(names))
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
		out = append(out, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      copiedSecretName(jobName, ref.Namespace, name),
				Namespace: r.Namespace,
				Labels: map[string]string{
					managedByLabel:        managedByValue,
					copiedPullSecretLabel: "true",
				},
			},
			Type: src.Type,
			Data: src.Data,
		})
	}
	return out
}

// createOwnedSecrets creates the resolved pull-secret copies in the scanner
// namespace, each owned by the scan Job so Kubernetes garbage-collects them when
// the Job is deleted (by its TTL or the reaper). The ownerReference is stamped
// at creation time by design: the agent has `create` on secrets but not
// `update`/`delete`, so it cannot attach the reference (or clean the copy up)
// after the fact. BlockOwnerDeletion is deliberately left unset — it would
// require `update` on the Job's finalizers subresource, which the agent lacks,
// and it is unnecessary for garbage collection. Best-effort: a copy that fails
// to create is logged; the pod's pull for that registry fails while other
// registries/layers still scan. AlreadyExists (a retry with the same Job name)
// is treated as success.
func (r *Runner) createOwnedSecrets(ctx context.Context, secrets []*corev1.Secret, job *batchv1.Job) {
	owner := metav1.OwnerReference{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
	}
	for _, s := range secrets {
		s.OwnerReferences = append(s.OwnerReferences, owner)
		if _, err := r.Client.CoreV1().Secrets(r.Namespace).Create(ctx, s, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			slog.Warn("image scan: cannot create copied pull secret in scanner namespace",
				"secret", s.Name, "scanner_namespace", r.Namespace, "error", err)
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
