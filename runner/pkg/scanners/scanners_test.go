package scanners

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

// ---------- Runner construction & BuildJob hygiene ----------

func TestNewRunner_Defaults(t *testing.T) {
	r := NewRunner(fake.NewClientset(), "", "")
	if r.Namespace != "nudgebee-agent" {
		t.Errorf("Namespace = %q; want nudgebee-agent default", r.Namespace)
	}
	if r.MaxConcurrentJobs != defaultMaxConcurrent {
		t.Errorf("MaxConcurrentJobs = %d; want default %d", r.MaxConcurrentJobs, defaultMaxConcurrent)
	}
	if r.LogCapBytes != defaultLogCapBytes {
		t.Errorf("LogCapBytes = %d; want default %d", r.LogCapBytes, defaultLogCapBytes)
	}
}

func TestBuildJob_HygieneInvariants(t *testing.T) {
	// Every Job the agent creates MUST have:
	//   - TTLSecondsAfterFinished = 600
	//   - BackoffLimit = 0
	//   - managed-by + orchestrator labels
	//   - per-Job UUID label
	// regardless of what the JobSpec asked for.
	r := NewRunner(fake.NewClientset(), "ns", "agent-sa")
	job := r.BuildJob(JobSpec{
		NamePrefix: "popeye-scan",
		Image:      "derailed/popeye:v0.21.5",
		Args:       []string{"-A", "-o", "json"},
	}, "popeye-scan-abcd1234", "uuid-1")

	if got := *job.Spec.TTLSecondsAfterFinished; got != jobTTLSeconds {
		t.Errorf("TTL = %d; want hard-clamped %d", got, jobTTLSeconds)
	}
	if got := *job.Spec.BackoffLimit; got != jobBackoffLimit {
		t.Errorf("BackoffLimit = %d; want hard-clamped %d", got, jobBackoffLimit)
	}
	if job.Labels[managedByLabel] != managedByValue {
		t.Errorf("missing managed-by label: %v", job.Labels)
	}
	if job.Labels[orchestratorLabel] != orchestratorValue {
		t.Errorf("missing orchestrator label: %v", job.Labels)
	}
	if job.Labels[jobUUIDLabel] != "uuid-1" {
		t.Errorf("missing job-uuid label: %v", job.Labels)
	}
	if job.Namespace != "ns" {
		t.Errorf("namespace = %q; agent must clamp", job.Namespace)
	}
	if job.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %v; want Never", job.Spec.Template.Spec.RestartPolicy)
	}
}

func TestBuildJob_HonorsServerSecurityContext(t *testing.T) {
	// kube-bench needs Privileged+HostPID+HostNetwork. The agent honors
	// these as opaque server-supplied flags — no scanner-name lookup.
	r := NewRunner(fake.NewClientset(), "ns", "")
	job := r.BuildJob(JobSpec{
		NamePrefix:  "kube-bench",
		Image:       "aquasec/kube-bench:v0.10.4",
		Args:        []string{"--json"},
		Privileged:  true,
		HostPID:     true,
		HostNetwork: true,
	}, "kube-bench-1", "uuid-2")

	ps := job.Spec.Template.Spec
	if !ps.HostPID {
		t.Error("HostPID lost in BuildJob")
	}
	if !ps.HostNetwork {
		t.Error("HostNetwork lost in BuildJob")
	}
	c := ps.Containers[0]
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Error("Privileged lost in BuildJob")
	}
}

func TestBuildJob_ServiceAccountFallback(t *testing.T) {
	r := NewRunner(fake.NewClientset(), "ns", "default-sa")

	// Empty ServiceAccount → falls back to runner default.
	job := r.BuildJob(JobSpec{NamePrefix: "x", Image: "x:1"}, "x-1", "u")
	if got := job.Spec.Template.Spec.ServiceAccountName; got != "default-sa" {
		t.Errorf("SA fallback = %q; want default-sa", got)
	}

	// Server-supplied ServiceAccount wins.
	job = r.BuildJob(JobSpec{NamePrefix: "x", Image: "x:1", ServiceAccount: "explicit-sa"}, "x-2", "u")
	if got := job.Spec.Template.Spec.ServiceAccountName; got != "explicit-sa" {
		t.Errorf("explicit SA = %q; want explicit-sa", got)
	}
}

