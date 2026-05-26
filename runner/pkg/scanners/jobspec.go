package scanners

import corev1 "k8s.io/api/core/v1"

// JobSpec is the wire shape api-server supplies to schedule_k8s_job.
//
// The agent has no knowledge of which scanner this Job runs — adding a new
// scanner is a pure api-server change. Anything semantic (image, args, RBAC)
// is server-supplied; the agent enforces hygiene only (namespace, TTL,
// BackoffLimit, concurrency cap, log size cap — see primitives.go).
//
// Field set is intentionally narrow: only what scanners needed at the time of
// cutover. Fields the agent ignores from the server are documented inline
// so a future caller doesn't waste time setting them.
type JobSpec struct {
	NamePrefix     string            `json:"name_prefix"`               // required; sanitized to DNS-1123, prefix of <prefix>-<random8>
	Image          string            `json:"image"`                     // required; agent does NOT validate
	Command        []string          `json:"command,omitempty"`         // optional ENTRYPOINT override
	Args           []string          `json:"args,omitempty"`            // container args
	Env            map[string]string `json:"env,omitempty"`             // simple key/value env
	ServiceAccount string            `json:"service_account,omitempty"` // empty → cfg.ScannerServiceAccount default

	Privileged  bool `json:"privileged,omitempty"`   // securityContext.privileged
	HostPID     bool `json:"host_pid,omitempty"`     // pod.spec.hostPID
	HostNetwork bool `json:"host_network,omitempty"` // pod.spec.hostNetwork

	Volumes      []corev1.Volume      `json:"volumes,omitempty"`       // for kube-bench /etc, /var/lib/etcd hostPath mounts
	VolumeMounts []corev1.VolumeMount `json:"volume_mounts,omitempty"` // matching mounts inside the container

	// TimeoutHintSeconds is the api-server orchestrator's expected upper bound
	// for this Job. Agent ignores it — the api-server polls and decides when to
	// give up. Carried through purely so audit logs can record server intent.
	TimeoutHintSeconds int `json:"timeout_hint_seconds,omitempty"`
}
