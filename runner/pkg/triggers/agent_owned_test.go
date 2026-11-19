package triggers

import (
	"testing"
	"time"
)

// The agent's own scan pods and Jobs fail in ways the scan result already
// reports (an image it cannot pull, a scan that runs out of memory). The same
// failures on a workload must still raise findings.
func TestEngine_SkipsAgentOwnedObjects(t *testing.T) {
	pullBackoffPod := func(labels string) map[string]any {
		return asObj(t, `{
			"metadata":{"name":"trivy-image-scan-abcd1234-xyz","namespace":"agent",`+labels+`
				"ownerReferences":[{"kind":"Job","name":"trivy-image-scan-abcd1234","controller":true}]},
			"status":{"containerStatuses":[
				{"name":"scanner","image":"registry.example.com/app:1","state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image"}}}
			]}
		}`)
	}
	failedJob := func(labels string) map[string]any {
		return asObj(t, `{
			"metadata":{"name":"trivy-image-scan-abcd1234","namespace":"agent"`+labels+`},
			"status":{"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}
		}`)
	}
	runningJob := func(labels string) map[string]any {
		return asObj(t, `{"metadata":{"name":"trivy-image-scan-abcd1234","namespace":"agent"`+labels+`},"status":{"active":1}}`)
	}
	const ownedJob = `,"labels":{"app.kubernetes.io/managed-by":"nudgebee-agent"}`
	const owned = `"labels":{"app.kubernetes.io/managed-by":"nudgebee-agent","job-name":"trivy-image-scan-abcd1234"},`
	const helm = `"labels":{"app.kubernetes.io/managed-by":"Helm"},`

	cases := []struct {
		name      string
		ev        IncomingK8sEvent
		wantMatch string // "" = no finding
	}{
		{"agent scan pod in ImagePullBackOff", IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: pullBackoffPod(owned), OldObj: pullBackoffPod(owned)}, ""},
		{"workload pod in ImagePullBackOff", IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: pullBackoffPod(""), OldObj: pullBackoffPod("")}, "image_pull_backoff"},
		{"helm-managed pod still reported", IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: pullBackoffPod(helm), OldObj: pullBackoffPod(helm)}, "image_pull_backoff"},
		{"agent scan Job failed", IncomingK8sEvent{Operation: "update", Kind: "Job", Obj: failedJob(ownedJob), OldObj: runningJob(ownedJob)}, ""},
		{"same Job unlabelled is reported", IncomingK8sEvent{Operation: "update", Kind: "Job", Obj: failedJob(""), OldObj: runningJob("")}, "job_failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngine(Builtins(), time.Now().Add(-time.Hour))
			names := matchNames(eng.Match(tc.ev))
			if tc.wantMatch == "" {
				if len(names) != 0 {
					t.Errorf("matches = %v; want none for an agent-owned object", names)
				}
				return
			}
			if !contains(names, tc.wantMatch) {
				t.Errorf("matches = %v; want %q", names, tc.wantMatch)
			}
		})
	}
}

func TestEngine_WorkloadJobFailureStillReported(t *testing.T) {
	old := asObj(t, `{"metadata":{"name":"nightly-etl-1","namespace":"data"},"status":{}}`)
	failed := asObj(t, `{"metadata":{"name":"nightly-etl-1","namespace":"data"},
		"status":{"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}}`)
	eng := NewEngine(Builtins(), time.Now().Add(-time.Hour))
	if names := matchNames(eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Job", Obj: failed, OldObj: old})); !contains(names, "job_failure") {
		t.Errorf("matches = %v; want job_failure for a workload Job", names)
	}
}