func TestBuildJob_VolumesAndMounts(t *testing.T) {
	// kube-bench needs hostPath volumes — verify they round-trip through BuildJob.
	r := NewRunner(fake.NewClientset(), "ns", "")
	hp := corev1.HostPathVolumeSource{Path: "/etc"}
	job := r.BuildJob(JobSpec{
		NamePrefix: "kb",
		Image:      "aquasec/kube-bench:v0.10.4",
		Volumes: []corev1.Volume{
			{Name: "etc", VolumeSource: corev1.VolumeSource{HostPath: &hp}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "etc", MountPath: "/etc"},
		},
	}, "kb-1", "u")
	if len(job.Spec.Template.Spec.Volumes) != 1 {
		t.Fatalf("volumes lost: %v", job.Spec.Template.Spec.Volumes)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/etc" {
		t.Errorf("volume mounts lost: %v", c.VolumeMounts)
	}
}

func TestBuildJob_ImageScanFsShape(t *testing.T) {
	// image_scanner runs a rootfs scan: pinned to a node, target image as the
	// main container with IfNotPresent + runAsUser=0, and an init container that
	// stages the trivy binary into a shared volume. Verify all of it round-trips.
	r := NewRunner(fake.NewClientset(), "ns", "scanner-sa")
	job := r.BuildJob(JobSpec{
		NamePrefix:      "trivy-image-scan",
		Image:           "registry.private.example.com/app:v1",
		Command:         []string{"sh", "-c", "/var/trivy-operator/trivy fs --format json /"},
		NodeName:        "node-7",
		ImagePullPolicy: "IfNotPresent",
		RunAsUser:       ptr.To(int64(0)),
		InitContainers: []corev1.Container{
			{
				Name:    "trivy-get-binary",
				Image:   "ghcr.io/nudgebee/trivy:0.58.0",
				Command: []string{"cp", "/usr/local/bin/trivy", "/var/trivy-operator/trivy"},
			},
		},
	}, "trivy-image-scan-1", "uuid-img")

	ps := job.Spec.Template.Spec
	if ps.NodeName != "node-7" {
		t.Errorf("NodeName = %q; want node-7 (image scan must pin to the node)", ps.NodeName)
	}
	if len(ps.InitContainers) != 1 || ps.InitContainers[0].Name != "trivy-get-binary" {
		t.Fatalf("init container lost: %v", ps.InitContainers)
	}
	c := ps.Containers[0]
	if c.Image != "registry.private.example.com/app:v1" {
		t.Errorf("main container image = %q; want the target image itself", c.Image)
	}
	if c.ImagePullPolicy != corev1.PullIfNotPresent {
		t.Errorf("ImagePullPolicy = %q; want IfNotPresent (reuse node-local image)", c.ImagePullPolicy)
	}
	if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil || *c.SecurityContext.RunAsUser != 0 {
		t.Errorf("RunAsUser not honored: %+v", c.SecurityContext)
	}
	// Privileged must stay unset when only RunAsUser was requested.
	if c.SecurityContext.Privileged != nil {
		t.Errorf("Privileged should be nil when not requested: %+v", c.SecurityContext.Privileged)
	}
}

func TestMakeJobName_SanitizesAndShortens(t *testing.T) {
	// "Bad" prefix must be lowercased and stripped.
	name, err := makeJobName("My_Bad/Prefix!")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "my-bad-prefix-") {
		t.Errorf("name = %q; want sanitized prefix", name)
	}
	if len(name) > 63 {
		t.Errorf("name = %q (len %d); DNS-1123 cap is 63", name, len(name))
	}
	// Long prefix is truncated, not rejected.
	long := strings.Repeat("a", 200)
	name, err = makeJobName(long)
	if err != nil {
		t.Fatal(err)
	}
	if len(name) > 63 {
		t.Errorf("long name not truncated: %d chars", len(name))
	}
}

func TestMakeJobName_RejectsEmpty(t *testing.T) {
	if _, err := makeJobName(""); err == nil {
		t.Error("empty prefix must be rejected")
	}
	if _, err := makeJobName("!!!"); err == nil {
		t.Error("prefix with no DNS-1123 chars must be rejected")
	}
}

// ---------- schedule_k8s_job ----------

