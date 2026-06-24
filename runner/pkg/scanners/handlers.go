package scanners

import "github.com/nudgebee/nudgebee-agent/pkg/dispatch"

// Handlers wires the generic Job primitives into the dispatch registry.
//
// The agent owns no scanner-specific behavior. The api-server's
// scan_orchestrator builds JobSpecs (image, args, security context) and drives
// them through schedule_k8s_job → wait_for_k8s_job → get_k8s_job_logs →
// delete_k8s_job. Adding a new scanner is a pure api-server change.
//
// All actions require HMAC sig (or RSA partial-keys for callers that pre-sign
// mutations). Not light-action — these create, read, and delete Jobs.
func Handlers(r *Runner) map[string]dispatch.Handler {
	return map[string]dispatch.Handler{
		"schedule_k8s_job": r.handleScheduleJob,
		"wait_for_k8s_job": r.handleWaitForJob,
		"get_k8s_job_logs": r.handleGetJobLogs,
		"delete_k8s_job":   r.handleDeleteJob,
	}
}
