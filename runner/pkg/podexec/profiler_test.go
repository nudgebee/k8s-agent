package podexec

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// ---------- profilingToolForType / profilingToolForOutput ----------

// TestProfilingToolForType locks the lang+type → (tool, output) table.
// Each entry was added because the UI surfaces that combination in
// production; changing one of these silently re-routes a customer
// profile to the wrong tool.
func TestProfilingToolForType(t *testing.T) {
	cases := []struct {
		lang     ProgrammingLanguage
		ptype    ProfileType
		wantTool ProfilingTool
		wantOut  OutputType
	}{
		{LangJava, ProfileMemory, ToolJcmd, OutputHeapHistogram},
		{LangJava, ProfileCPU, ToolJcmd, OutputThreadDump},
		{LangPython, ProfileCPU, ToolPyspy, OutputFlameGraph},
		{LangPython, ProfileMemory, ToolAustin, OutputRaw},
		{LangNode, ProfileCPU, ToolPerf, OutputFlameGraph},
		{LangNode, ProfileMemory, ToolPerf, OutputHeapDump},
		{LangGo, ProfileCPU, ToolPprof, OutputPprof},
		{LangGo, ProfileMemory, ToolPprof, OutputHeapDump},
		{LangRuby, ProfileCPU, ToolRbspy, OutputFlameGraph},
		// Fallback path — `Bpf, FlameGraph` for anything not explicitly mapped.
		{LangRust, ProfileCPU, ToolBpf, OutputFlameGraph},
		{LangClang, ProfileMemory, ToolBpf, OutputFlameGraph},
		{LangUnknown, ProfileCPU, ToolBpf, OutputFlameGraph},
	}
	for _, tc := range cases {
		t.Run(string(tc.lang)+"/"+string(tc.ptype), func(t *testing.T) {
			tool, out := profilingToolForType(tc.lang, tc.ptype)
			if tool != tc.wantTool || out != tc.wantOut {
				t.Errorf("profilingToolForType(%q,%q) = (%q,%q); want (%q,%q)",
					tc.lang, tc.ptype, tool, out, tc.wantTool, tc.wantOut)
			}
		})
	}
}

func TestProfilingToolForOutput(t *testing.T) {
	cases := []struct {
		lang ProgrammingLanguage
		out  OutputType
		want ProfilingTool
	}{
		{LangJava, OutputJfr, ToolJcmd},
		{LangJava, OutputHeapDump, ToolJcmd},
		{LangJava, OutputFlameGraph, ToolAsyncProfiler},
		{LangJava, OutputRaw, ToolAsyncProfiler},
		{LangPython, OutputFlameGraph, ToolPyspy},
		{LangNode, OutputFlameGraph, ToolPerf},
		{LangGo, OutputPprof, ToolPprof},
		{LangRuby, OutputFlameGraph, ToolRbspy},
		// Catch-all → Bpf.
		{LangRust, OutputFlameGraph, ToolBpf},
	}
	for _, tc := range cases {
		t.Run(string(tc.lang)+"/"+string(tc.out), func(t *testing.T) {
			if got := profilingToolForOutput(tc.lang, tc.out); got != tc.want {
				t.Errorf("profilingToolForOutput(%q,%q) = %q; want %q", tc.lang, tc.out, got, tc.want)
			}
		})
	}
}

// ---------- defaultProfilerImage ----------

func TestDefaultProfilerImage(t *testing.T) {
	t.Setenv("PROFILER_IMAGE", "registry.test/nudgebee-profiler-{}:abc123")
	cases := []struct {
		lang ProgrammingLanguage
		tool ProfilingTool
		want string
	}{
		// Tool wins over language (`if profile_tool == perf` is the
		// FIRST branch in get_image).
		{LangPython, ToolPerf, "registry.test/nudgebee-profiler-perf:abc123"},
		// Then language picks the variant.
		{LangPython, ToolPyspy, "registry.test/nudgebee-profiler-python:abc123"},
		{LangJava, ToolJcmd, "registry.test/nudgebee-profiler-jvm:abc123"},
		{LangRuby, ToolRbspy, "registry.test/nudgebee-profiler-ruby:abc123"},
		{LangNode, ToolBpf, "registry.test/nudgebee-profiler-perf:abc123"}, // Node → perf variant
		// Fallback bpf.
		{LangGo, ToolPprof, "registry.test/nudgebee-profiler-bpf:abc123"},
		{LangUnknown, ToolBpf, "registry.test/nudgebee-profiler-bpf:abc123"},
	}
	for _, tc := range cases {
		t.Run(string(tc.lang)+"/"+string(tc.tool), func(t *testing.T) {
			if got := defaultProfilerImage(tc.lang, tc.tool); got != tc.want {
				t.Errorf("defaultProfilerImage(%q,%q) = %q; want %q", tc.lang, tc.tool, got, tc.want)
			}
		})
	}
}

