package scanners

import (
	"context"
	"log/slog"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Job cleanup is normally handled by Kubernetes' TTLAfterFinished controller
// (we stamp every Job with TTLSecondsAfterFinished=jobTTLSeconds). That
// controller is GA and on by default since k8s 1.21, but a customer cluster
// can have it disabled (kube-controller-manager --controllers=-ttl-after-finished,
// hardened/managed distros, or pre-1.21). When it's off, finished scanner Jobs
// and their Completed pods accumulate forever.
//
// The reaper makes cleanup independent of that controller: the agent itself
// periodically deletes its own finished Jobs once they're past the same grace
// window the TTL field encodes. It only ever touches Jobs carrying BOTH the
// managed-by and orchestrator labels, scoped to the runner's namespace, so it
// can never delete a customer's Jobs. Where the TTL controller IS present this
// is a harmless no-op (the controller usually wins the race first).
const (
	// reaperInterval is how often the sweep runs.
	reaperInterval = 60 * time.Second
	// reaperSweepTimeout bounds a single sweep's API calls, safely below
	// reaperInterval so a hung API server can't wedge the loop.
	reaperSweepTimeout = 45 * time.Second
	// reaperListLimit caps how many Jobs a single sweep lists/deletes, so a large
	// accumulated backlog (TTL controller off for a long time) is drained in
	// bounded batches across successive sweeps instead of one giant List that
	// could spike agent memory or overload the API server.
	reaperListLimit = 100
	// reapGraceSeconds is how long after a Job finishes before it's eligible for
	// deletion. Matched to jobTTLSeconds so behavior mirrors the TTL field: the
	// orchestrator's completion→log-fetch gap is seconds, so a 10-min grace
	// preserves the existing window for retries/manual debugging of logs.
	reapGraceSeconds = jobTTLSeconds
)

// StartReaper launches the background cleanup loop and returns immediately. The
// loop runs until ctx is cancelled (agent shutdown). Safe to call once per
// Runner from the agent's main wiring.
func (r *Runner) StartReaper(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "scanner_job_reaper", "namespace", r.Namespace)
	go func() {
		ticker := time.NewTicker(reaperInterval)
		defer ticker.Stop()
		logger.Info("scanner job reaper started",
			"interval", reaperInterval.String(), "grace_seconds", reapGraceSeconds)
		for {
			select {
			case <-ctx.Done():
				logger.Info("scanner job reaper stopped")
				return
			case <-ticker.C:
				// Bound each sweep below the tick interval so an unresponsive API
				// server can't wedge the loop indefinitely.
				sweepCtx, cancel := context.WithTimeout(ctx, reaperSweepTimeout)
				if n := r.reapFinishedJobs(sweepCtx, logger, time.Now()); n > 0 {
					logger.Info("reaped finished scanner jobs", "deleted", n)
				}
				cancel()
			}
		}
	}()
}

// reapFinishedJobs deletes managed Jobs that finished more than reapGraceSeconds
// before now. Returns the number deleted. Errors are logged and skipped — a
// single bad Job must not stall the sweep. `now` is a parameter so tests can
// drive it deterministically.
func (r *Runner) reapFinishedJobs(ctx context.Context, logger *slog.Logger, now time.Time) int {
	jobs, err := r.Client.BatchV1().Jobs(r.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: managedByLabel + "=" + managedByValue + "," + orchestratorLabel + "=" + orchestratorValue,
		Limit:         reaperListLimit,
	})
	if err != nil {
		// Suppress expected cancellation noise during agent shutdown.
		if ctx.Err() == nil {
			logger.Warn("reaper: list managed jobs failed", "error", err)
		}
		return 0
	}

	grace := time.Duration(reapGraceSeconds) * time.Second
	deleted := 0
	for i := range jobs.Items {
		job := &jobs.Items[i]
		finishedAt, ok := jobFinishedAt(job)
		if !ok {
			continue // still running / no terminal condition yet
		}
		if now.Sub(finishedAt) < grace {
			continue // within the grace window — leave it for log fetch / debugging
		}
		if err := r.deleteJobCascade(ctx, job.Name); err != nil && !apierrors.IsNotFound(err) {
			if ctx.Err() == nil {
				logger.Warn("reaper: delete job failed", "job_name", job.Name, "error", err)
			}
			continue
		}
		deleted++
		logger.Info("reaper: deleted finished job",
			"job_name", job.Name, "finished_at", finishedAt.UTC().Format(time.RFC3339))
	}
	return deleted
}

// deleteJobCascade deletes a Job with background propagation so the Job's pods
// are garbage-collected along with it (the default foreground/orphan behavior
// would otherwise leave the Completed pods behind).
func (r *Runner) deleteJobCascade(ctx context.Context, jobName string) error {
	policy := metav1.DeletePropagationBackground
	return r.Client.BatchV1().Jobs(r.Namespace).Delete(ctx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &policy,
	})
}

// jobFinishedAt returns when a Job reached a terminal (Complete/Failed)
// condition. Complete Jobs carry Status.CompletionTime; Failed Jobs don't, so
// we fall back to the terminal condition's LastTransitionTime. Returns false
// when the Job has not finished.
func jobFinishedAt(j *batchv1.Job) (time.Time, bool) {
	if !jobIsTerminal(j) {
		return time.Time{}, false
	}
	if j.Status.CompletionTime != nil {
		return j.Status.CompletionTime.Time, true
	}
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		if c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed {
			return c.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}