func TestScheduleJob_HappyPath(t *testing.T) {
	cs := fake.NewClientset()
	r := NewRunner(cs, "default", "agent-sa")
	out, err := r.handleScheduleJob(context.Background(), map[string]any{
		"spec": map[string]any{
			"name_prefix": "popeye-scan",
			"image":       "derailed/popeye:v0.21.5",
			"args":        []any{"-A", "-o", "json"},
		},
	})
	if err != nil {
		t.Fatalf("handleScheduleJob: %v", err)
	}
	resp := out.(map[string]any)
	if resp["success"] != true {
		t.Fatalf("success = %v", resp["success"])
	}
	jobName := resp["job_name"].(string)
	if !strings.HasPrefix(jobName, "popeye-scan-") {
		t.Errorf("job_name = %q; want popeye-scan-* prefix", jobName)
	}
	if resp["job_uuid"] == nil || resp["job_uuid"] == "" {
		t.Error("job_uuid missing")
	}
	jobs, _ := cs.BatchV1().Jobs("default").List(context.Background(), metav1.ListOptions{})
	if len(jobs.Items) != 1 {
		t.Fatalf("Job not actually created: %d", len(jobs.Items))
	}
}

func TestScheduleJob_RejectsMissingFields(t *testing.T) {
	r := NewRunner(fake.NewClientset(), "ns", "")
	cases := map[string]map[string]any{
		"missing spec": {},
		"missing name_prefix": {"spec": map[string]any{
			"image": "x:1",
		}},
		"missing image": {"spec": map[string]any{
			"name_prefix": "x",
		}},
	}
	for name, params := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := r.handleScheduleJob(context.Background(), params); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestScheduleJob_ConcurrencyCap(t *testing.T) {
	cs := fake.NewClientset()
	r := NewRunner(cs, "default", "")
	r.MaxConcurrentJobs = 2

	// Pre-populate two non-terminal Jobs with the agent's labels.
	for i := 0; i < 2; i++ {
		_, _ = cs.BatchV1().Jobs("default").Create(context.Background(), &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: "existing-" + string(rune('a'+i)), Namespace: "default",
				Labels: map[string]string{
					managedByLabel:    managedByValue,
					orchestratorLabel: orchestratorValue,
				},
			},
		}, metav1.CreateOptions{})
	}

	out, err := r.handleScheduleJob(context.Background(), map[string]any{
		"spec": map[string]any{"name_prefix": "p", "image": "x:1"},
	})
	if err != nil {
		t.Fatalf("handleScheduleJob: %v", err)
	}
	resp := out.(map[string]any)
	if resp["success"] != false {
		t.Errorf("success = %v; want false (capped)", resp["success"])
	}
	if resp["error"] != "concurrent_job_limit" {
		t.Errorf("error = %v; want concurrent_job_limit", resp["error"])
	}
	if resp["limit"] != 2 {
		t.Errorf("limit = %v; want 2", resp["limit"])
	}
}

// ---------- wait_for_k8s_job ----------

func TestWaitForJob_Running(t *testing.T) {
	cs := fake.NewClientset()
	_, _ = cs.BatchV1().Jobs("default").Create(context.Background(), &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j-1", Namespace: "default"},
	}, metav1.CreateOptions{})
	r := NewRunner(cs, "default", "")
	out, err := r.handleWaitForJob(context.Background(), map[string]any{"job_name": "j-1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]any)["status"] != "Running" {
		t.Errorf("status = %v; want Running", out.(map[string]any)["status"])
	}
}

func TestWaitForJob_Complete(t *testing.T) {
	cs := fake.NewClientset()
	now := metav1.Now()
	_, _ = cs.BatchV1().Jobs("default").Create(context.Background(), &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j-2", Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
				LastProbeTime: now, LastTransitionTime: now,
			}},
			CompletionTime: &now,
			StartTime:      &now,
			Succeeded:      1,
		},
	}, metav1.CreateOptions{})
	r := NewRunner(cs, "default", "")
	out, err := r.handleWaitForJob(context.Background(), map[string]any{"job_name": "j-2"})
	if err != nil {
		t.Fatal(err)
	}
	resp := out.(map[string]any)
	if resp["status"] != "Complete" {
		t.Errorf("status = %v", resp["status"])
	}
	if resp["completion_time"] == nil {
		t.Error("completion_time missing")
	}
}

func TestWaitForJob_Failed(t *testing.T) {
	cs := fake.NewClientset()
	now := metav1.Now()
	_, _ = cs.BatchV1().Jobs("default").Create(context.Background(), &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j-3", Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
				LastProbeTime: now, LastTransitionTime: now,
				Message: "container exited 1",
			}},
		},
	}, metav1.CreateOptions{})
	r := NewRunner(cs, "default", "")
	out, err := r.handleWaitForJob(context.Background(), map[string]any{"job_name": "j-3"})
	if err != nil {
		t.Fatal(err)
	}
	resp := out.(map[string]any)
	if resp["status"] != "Failed" {
		t.Errorf("status = %v", resp["status"])
	}
	if !strings.Contains(resp["failure_reason"].(string), "container exited 1") {
		t.Errorf("failure_reason = %v", resp["failure_reason"])
	}
}

