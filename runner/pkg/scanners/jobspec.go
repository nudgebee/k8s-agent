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

	// NodeName pins the Job's pod to a specific node (pod.spec.nodeName), bypassing
	// the scheduler. The image_scanner uses this to land the scan on the node where
	// the target image is already pulled, so a `trivy fs` rootfs scan can reuse the
	// node-local image (imagePullPolicy=IfNotPresent) instead of pulling it from the
	// registry — which is why image scans need no registry credentials.
	NodeName string `json:"node_name,omitempty"`

	// ImagePullPolicy overrides the main container's pull policy. Empty → the
	// kubelet default (Always for :latest, IfNotPresent otherwise). image_scanner
	// sets IfNotPresent so the node-local copy is reused.
	ImagePullPolicy string `json:"image_pull_policy,omitempty"`

	// RunAsUser sets the main container's securityContext.runAsUser. Pointer so an
	// explicit 0 (root — needed for `trivy fs` to read every file in the scanned
	// rootfs) is distinguishable from "unset". nil → image default.
	RunAsUser *int64 `json:"run_as_user,omitempty"`

	// InitContainers run before the main container. image_scanner uses one to copy
	// the trivy binary from the scanner image into a shared emptyDir, so the main
	// container (the target image itself) can run `trivy fs /` against its own
	// rootfs without the target image needing trivy installed.
	InitContainers []corev1.Container `json:"init_containers,omitempty"`

	Volumes      []corev1.Volume      `json:"volumes,omitempty"`       // for kube-bench /etc, /var/lib/etcd hostPath mounts; image_scanner's shared emptyDir
	VolumeMounts []corev1.VolumeMount `json:"volume_mounts,omitempty"` // matching mounts inside the main container

	// TimeoutHintSeconds is the api-server orchestrator's expected upper bound
	// for this Job. Agent ignores it — the api-server polls and decides when to
	// give up. Carried through purely so audit logs can record server intent.
	TimeoutHintSeconds int `json:"timeout_hint_seconds,omitempty"`
}