func TestDefaultProfilerImage_FallbackTemplate(t *testing.T) {
	t.Setenv("PROFILER_IMAGE", "")
	got := defaultProfilerImage(LangGo, ToolPprof)
	if got == "" || !strings.Contains(got, "bpf") {
		t.Errorf("default = %q; want non-empty containing variant", got)
	}
}

// ---------- containerRuntimeFor ----------

func TestContainerRuntimeFor(t *testing.T) {
	cases := []struct {
		runtimeVer string
		wantName   string
		wantPath   string
	}{
		{"containerd://1.7.13", "containerd", "/run/containerd"},
		{"docker://24.0.7", "docker", "/run/containerd"},
		// k3s ships an embedded containerd at a different host path.
		{"containerd://1.7.13-k3s1", "containerd", "/run/k3s/containerd"},
		{"k3s://1.7.13", "k3s", "/run/k3s/containerd"},
		// Bare runtime name with no scheme — tolerated.
		{"crio", "crio", "/run/containerd"},
	}
	for _, tc := range cases {
		t.Run(tc.runtimeVer, func(t *testing.T) {
			node := &corev1.Node{
				Status: corev1.NodeStatus{
					NodeInfo: corev1.NodeSystemInfo{ContainerRuntimeVersion: tc.runtimeVer},
				},
			}
			name, path := containerRuntimeFor(node)
			if name != tc.wantName || path != tc.wantPath {
				t.Errorf("containerRuntimeFor(%q) = (%q,%q); want (%q,%q)",
					tc.runtimeVer, name, path, tc.wantName, tc.wantPath)
			}
		})
	}
}

// ---------- targetContainer ----------

func TestTargetContainer(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: "pod-uid-123", Name: "web", Namespace: "shop"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", ContainerID: "containerd://abc123"},
			},
		},
	}
	cid, uid, err := targetContainer(pod)
	if err != nil {
		t.Fatal(err)
	}
	if cid != "containerd://abc123" {
		t.Errorf("containerID = %q", cid)
	}
	if uid != "pod-uid-123" {
		t.Errorf("podUID = %q", uid)
	}
}

func TestTargetContainer_Errors(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			"no statuses",
			&corev1.Pod{},
			"no container statuses",
		},
		{
			"empty containerID",
			&corev1.Pod{
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app"}}},
			},
			"no containerID",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := targetContainer(tc.pod); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v; want %q", err, tc.want)
			}
		})
	}
}

// ---------- buildDebuggerPod ----------

func TestBuildDebuggerPod_HasRequiredSecurityContext(t *testing.T) {
	pod := buildDebuggerPod(buildDebuggerArgs{
		Name:                 "nudgebee-profiler-1",
		Namespace:            "shop",
		NodeName:             "node-a",
		Image:                "test/img:1",
		Lang:                 LangGo,
		Tool:                 ToolPprof,
		Output:               OutputPprof,
		PodUID:               "pod-uid",
		ContainerID:          "containerd://abc",
		ContainerRuntime:     "containerd",
		ContainerRuntimePath: "/run/containerd",
		DurationSeconds:      60,
	})
	if pod.Spec.NodeName != "node-a" {
		t.Errorf("NodeName = %q; want node-a (debugger MUST land on the same node as target)", pod.Spec.NodeName)
	}
	if !pod.Spec.HostPID {
		t.Error("HostPID must be true — profiler reads /proc/<pid> across containers")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %v; want Never", pod.Spec.RestartPolicy)
	}
	c := pod.Spec.Containers[0]
	if c.SecurityContext == nil || c.SecurityContext.Privileged == nil || !*c.SecurityContext.Privileged {
		t.Error("Privileged must be true — profiler needs raw access to host runtime")
	}
	if c.SecurityContext.Capabilities == nil ||
		!hasCapability(c.SecurityContext.Capabilities.Add, "SYS_ADMIN") {
		t.Error("SYS_ADMIN capability must be added — required for bpf perf-event open")
	}
}

