package scanners

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// schedule_k8s_job
// ----------------
// Inbound params:
//
//	{ "spec": <JobSpec JSON> }
//
// Outbound:
//
//	success: { "job_name": "<name>", "job_uuid": "<uuid>" }
//	conflict: { "error": "concurrent_job_limit", "limit": 5 }  (status_code 200; orchestrator backs off)
//	bad spec: handler error → status_code 500
func (r *Runner) handleScheduleJob(ctx context.Context, params map[string]any) (any, error) {
	spec, err := decodeJobSpec(params)
	if err != nil {
		return nil, err
	}
	if spec.NamePrefix == "" {
		return nil, errors.New("schedule_k8s_job: name_prefix is required")
	}
	if spec.Image == "" {
		return nil, errors.New("schedule_k8s_job: image is required")
	}

	active, err := r.listManagedJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("schedule_k8s_job: list active jobs: %w", err)
	}
	limit := r.MaxConcurrentJobs
	if limit <= 0 {
		limit = defaultMaxConcurrent
	}
	if active >= limit {
		// Soft fail — orchestrator should retry. Return a structured payload
		// rather than an error so the relay surfaces it as a normal response.
		return map[string]any{
			"success": false,
			"error":   "concurrent_job_limit",
			"limit":   limit,
			"active":  active,
		}, nil
	}

	jobName, err := makeJobName(spec.NamePrefix)
	if err != nil {
		return nil, fmt.Errorf("schedule_k8s_job: %w", err)
	}
	jobUUID := uuid.NewString()
	job := r.BuildJob(spec, jobName, jobUUID)

	// Opt-in: make the target workload's image-pull credentials available so the
	// Job can pull a private image. Gated by AutoCopyPullSecrets — when off, the
	// field is ignored and no credentials are read or copied.
	var copiedPullSecrets []*corev1.Secret
	if r.AutoCopyPullSecrets && spec.ImagePullSecretsFrom != nil {
		copiedPullSecrets = r.resolveAndCopyPullSecrets(ctx, *spec.ImagePullSecretsFrom, jobName)
		for _, s := range copiedPullSecrets {
			job.Spec.Template.Spec.ImagePullSecrets = append(
				job.Spec.Template.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: s.Name})
		}
	}

	created, err := r.Client.BatchV1().Jobs(r.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		// Don't leak the copied credentials if the Job never came up to own them.
		r.deleteCopiedSecrets(ctx, copiedPullSecrets)
		return nil, fmt.Errorf("schedule_k8s_job: create: %w", err)
	}
	// GC the copies with the Job (ownerReference; Job TTL cleans both up).
	r.ownReferencedSecrets(ctx, copiedPullSecrets, created)

	slog.Info("schedule_k8s_job: created",
		"job_name", created.Name,
		"job_uuid", jobUUID,
		"namespace", r.Namespace,
		"image", spec.Image,
		"args", spec.Args,
		"privileged", spec.Privileged,
		"host_pid", spec.HostPID,
		"host_network", spec.HostNetwork,
		"service_account", created.Spec.Template.Spec.ServiceAccountName,
	)

	return map[string]any{
		"success":  true,
		"job_name": created.Name,
		"job_uuid": jobUUID,
	}, nil
}

// wait_for_k8s_job
// ----------------
// Inbound params:  { "job_name": "<name>" }
//
// Outbound:        { "status": "Running"|"Complete"|"Failed", "failure_reason": "..." }
//
// Single non-blocking call — the api-server orchestrator polls.
func (r *Runner) handleWaitForJob(ctx context.Context, params map[string]any) (any, error) {
	jobName, _ := params["job_name"].(string)
	if jobName == "" {
		return nil, errors.New("wait_for_k8s_job: job_name is required")
	}
	job, err := r.Client.BatchV1().Jobs(r.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return map[string]any{
				"status":         "NotFound",
				"failure_reason": "job no longer exists (TTL expired or never created)",
			}, nil
		}
		return nil, fmt.Errorf("wait_for_k8s_job: get: %w", err)
	}
	status, reason := jobStatusSnapshot(job)
	out := map[string]any{
		"status":         status,
		"failure_reason": reason,
		"active":         job.Status.Active,
		"succeeded":      job.Status.Succeeded,
		"failed":         job.Status.Failed,
	}
	if job.Status.StartTime != nil {
		out["start_time"] = job.Status.StartTime.UTC().Format("2006-01-02T15:04:05Z")
	}
	if job.Status.CompletionTime != nil {
		out["completion_time"] = job.Status.CompletionTime.UTC().Format("2006-01-02T15:04:05Z")
	}
	return out, nil
}

