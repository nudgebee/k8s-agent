// Package scanners exposes generic Kubernetes Job primitives the api-server
// uses to orchestrate scanners (Trivy, Popeye, KRR, kube-bench, cert-scanner,
// Nova helm-upgrade, k8s-version-upgrade) and any future Job-shaped action.
//
// The agent intentionally has NO knowledge of scanner semantics. The
// api-server constructs a JobSpec (image, args, security context, volumes)
// and passes it through schedule_k8s_job; the agent enforces hygiene gates
// (namespace clamp, TTL clamp, BackoffLimit clamp, concurrency cap, log size
// cap) but never policy. Adding a new scanner tomorrow is a pure api-server
// change — no agent code, no customer Helm upgrade.
//
// Action surface (registered in handlers.go):
//   - schedule_k8s_job  : create a Job from a server-supplied JobSpec
//   - wait_for_k8s_job  : poll Job status (single call; orchestrator loops)
//   - get_k8s_job_logs  : fetch (capped) stdout from the Job's pod
package scanners

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
)

// Hygiene constants — agent-enforced, server cannot override.
const (
	jobTTLSeconds        int32 = 600 // 10 min after-finish cleanup
	jobBackoffLimit      int32 = 0   // fail-fast; orchestrator decides retries
	defaultMaxConcurrent int   = 5
	// 64 MiB raw stdout cap — large enough to absorb trivy_cis (~17 MiB) and
	// kube_bench full reports without truncation. Agent gzips before sending,
	// so the wire payload is typically ~5-10x smaller than the raw cap.
	defaultLogCapBytes   int = 64 << 20 // 64 MiB
	jobNameRandomBytes   int = 4        // → 8 hex chars
	managedByLabel           = "app.kubernetes.io/managed-by"
	managedByValue           = "nudgebee-agent"
	orchestratorLabel        = "nudgebee.com/orchestrator"
	orchestratorValue        = "api-server"
	jobUUIDLabel             = "nudgebee.com/job-uuid"
	jobNameSelectorLabel     = "job-name"
)

// Runner schedules and watches Jobs. Stateless apart from the K8s client and
// per-Runner namespace + default ServiceAccount; the per-Job hygiene gates
// live in primitives.go and reach Runner via BuildJob.
type Runner struct {
	Client            kubernetes.Interface
	Namespace         string // hard-clamped — server cannot override
	DefaultSA         string // applied when JobSpec.ServiceAccount == ""
	MaxConcurrentJobs int    // 0 → defaultMaxConcurrent
	LogCapBytes       int    // 0 → defaultLogCapBytes

	// AutoCopyPullSecrets gates the JobSpec.ImagePullSecretsFrom behavior. When
	// false (default), the field is ignored and the agent never reads or copies
	// registry credentials — scans rely on whatever pull secrets the scanner
	// ServiceAccount already carries. Set from SCANNER_AUTO_COPY_PULL_SECRETS.
	AutoCopyPullSecrets bool
}

// NewRunner returns a Runner with the canonical defaults.
//
// namespace defaults to "nudgebee-agent" when empty; the api-server's choice
// of namespace is ignored — the agent enforces the namespace it was deployed
// with so a misbehaving caller can't pile Jobs into kube-system.
func NewRunner(cs kubernetes.Interface, namespace, serviceAccount string) *Runner {
	if namespace == "" {
		namespace = "nudgebee-agent"
	}
	return &Runner{
		Client:            cs,
		Namespace:         namespace,
		DefaultSA:         serviceAccount,
		MaxConcurrentJobs: defaultMaxConcurrent,
		LogCapBytes:       defaultLogCapBytes,
	}
}

// BuildJob materializes a *batchv1.Job from a server-supplied JobSpec.
//
// Hygiene the agent enforces (server cannot override):
//   - Job runs in r.Namespace.
//   - TTLSecondsAfterFinished = 600 (10 min cleanup).
//   - BackoffLimit = 0 (fail-fast).
//   - app.kubernetes.io/managed-by + nudgebee.com/orchestrator labels are stamped.
//   - A per-Job UUID label is added for audit.
//
// Caller is expected to validate JobSpec first; BuildJob will not reject a
// missing NamePrefix or Image — that's the primitive's job before BuildJob
// is called.
func (r *Runner) BuildJob(spec JobSpec, jobName, jobUUID string) *batchv1.Job {
	envs := make([]corev1.EnvVar, 0, len(spec.Env))
	for k, v := range spec.Env {
		envs = append(envs, corev1.EnvVar{Name: k, Value: v})
	}
	container := corev1.Container{
		Name:         "scanner",
		Image:        spec.Image,
		Command:      spec.Command,
		Args:         spec.Args,
		Env:          envs,
		VolumeMounts: spec.VolumeMounts,
	}
	if spec.ImagePullPolicy != "" {
		container.ImagePullPolicy = corev1.PullPolicy(spec.ImagePullPolicy)
	}
	// SecurityContext is built only when the server asks for something — privileged
	// (kube-bench) and/or a specific runAsUser (image_scanner runs as root so
	// `trivy fs` can read the whole rootfs).
	if spec.Privileged || spec.RunAsUser != nil {
		container.SecurityContext = &corev1.SecurityContext{}
		if spec.Privileged {
			container.SecurityContext.Privileged = ptr.To(true)
		}
		if spec.RunAsUser != nil {
			container.SecurityContext.RunAsUser = spec.RunAsUser
		}
	}

	saName := spec.ServiceAccount
	if saName == "" {
		saName = r.DefaultSA
	}

	podSpec := corev1.PodSpec{
		RestartPolicy:      corev1.RestartPolicyNever,
		InitContainers:     spec.InitContainers,
		Containers:         []corev1.Container{container},
		HostPID:            spec.HostPID,
		HostNetwork:        spec.HostNetwork,
		NodeName:           spec.NodeName,
		ServiceAccountName: saName,
		Volumes:            spec.Volumes,
	}

	ttl := jobTTLSeconds
	backoff := jobBackoffLimit
	labels := map[string]string{
		managedByLabel:    managedByValue,
		orchestratorLabel: orchestratorValue,
		jobUUIDLabel:      jobUUID,
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: r.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{jobNameSelectorLabel: jobName},
				},
				Spec: podSpec,
			},
		},
	}
}