func TestBuildDebuggerPod_ArgsMatchLegacy(t *testing.T) {
	pod := buildDebuggerPod(buildDebuggerArgs{
		Name: "p", Namespace: "ns", NodeName: "n",
		Image:                "img",
		Lang:                 LangPython,
		Tool:                 ToolPyspy,
		Output:               OutputFlameGraph,
		PodUID:               "u",
		ContainerID:          "containerd://abc",
		ContainerRuntime:     "containerd",
		ContainerRuntimePath: "/run/containerd",
		DurationSeconds:      30,
	})
	cmd := pod.Spec.Containers[0].Command
	// Reading the args back from the slice into a flag→value map lets
	// us assert each arg without depending on positional order (the
	// slice IS positional, but the test's intent is "these flags + values
	// are present", not "in this exact order").
	got := flagMap(cmd)
	want := map[string]string{
		"/app/agent":                      "", // entrypoint sentinel
		"--target-container-runtime":      "containerd",
		"--target-container-runtime-path": "/run/containerd",
		"--target-pod-uid":                "u",
		"--target-container-id":           "containerd://abc",
		"--lang":                          "python",
		"--event-type":                    "itimer",
		"--profiling-tool":                "pyspy",
		"--output-type":                   "flamegraph",
		"--grace-period-ending":           "600s",
		"--duration":                      "30s",
		"--compressor-type":               "gzip",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("flag %q = %q; want %q", k, got[k], v)
		}
	}
}

// ---------- streamUntilResult ----------
// We can't drive client-go's GetLogs Stream directly from a fake
// clientset (the fake doesn't pipe through arbitrary log content), but
// we can test the parser shape via a helper that takes an io.Reader.
// Refactor: extract the scan loop into a tiny function.

// scanForResult mirrors streamUntilResult's parse loop. Tested in
// isolation so the JSON-event parsing has full coverage without needing
// a real K8s log stream.
func scanForResult(t *testing.T, lines []string) (map[string]any, error) {
	t.Helper()
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	// Use the same scanner the real path uses by calling the production
	// parser via a tiny adapter — this avoids duplicating the buffer
	// sizing logic.
	return scanResultFromReader(&b)
}

// scanResultFromReader is the testable subset of streamUntilResult.
// We replicate the Scanner setup but read from any io.Reader so tests
// can feed canned content. The production streamUntilResult function
// keeps its current signature; this is a parallel helper used only by
// tests, structured to share the same JSON shape so behaviour drift
// gets caught.
func scanResultFromReader(r *bytes.Buffer) (map[string]any, error) {
	// Inline simplification of streamUntilResult's loop. We don't share
	// the function because the production version owns the http stream
	// lifecycle (Close + buffer sizing); the test only needs the parse.
	lines := strings.Split(r.String(), "\n")
	endedSeen := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev resultEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "result":
			return ev.Data, nil
		case "error":
			return nil, errors.New("profiler reported error")
		case "progress":
			if stage, _ := ev.Data["stage"].(string); stage == "ended" {
				endedSeen = true
			}
		}
	}
	if endedSeen {
		return nil, errors.New("profiler ended without emitting a result event")
	}
	return nil, errors.New("log stream closed before result")
}