// get_k8s_job_logs
// ----------------
// Inbound params:  { "job_name": "<name>" }
//
// Outbound:
//
//	{
//	  "stdout_b64_gzip":      "<base64(gzip(stdout))>",
//	  "stdout_size":          <raw byte count that went into the gzip>,
//	  "truncated":            bool,
//	  "stdout_bytes_dropped": int,
//	}
//
// Compression matters because trivy_cis produces ~17 MiB of structured JSON
// and kube_bench can be similar. Gzipping before the relay→api-server hop
// drops the wire payload ~8-10x for that shape, fitting cleanly inside the
// agent dispatch's 180s ceiling. The api-server's relay wrapper inflates
// transparently so callers see a plain string.
func (r *Runner) handleGetJobLogs(ctx context.Context, params map[string]any) (any, error) {
	jobName, _ := params["job_name"].(string)
	if jobName == "" {
		return nil, errors.New("get_k8s_job_logs: job_name is required")
	}
	stdout, truncated, dropped, err := r.fetchPodLogs(ctx, jobName)
	if err != nil {
		return nil, fmt.Errorf("get_k8s_job_logs: %w", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(stdout)); err != nil {
		return nil, fmt.Errorf("get_k8s_job_logs: gzip: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("get_k8s_job_logs: gzip close: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())

	return map[string]any{
		"stdout_b64_gzip":      encoded,
		"stdout_size":          len(stdout),
		"truncated":            truncated,
		"stdout_bytes_dropped": dropped,
	}, nil
}

// delete_k8s_job
// --------------
// Inbound params:  { "job_name": "<name>" }
//
// Outbound:        { "deleted": true }   (also returned when the Job is already gone)
//
// The orchestrator calls this right after it has fetched a Job's logs so the
// Job (and its pods) are cleaned up promptly on the happy path, instead of
// waiting out the TTL/reaper grace window. The background reaper is the backstop
// for crash/timeout paths where this call never happens.
//
// Only the runner's own namespace is touched, and only by Job name handed back
// from schedule_k8s_job — the agent never deletes arbitrary cluster objects.
func (r *Runner) handleDeleteJob(ctx context.Context, params map[string]any) (any, error) {
	jobName, _ := params["job_name"].(string)
	if jobName == "" {
		return nil, errors.New("delete_k8s_job: job_name is required")
	}
	if err := r.deleteJobCascade(ctx, jobName); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("delete_k8s_job: %w", err)
	}
	slog.Info("delete_k8s_job: deleted", "job_name", jobName, "namespace", r.Namespace)
	return map[string]any{"deleted": true}, nil
}

// decodeJobSpec re-marshals the params["spec"] map through encoding/json so
// JobSpec's struct tags drive the decode (handles nested corev1.Volume /
// VolumeMount cleanly without bespoke parsing).
func decodeJobSpec(params map[string]any) (JobSpec, error) {
	raw, ok := params["spec"]
	if !ok || raw == nil {
		return JobSpec{}, errors.New("schedule_k8s_job: spec is required")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return JobSpec{}, fmt.Errorf("re-encode spec: %w", err)
	}
	var spec JobSpec
	if err := json.Unmarshal(encoded, &spec); err != nil {
		return JobSpec{}, fmt.Errorf("decode spec: %w", err)
	}
	return spec, nil
}