// makeJobName returns "<prefix>-<8-hex>". Prefix is sanitized so an attacker
// or a buggy api-server can't smuggle a non-DNS-1123 name into the apiserver.
func makeJobName(prefix string) (string, error) {
	clean := sanitizeDNSPrefix(prefix)
	if clean == "" {
		return "", errors.New("name_prefix is required and must contain at least one DNS-1123 character")
	}
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	suffix := hex.EncodeToString(buf[:])
	// Job name max is 63 chars (DNS-1123). Reserve 9 for "-<8hex>".
	const max = 63 - 1 - 8
	if len(clean) > max {
		clean = clean[:max]
	}
	return clean + "-" + suffix, nil
}

func sanitizeDNSPrefix(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// listManagedJobs counts Jobs the agent itself created that haven't reached
// a terminal state. Used by primitives.go for the concurrency cap.
func (r *Runner) listManagedJobs(ctx context.Context) (int, error) {
	jobs, err := r.Client.BatchV1().Jobs(r.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + managedByValue + "," + orchestratorLabel + "=" + orchestratorValue,
	})
	if err != nil {
		return 0, err
	}
	active := 0
	for i := range jobs.Items {
		if !jobIsTerminal(&jobs.Items[i]) {
			active++
		}
	}
	return active, nil
}

func jobIsTerminal(j *batchv1.Job) bool {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed {
			return true
		}
	}
	return false
}

// jobStatusSnapshot returns ("Running" | "Complete" | "Failed", reason).
// Empty string for status when the Job hasn't reached a condition yet.
func jobStatusSnapshot(j *batchv1.Job) (string, string) {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete {
			return "Complete", c.Message
		}
		if c.Type == batchv1.JobFailed {
			return "Failed", c.Message
		}
	}
	return "Running", ""
}

// imagePullFailure reports whether the Job's pod is wedged on an image pull it
// will never recover from — the container is Waiting with reason ImagePullBackOff
// or ErrImagePull. This is terminal in practice (a missing tag or a private
// registry the scanner can't authenticate to won't fix itself) but the kubelet
// never starts the container, so the Job earns no Failed condition and would
// otherwise poll as "Running" until the orchestrator's overall deadline — never
// getting deleted, never reaped (TTL and the reaper only touch finished Jobs).
// Surfacing it as Failed lets the orchestrator stop early, delete the Job, and
// record a precise reason. Best-effort: a list error yields ("", false) so the
// caller falls back to the condition-based snapshot.
//
// Both init and main container statuses are checked — the scanner container is
// the target image being pulled, but a broken TrivyImage would wedge the init
// container the same way.
func (r *Runner) imagePullFailure(ctx context.Context, jobName string) (string, bool) {
	pods, err := r.Client.CoreV1().Pods(r.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: jobNameSelectorLabel + "=" + jobName,
	})
	if err != nil {
		return "", false
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
		for _, cs := range statuses {
			w := cs.State.Waiting
			if w == nil {
				continue
			}
			if w.Reason == "ImagePullBackOff" || w.Reason == "ErrImagePull" {
				return fmt.Sprintf("image pull failed for container %q: %s", cs.Name, w.Message), true
			}
		}
	}
	return "", false
}

// fetchPodLogs reads at most LogCapBytes from the (single) pod the Job
// spawned. Returns (stdout, truncated, droppedBytes, error).
func (r *Runner) fetchPodLogs(ctx context.Context, jobName string) (string, bool, int, error) {
	pods, err := r.Client.CoreV1().Pods(r.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: jobNameSelectorLabel + "=" + jobName,
	})
	if err != nil {
		return "", false, 0, err
	}
	if len(pods.Items) == 0 {
		return "", false, 0, errors.New("no pods found for job")
	}
	cap := r.LogCapBytes
	if cap <= 0 {
		cap = defaultLogCapBytes
	}
	req := r.Client.CoreV1().Pods(r.Namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", false, 0, err
	}
	defer func() { _ = stream.Close() }()

	var out strings.Builder
	out.Grow(min(cap, 32<<10))
	buf := make([]byte, 32<<10)
	written := 0
	dropped := 0
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			room := cap - written
			switch {
			case room <= 0:
				dropped += n
			case n <= room:
				out.Write(buf[:n])
				written += n
			default:
				out.Write(buf[:room])
				dropped += n - room
				written = cap
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			break
		}
	}
	return out.String(), dropped > 0, dropped, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