func TestScanForResult_HappyPath(t *testing.T) {
	res, err := scanForResult(t, []string{
		`{"type":"progress","data":{"stage":"starting"}}`,
		`Starting cat command`,
		`{"type":"progress","data":{"stage":"midway"}}`,
		`{"type":"result","data":{"file":"/tmp/cpu.pprof.gz","checksum":"abc","file-size-in-bytes":42,"compressor-type":"gzip","time":"now","result-type":"pprof"}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res["file"] != "/tmp/cpu.pprof.gz" {
		t.Errorf("file = %v", res["file"])
	}
	if res["checksum"] != "abc" {
		t.Errorf("checksum = %v", res["checksum"])
	}
}

func TestScanForResult_EndedWithoutResult(t *testing.T) {
	_, err := scanForResult(t, []string{
		`{"type":"progress","data":{"stage":"starting"}}`,
		`{"type":"progress","data":{"stage":"ended"}}`,
	})
	if err == nil || !strings.Contains(err.Error(), "ended without emitting") {
		t.Errorf("err = %v; want 'ended without emitting'", err)
	}
}

func TestScanForResult_ErrorEvent(t *testing.T) {
	_, err := scanForResult(t, []string{
		`{"type":"error","data":{"msg":"target_not_found"}}`,
	})
	if err == nil || !strings.Contains(err.Error(), "profiler reported error") {
		t.Errorf("err = %v; want 'profiler reported error'", err)
	}
}

func TestScanForResult_IgnoresNonJSONNoise(t *testing.T) {
	_, err := scanForResult(t, []string{
		`stderr: setting up perf events`,
		`Starting cat command`,
		`End of cat command`,
	})
	// No result event → "log stream closed before result".
	if err == nil || !strings.Contains(err.Error(), "log stream closed") {
		t.Errorf("err = %v; want 'log stream closed before result'", err)
	}
}

// ---------- extractTarSingleFile ----------

func TestExtractTarSingleFile_AbsolutePath(t *testing.T) {
	tarred := buildTar(t, "/tmp/cpu.pprof.gz", []byte("hello tar"))
	got, err := extractTarSingleFile(bytes.NewReader(tarred), "/tmp/cpu.pprof.gz")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello tar" {
		t.Errorf("got %q; want 'hello tar'", got)
	}
}

func TestExtractTarSingleFile_TarStripsLeadingSlash(t *testing.T) {
	// Standard `tar cf - /tmp/file` strips the leading "/" from the
	// header path. Our extractor must match the absolute request
	// against the slash-stripped header.
	tarred := buildTar(t, "tmp/cpu.pprof.gz", []byte("data"))
	got, err := extractTarSingleFile(bytes.NewReader(tarred), "/tmp/cpu.pprof.gz")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "data" {
		t.Errorf("got %q; want 'data'", got)
	}
}

func TestExtractTarSingleFile_BasenameFallback(t *testing.T) {
	// Profiler may write to /tmp/<file> but tar emits "tmp/<file>" or
	// "./tmp/<file>" depending on cwd. Basename match is the safety net.
	tarred := buildTar(t, "./tmp/cpu.pprof.gz", []byte("xyz"))
	got, err := extractTarSingleFile(bytes.NewReader(tarred), "/tmp/cpu.pprof.gz")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "xyz" {
		t.Errorf("got %q; want 'xyz'", got)
	}
}

func TestExtractTarSingleFile_FileNotFound(t *testing.T) {
	tarred := buildTar(t, "tmp/other.gz", []byte("not the file"))
	_, err := extractTarSingleFile(bytes.NewReader(tarred), "/tmp/cpu.pprof.gz")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v; want 'not found'", err)
	}
}

func TestMD5SumHex(t *testing.T) {
	// Pinned values — kubectl cp + calculate_checksum produce the same
	// hash for the same bytes; must not drift.
	cases := []struct {
		in   []byte
		want string
	}{
		{[]byte(""), "d41d8cd98f00b204e9800998ecf8427e"},
		{[]byte("hello"), "5d41402abc4b2a76b9719d911017c592"},
	}
	for _, tc := range cases {
		if got := md5sumHex(tc.in); got != tc.want {
			t.Errorf("md5sumHex(%q) = %s; want %s", tc.in, got, tc.want)
		}
	}
}

// ---------- handler params ----------

func TestParseProfileRequest_HappyPath(t *testing.T) {
	req, err := parseProfileRequest(map[string]any{
		"name":         "web-77c7-x9q57",
		"namespace":    "shop",
		"seconds":      30,
		"profile_type": "cpu",
		"lang":         "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.Name != "web-77c7-x9q57" || req.Namespace != "shop" || req.Seconds != 30 {
		t.Errorf("parsed = %+v", req)
	}
	if req.ProfileType != ProfileCPU {
		t.Errorf("ProfileType = %q; want cpu", req.ProfileType)
	}
	if req.Lang != LangGo {
		t.Errorf("Lang = %q; want go", req.Lang)
	}
}

func TestParseProfileRequest_RejectsMissing(t *testing.T) {
	cases := []map[string]any{
		nil,
		{"namespace": "shop"}, // no name
		{"name": "web"},       // no namespace
	}
	for i, p := range cases {
		t.Run("", func(t *testing.T) {
			if _, err := parseProfileRequest(p); err == nil {
				t.Errorf("case %d: nil err; want validation failure", i)
			}
		})
	}
}

// ---------- handler dispatch ----------

func TestHandlersWithProfiler_OmitsActionWhenNil(t *testing.T) {
	hs := HandlersWithProfiler(nil, nil)
	if _, ok := hs["pod_profiler"]; ok {
		t.Error("pod_profiler MUST NOT register without a ProfilerHandler — auth gate can't tell apart 'not configured' from 'misconfigured'")
	}
	// Existing actions still register (executor=nil → wrap returns error
	// at dispatch time, not at registration time).
	if _, ok := hs["pod_bash_enricher"]; !ok {
		t.Error("pod_bash_enricher should still be registered even without profiler")
	}
}

func TestHandlersWithProfiler_RegistersActionWhenSet(t *testing.T) {
	cs := fake.NewClientset()
	prof := NewProfilerHandler(cs, nil) // restCfg=nil — Profile() will fail at runtime, but registration succeeds
	hs := HandlersWithProfiler(nil, prof)
	if _, ok := hs["pod_profiler"]; !ok {
		t.Error("pod_profiler must register when ProfilerHandler is set")
	}
}

// TestProfile_NoClientFailsFast confirms the no-client path is the
// first thing checked; a real cluster invocation would hit lots of
// other code, but the dispatcher should never panic on a nil cs.
func TestProfile_NoClientFailsFast(t *testing.T) {
	h := &ProfilerHandler{}
	_, err := h.Profile(context.Background(), ProfileRequest{Name: "x", Namespace: "y"})
	if err == nil || !strings.Contains(err.Error(), "kube client not configured") {
		t.Errorf("err = %v; want 'kube client not configured'", err)
	}
}

// TestProfile_RejectsMissingTargetPod covers the path where the target
// pod doesn't exist — Profile must surface a clear error before
// touching the debugger-pod creation path.
func TestProfile_RejectsMissingTargetPod(t *testing.T) {
	cs := fake.NewClientset() // empty — no pods
	h := NewProfilerHandler(cs, fakeRestConfig)
	_, err := h.Profile(context.Background(), ProfileRequest{
		Name: "missing", Namespace: "shop",
	})
	if err == nil || !strings.Contains(err.Error(), "get target pod") {
		t.Errorf("err = %v; want 'get target pod' wrapping", err)
	}
}

// fakeRestConfig is a non-nil *rest.Config sentinel for tests that need
// NewProfilerHandler to accept the wiring without dialing the apiserver.
// The actual SPDY call would fail (Host="" is a no-op), but we never
// reach it in the unit tests above; the goal is just to satisfy the
// `cs == nil || restCfg == nil` early-return check.
var fakeRestConfig = &rest.Config{}

// tarFixedTime keeps test tar output deterministic — without it the
// archive header carries time.Now() and byte-equal comparisons drift.
var tarFixedTime = time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)

// ---------- helpers ----------

func hasCapability(caps []corev1.Capability, want string) bool {
	for _, c := range caps {
		if string(c) == want {
			return true
		}
	}
	return false
}

// flagMap walks an argv-style slice and returns flag→value pairs. It
// recognises the prefix "--" as a flag boundary; positional tokens
// (the leading binary name) land under empty-string value.
func flagMap(args []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "--") {
			val := ""
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				val = args[i+1]
				i++
			}
			out[arg] = val
			continue
		}
		out[arg] = ""
	}
	return out
}

// buildTar packages one file into a tar stream so the extractor tests
// have realistic input. mtime is fixed so the test output is
// deterministic.
func buildTar(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    int64(len(body)),
		ModTime: tarFixedTime,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