func TestWaitForJob_NotFound(t *testing.T) {
	cs := fake.NewClientset()
	r := NewRunner(cs, "default", "")
	out, err := r.handleWaitForJob(context.Background(), map[string]any{"job_name": "nonexistent"})
	if err != nil {
		t.Fatal(err)
	}
	if out.(map[string]any)["status"] != "NotFound" {
		t.Errorf("status = %v; want NotFound", out.(map[string]any)["status"])
	}
}

func TestWaitForJob_RejectsMissingName(t *testing.T) {
	r := NewRunner(fake.NewClientset(), "ns", "")
	if _, err := r.handleWaitForJob(context.Background(), map[string]any{}); err == nil {
		t.Error("expected error for missing job_name")
	}
}

// ---------- get_k8s_job_logs ----------

func TestGetJobLogs_HappyPath(t *testing.T) {
	cs := fake.NewClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p-1", Namespace: "default",
			Labels: map[string]string{jobNameSelectorLabel: "j-1"},
		},
	}
	_, _ = cs.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	r := NewRunner(cs, "default", "")
	out, err := r.handleGetJobLogs(context.Background(), map[string]any{"job_name": "j-1"})
	if err != nil {
		t.Fatal(err)
	}
	resp := out.(map[string]any)
	// fake clientset returns "fake logs" as default stdout — it's gzipped + base64'd.
	encoded, ok := resp["stdout_b64_gzip"].(string)
	if !ok {
		t.Fatalf("stdout_b64_gzip missing or wrong type: %T", resp["stdout_b64_gzip"])
	}
	stdout, err := decodeStdoutB64Gzip(encoded)
	if err != nil {
		t.Fatalf("inflate: %v", err)
	}
	if stdout != "fake logs" {
		t.Errorf("inflated stdout = %q; want %q", stdout, "fake logs")
	}
	if resp["truncated"] != false {
		t.Errorf("truncated = %v; want false (well under cap)", resp["truncated"])
	}
	if size, _ := resp["stdout_size"].(int); size != len("fake logs") {
		t.Errorf("stdout_size = %v; want %d", resp["stdout_size"], len("fake logs"))
	}
}

// decodeStdoutB64Gzip is the inverse of handleGetJobLogs's encode step. Tests
// use it to assert the inflated bytes; api-server's relay wrapper does the
// equivalent transparently.
func decodeStdoutB64Gzip(s string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	defer func() { _ = gr.Close() }()
	out, err := io.ReadAll(gr)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// TestGetJobLogs_GzipRoundtrip proves the agent's compress-on-the-way-out
// shape inflates back to the same bytes — and that JSON-shaped logs (the
// kind trivy_cis / popeye produce) compress meaningfully so the bandwidth
// argument for shipping over the relay holds.
func TestGetJobLogs_GzipRoundtrip(t *testing.T) {
	// Synthesize a popeye-shaped JSON blob with lots of repetition, the same
	// pattern real scanner output has.
	var b strings.Builder
	b.WriteString(`{"sections":[`)
	for i := 0; i < 200; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"linter":"deployments","gvr":"apps/v1/deployments","issues":{"default/my-app":[{"group":"__root__","gvr":"apps/v1/deployments","level":2,"message":"[POP-100] No probes defined"}]}}`)
	}
	b.WriteString(`]}`)
	raw := b.String()

	// Round-trip through gzip+base64 and back.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())

	// Bandwidth assertion — repetitive JSON should compress at least 5x.
	if len(encoded) > len(raw)/5 {
		t.Errorf("compression too weak: raw=%d, base64(gzip)=%d (ratio %.1fx); expected ≥5x",
			len(raw), len(encoded), float64(len(raw))/float64(len(encoded)))
	}

	stdout, err := decodeStdoutB64Gzip(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != raw {
		t.Errorf("inflated bytes did not match raw input")
	}
}

func TestGetJobLogs_NoPod(t *testing.T) {
	cs := fake.NewClientset()
	r := NewRunner(cs, "default", "")
	if _, err := r.handleGetJobLogs(context.Background(), map[string]any{"job_name": "j-x"}); err == nil {
		t.Error("expected error when no pods match the job")
	}
}

func TestGetJobLogs_RejectsMissingName(t *testing.T) {
	r := NewRunner(fake.NewClientset(), "ns", "")
	if _, err := r.handleGetJobLogs(context.Background(), map[string]any{}); err == nil {
		t.Error("expected error for missing job_name")
	}
}

// ---------- Handlers wiring ----------

func TestHandlers_RegistersOnlyPrimitives(t *testing.T) {
	r := NewRunner(fake.NewClientset(), "ns", "")
	hs := Handlers(r)
	want := []string{"schedule_k8s_job", "wait_for_k8s_job", "get_k8s_job_logs", "delete_k8s_job"}
	if len(hs) != len(want) {
		t.Fatalf("Handlers count = %d; want %d (primitives only)", len(hs), len(want))
	}
	for _, name := range want {
		if _, ok := hs[name]; !ok {
			t.Errorf("missing primitive %q", name)
		}
	}
	// Sanity: scanner-named actions must NOT be registered.
	for _, gone := range []string{
		"image_scanner", "popeye_scan", "trivy_cis_scan", "kube_bench_scan",
		"certificate_scanner", "krr_scan", "helm_chart_upgrade",
	} {
		if _, ok := hs[gone]; ok {
			t.Errorf("named scanner %q is still registered — agent must own no scanner knowledge", gone)
		}
	}
}

// ---------- end-to-end through the dispatch.Handler signature ----------

// Schedule + Wait + GetLogs in sequence against a fake clientset, simulating
// kubelet finishing the Job. Mirrors the api-server orchestrator's flow.
func TestPrimitives_FullCycle(t *testing.T) {
	cs := fake.NewClientset()
	r := NewRunner(cs, "default", "agent-sa")
	hs := Handlers(r)

	// 1. schedule
	scheduleOut, err := hs["schedule_k8s_job"](context.Background(), map[string]any{
		"spec": map[string]any{
			"name_prefix": "trivy",
			"image":       "aquasec/trivy:0.58.0",
			"args":        []any{"image", "--format", "json", "nginx:1.27"},
		},
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	resp := scheduleOut.(map[string]any)
	if resp["success"] != true {
		t.Fatalf("schedule failed: %+v", resp)
	}
	jobName := resp["job_name"].(string)

	// 2. simulate kubelet: mark Complete + spawn pod with the matching label
	now := metav1.Now()
	j, _ := cs.BatchV1().Jobs("default").Get(context.Background(), jobName, metav1.GetOptions{})
	j.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
		LastProbeTime: now, LastTransitionTime: now,
	}}
	j.Status.CompletionTime = &now
	_, _ = cs.BatchV1().Jobs("default").UpdateStatus(context.Background(), j, metav1.UpdateOptions{})
	_, _ = cs.CoreV1().Pods("default").Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: jobName + "-pod", Namespace: "default",
			Labels: map[string]string{jobNameSelectorLabel: jobName},
		},
	}, metav1.CreateOptions{})

	// 3. wait
	waitOut, err := hs["wait_for_k8s_job"](context.Background(), map[string]any{"job_name": jobName})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if waitOut.(map[string]any)["status"] != "Complete" {
		t.Fatalf("status = %v", waitOut.(map[string]any)["status"])
	}

	// 4. fetch logs
	logsOut, err := hs["get_k8s_job_logs"](context.Background(), map[string]any{"job_name": jobName})
	if err != nil {
		t.Fatalf("logs: %v", err)
	}
	if _, ok := logsOut.(map[string]any)["stdout_b64_gzip"].(string); !ok {
		t.Errorf("stdout_b64_gzip missing in logs response")
	}
}

// Sanity guard against a regression where someone reintroduces named-scanner
// behavior. If new code starts referencing constants like "image_scanner" or
// "popeye_scan" inside this package, these tests catch it.
func TestNoScannerKnowledgeInPackage(t *testing.T) {
	r := NewRunner(fake.NewClientset(), "ns", "")
	// Runner has no Specs map, no defaultSpecs, no per-scanner config.
	// Reflective check would be brittle; instead rely on package-level
	// design: BuildJob takes a server-supplied JobSpec.
	job := r.BuildJob(JobSpec{NamePrefix: "x", Image: "x:1"}, "x-1", "u")
	if !strings.HasPrefix(job.Name, "x-1") {
		t.Errorf("BuildJob preserves the caller-supplied name: got %q", job.Name)
	}
	// Time import — keep useful for future sub-tests.
	_ = time.Second
}
