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
		wantOK   bool
	}{
		{LangJava, ProfileMemory, ToolJcmd, OutputHeapHistogram, true},
		{LangJava, ProfileCPU, ToolJcmd, OutputThreadDump, true},
		{LangPython, ProfileCPU, ToolPyspy, OutputFlameGraph, true},
		{LangPython, ProfileMemory, ToolAustin, OutputRaw, true},
		{LangNode, ProfileCPU, ToolPerf, OutputFlameGraph, true},
		{LangGo, ProfileCPU, ToolPprof, OutputPprof, true},
		{LangGo, ProfileMemory, ToolPprof, OutputHeapDump, true},
		{LangRuby, ProfileCPU, ToolRbspy, OutputFlameGraph, true},
		// Fallback path — `Bpf, FlameGraph` for any CPU profile not explicitly mapped.
		{LangRust, ProfileCPU, ToolBpf, OutputFlameGraph, true},
		{LangUnknown, ProfileCPU, ToolBpf, OutputFlameGraph, true},
		// No memory profiler for these: they used to come back as a CPU
		// flamegraph labelled "memory".
		{LangNode, ProfileMemory, "", "", false},
		{LangRuby, ProfileMemory, "", "", false},
		{LangClang, ProfileMemory, "", "", false},
		{LangRust, ProfileMemory, "", "", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.lang)+"/"+string(tc.ptype), func(t *testing.T) {
			tool, out, ok := profilingToolForType(tc.lang, tc.ptype)
			if tool != tc.wantTool || out != tc.wantOut || ok != tc.wantOK {
				t.Errorf("profilingToolForType(%q,%q) = (%q,%q,%v); want (%q,%q,%v)",
					tc.lang, tc.ptype, tool, out, ok, tc.wantTool, tc.wantOut, tc.wantOK)
			}
		})
	}
}

// TestResolveTool covers the requests the profiler screen and API callers
// send. The screen sends output_type "flamegraph" with every cpu/memory
// request; before profile_type decided the tool, that output silently won
// and every "memory" request for Python or Go came back as a CPU profile.
func TestResolveTool(t *testing.T) {
	cases := []struct {
		name     string
		lang     ProgrammingLanguage
		req      ProfileRequest
		wantTool ProfilingTool
		wantOut  OutputType
		wantErr  string
	}{
		// Profile type decides; flamegraph kept where the tool draws one.
		{name: "python memory flamegraph", lang: LangPython,
			req:      ProfileRequest{ProfileType: ProfileMemory, OutputType: OutputFlameGraph},
			wantTool: ToolAustin, wantOut: OutputFlameGraph},
		{name: "python memory default", lang: LangPython,
			req:      ProfileRequest{ProfileType: ProfileMemory},
			wantTool: ToolAustin, wantOut: OutputRaw},
		{name: "python cpu flamegraph", lang: LangPython,
			req:      ProfileRequest{ProfileType: ProfileCPU, OutputType: OutputFlameGraph},
			wantTool: ToolPyspy, wantOut: OutputFlameGraph},
		// go_pprof cannot draw a flamegraph; the profiler would hand back
		// a CPU pprof for either type, so the type's own output is used.
		{name: "go memory flamegraph", lang: LangGo,
			req:      ProfileRequest{ProfileType: ProfileMemory, OutputType: OutputFlameGraph},
			wantTool: ToolPprof, wantOut: OutputHeapDump},
		{name: "go cpu flamegraph", lang: LangGo,
			req:      ProfileRequest{ProfileType: ProfileCPU, OutputType: OutputFlameGraph},
			wantTool: ToolPprof, wantOut: OutputPprof},
		{name: "go cpu raw is not buildable", lang: LangGo,
			req:      ProfileRequest{ProfileType: ProfileCPU, OutputType: OutputRaw},
			wantTool: ToolPprof, wantOut: OutputPprof},
		{name: "node cpu flamegraph", lang: LangNode,
			req:      ProfileRequest{ProfileType: ProfileCPU, OutputType: OutputFlameGraph},
			wantTool: ToolPerf, wantOut: OutputFlameGraph},
		{name: "node memory unsupported", lang: LangNode,
			req:     ProfileRequest{ProfileType: ProfileMemory, OutputType: OutputFlameGraph},
			wantErr: "memory profiling is not available for node targets"},
		{name: "ruby memory unsupported", lang: LangRuby,
			req:     ProfileRequest{ProfileType: ProfileMemory},
			wantErr: "memory profiling is not available for ruby targets"},
		// Same profile type, another tool of the same language.
		{name: "java cpu flamegraph via async-profiler", lang: LangJava,
			req:      ProfileRequest{ProfileType: ProfileCPU, OutputType: OutputFlameGraph},
			wantTool: ToolAsyncProfiler, wantOut: OutputFlameGraph},
		{name: "java memory flamegraph stays a heap profile", lang: LangJava,
			req:      ProfileRequest{ProfileType: ProfileMemory, OutputType: OutputFlameGraph},
			wantTool: ToolJcmd, wantOut: OutputHeapHistogram},
		{name: "java memory heapdump", lang: LangJava,
			req:      ProfileRequest{ProfileType: ProfileMemory, OutputType: OutputHeapDump},
			wantTool: ToolJcmd, wantOut: OutputHeapDump},
		{name: "unknown profile type", lang: LangGo,
			req:     ProfileRequest{ProfileType: "wall"},
			wantErr: `unknown profile_type "wall"`},
		// A named tool runs as asked, whatever the profile type says.
		{name: "explicit tool kept", lang: LangGo,
			req:      ProfileRequest{ProfileType: ProfileMemory, ProfileTool: ToolPprof, OutputType: OutputFlameGraph},
			wantTool: ToolPprof, wantOut: OutputFlameGraph},
		{name: "explicit tool no output", lang: LangPython,
			req:      ProfileRequest{ProfileType: ProfileCPU, ProfileTool: ToolPyspy},
			wantTool: ToolPyspy, wantOut: OutputFlameGraph},
		// No profile type: output_type picks the tool, as before (the
		// screen's Java and native-language paths).
		{name: "java jfr no type", lang: LangJava,
			req:      ProfileRequest{OutputType: OutputJfr},
			wantTool: ToolJcmd, wantOut: OutputJfr},
		{name: "ruby flamegraph no type", lang: LangRuby,
			req:      ProfileRequest{OutputType: OutputFlameGraph},
			wantTool: ToolRbspy, wantOut: OutputFlameGraph},
		{name: "nothing given", lang: LangRust,
			req:      ProfileRequest{},
			wantTool: ToolBpf, wantOut: OutputFlameGraph},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool, out, err := resolveTool(tc.lang, tc.req)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v; want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tool != tc.wantTool || out != tc.wantOut {
				t.Errorf("resolveTool = (%q,%q); want (%q,%q)", tool, out, tc.wantTool, tc.wantOut)
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
// readProfilerEvents is streamUntilResult's parse loop over any reader, so
// the JSON-event parsing is covered without a real K8s log stream.

func scanForResult(t *testing.T, lines []string) (map[string]any, error) {
	t.Helper()
	res, _, err := readProfilerEvents(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	return res, err
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

// TestScanForResult_ErrorEventReason — the profiler's reason is kept as
// text (not a Go map dump) and as a typed error, which is what the Go
// pprof fallback inspects.
func TestScanForResult_ErrorEventReason(t *testing.T) {
	_, err := scanForResult(t, []string{
		`{"type":"progress","data":{"stage":"started"}}`,
		`{"type":"error","data":{"reason":"no PIDs found for container ID: abc"}}`,
	})
	var perr *profilerError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v (%T); want *profilerError", err, err)
	}
	if perr.Reason != "no PIDs found for container ID: abc" {
		t.Errorf("Reason = %q", perr.Reason)
	}
	if want := "pod_profiler: profiler reported error: no PIDs found for container ID: abc"; err.Error() != want {
		t.Errorf("err = %q; want %q", err, want)
	}
}

// TestReadProfilerEvents_TailWithoutVerdict — with no result or error the
// last lines come back for the error message, JSON events cut down to
// their payload and anything else kept as written.
func TestReadProfilerEvents_TailWithoutVerdict(t *testing.T) {
	lines := []string{`{"type":"progress","data":{"stage":"started"}}`}
	for i := 0; i < 30; i++ {
		lines = append(lines, "noise")
	}
	lines = append(lines, "panic: runtime error: index out of range")
	_, tail, err := readProfilerEvents(strings.NewReader(strings.Join(lines, "\n")))
	if !errors.Is(err, errStreamClosed) {
		t.Fatalf("err = %v; want errStreamClosed", err)
	}
	if len(tail) != debuggerLogTailLines {
		t.Fatalf("tail has %d lines; want %d", len(tail), debuggerLogTailLines)
	}
	if got := tail[len(tail)-1]; got != "panic: runtime error: index out of range" {
		t.Errorf("last tail line = %q", got)
	}
	if compactLogLine(`{"type":"notice","data":{"msg":"Detected more than one PID"}}`) != "notice: Detected more than one PID" {
		t.Error("JSON events should be rendered as their payload")
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
	if err == nil || err.Error() != "pod_profiler: pod shop/missing no longer exists" {
		t.Errorf("err = %v; want 'pod shop/missing no longer exists'", err)
	}
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			t.Errorf("unexpected %s %s for a missing target", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

// ---------- language detection ----------

// fakeProm returns a canned /api/v1/query body (or an error) for
// detectLang. It records the query so the test can assert the matcher.
// failures are returned, in order, by the first calls; err by every call.
type fakeProm struct {
	body     string
	err      error
	failures []error
	calls    int
	lastSeen string
}

func (f *fakeProm) Query(_ context.Context, query, _, _ string) (json.RawMessage, error) {
	f.lastSeen = query
	f.calls++
	if len(f.failures) > 0 {
		err := f.failures[0]
		f.failures = f.failures[1:]
		return nil, err
	}
	if f.err != nil {
		return nil, f.err
	}
	return json.RawMessage(f.body), nil
}

// promVector builds a success envelope carrying one series per
// application_type, in order.
func promVector(appTypes ...string) string {
	series := make([]string, 0, len(appTypes))
	for _, at := range appTypes {
		series = append(series, `{"metric":{"application_type":"`+at+`"},"value":[0,"1"]}`)
	}
	return `{"status":"success","data":{"resultType":"vector","result":[` +
		strings.Join(series, ",") + `]}}`
}

// TestDetectLang covers the application_type → language mapping plus
// every way detection is allowed to come back empty. LangUnknown is not
// a soft default here: Profile turns it into an error, so a wrong
// mapping silently reroutes a customer profile to the wrong tool.
func TestDetectLang(t *testing.T) {
	cases := []struct {
		name  string
		prom  *fakeProm
		want  ProgrammingLanguage
		noSet bool
	}{
		{name: "golang maps to go", prom: &fakeProm{body: promVector("golang")}, want: LangGo},
		{name: "java", prom: &fakeProm{body: promVector("java")}, want: LangJava},
		{name: "python", prom: &fakeProm{body: promVector("python")}, want: LangPython},
		{name: "nodejs maps to node", prom: &fakeProm{body: promVector("nodejs")}, want: LangNode},
		{name: "ruby", prom: &fakeProm{body: promVector("ruby")}, want: LangRuby},
		{name: "case insensitive", prom: &fakeProm{body: promVector("Java")}, want: LangJava},
		// A sidecar the node-agent labels envoy must not shadow the app.
		{name: "skips unprofilable sidecar", prom: &fakeProm{body: promVector("envoy", "java")}, want: LangJava},
		{name: "native-only pod stays unknown", prom: &fakeProm{body: promVector("nginx", "redis")}, want: LangUnknown},
		{name: "empty result", prom: &fakeProm{body: promVector()}, want: LangUnknown},
		{name: "query error", prom: &fakeProm{err: errors.New("boom")}, want: LangUnknown},
		{name: "prometheus error status", prom: &fakeProm{body: `{"status":"error","error":"bad"}`}, want: LangUnknown},
		{name: "malformed body", prom: &fakeProm{body: `not json`}, want: LangUnknown},
		{name: "no prometheus configured", noSet: true, want: LangUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewProfilerHandler(fake.NewClientset(), fakeRestConfig)
			h.langRetryDelay = time.Millisecond
			if !tc.noSet {
				h.SetLanguageDetector(tc.prom)
			}
			got, why := h.detectLang(context.Background(), "shop", "cart-0")
			if got != tc.want {
				t.Errorf("detectLang = %q; want %q", got, tc.want)
			}
			if got == LangUnknown && why == "" {
				t.Error("detectLang returned LangUnknown with no reason for the caller to report")
			}
		})
	}
}

// TestDetectLang_QuotesMatcher — a dot in a pod name is a regex
// wildcard; unescaped it would match other pods' containers.
func TestDetectLang_QuotesMatcher(t *testing.T) {
	p := &fakeProm{body: promVector()}
	h := NewProfilerHandler(fake.NewClientset(), fakeRestConfig)
	h.SetLanguageDetector(p)
	h.detectLang(context.Background(), "shop", "cart.0")

	// The backslash QuoteMeta adds must reach Prometheus as a literal, so it
	// is doubled: a bare `\.` inside a PromQL string literal is an unknown
	// escape sequence and fails the whole query at parse time.
	// last_over_time reaches back past a missed scrape, where an instant
	// selector would find no sample at "now".
	want := `last_over_time(container_application_type{container_id=~"/k8s/shop/cart\\.0/.*"}[15m])`
	if p.lastSeen != want {
		t.Errorf("query = %s; want %s", p.lastSeen, want)
	}
}

// TestDetectLang_RetriesFailedLookup — one failed lookup (a dropped
// connection, a busy Prometheus) is retried; an empty answer is not.
func TestDetectLang_RetriesFailedLookup(t *testing.T) {
	cases := []struct {
		name      string
		prom      *fakeProm
		want      ProgrammingLanguage
		wantCalls int
		wantWhy   string
	}{
		{name: "transient failure then answer",
			prom: &fakeProm{failures: []error{errors.New("connection reset by peer")}, body: promVector("golang")},
			want: LangGo, wantCalls: 2},
		{name: "failure twice",
			prom: &fakeProm{err: errors.New("connection refused")},
			want: LangUnknown, wantCalls: 2, wantWhy: "the container_application_type lookup failed: connection refused"},
		{name: "no series is an answer",
			prom: &fakeProm{body: promVector()},
			want: LangUnknown, wantCalls: 1, wantWhy: "no container_application_type metric reported for it"},
		{name: "rejected query is an answer",
			prom: &fakeProm{body: `{"status":"error","error":"bad"}`},
			want: LangUnknown, wantCalls: 1, wantWhy: "Prometheus rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewProfilerHandler(fake.NewClientset(), fakeRestConfig)
			h.langRetryDelay = time.Millisecond
			h.SetLanguageDetector(tc.prom)
			got, why := h.detectLang(context.Background(), "shop", "cart-0")
			if got != tc.want {
				t.Errorf("detectLang = %q (%s); want %q", got, why, tc.want)
			}
			if tc.prom.calls != tc.wantCalls {
				t.Errorf("queries = %d; want %d", tc.prom.calls, tc.wantCalls)
			}
			if !strings.Contains(why, tc.wantWhy) {
				t.Errorf("why = %q; want it to contain %q", why, tc.wantWhy)
			}
		})
	}
}

// TestDetectLang_NoRetryOnceCancelled — a lookup that failed because the
// run itself is over is not retried.
func TestDetectLang_NoRetryOnceCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &fakeProm{err: context.Canceled}
	h := NewProfilerHandler(fake.NewClientset(), fakeRestConfig)
	h.SetLanguageDetector(p)
	if got, _ := h.detectLang(ctx, "shop", "cart-0"); got != LangUnknown {
		t.Errorf("detectLang = %q; want unknown", got)
	}
	if p.calls != 1 {
		t.Errorf("queries = %d; want 1", p.calls)
	}
}

// TestProfile_UndetectableLanguageErrors locks the contract that
// replaced the old "assume Go" default: with no lang and no detection,
// the caller is told to name the language instead of getting a pprof
// scrape that 404s on every non-Go process.
func TestProfile_UndetectableLanguageErrors(t *testing.T) {
	cs := fake.NewClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "cart-0", Namespace: "shop"},
			Spec:       corev1.PodSpec{NodeName: "node-a"},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{ContainerID: "containerd://abc"}},
			},
		},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
	)
	h := NewProfilerHandler(cs, fakeRestConfig)
	h.SetLanguageDetector(&fakeProm{body: promVector("nginx")})

	_, err := h.Profile(context.Background(), ProfileRequest{Name: "cart-0", Namespace: "shop"})
	if err == nil || !strings.Contains(err.Error(), "could not determine the application language") {
		t.Errorf("err = %v; want 'could not determine the application language'", err)
	}
}

// TestProfile_RejectsDurationBeyondDeadline — pod_profiler runs under
// the dispatcher's 180s budget, so a longer profile can only end in
// "context deadline exceeded" once the whole budget has burned.
func TestProfile_RejectsDurationBeyondDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h := NewProfilerHandler(fake.NewClientset(), fakeRestConfig)
	_, err := h.Profile(ctx, ProfileRequest{Name: "cart-0", Namespace: "shop", Seconds: 300})
	if err == nil || !strings.Contains(err.Error(), "does not fit") {
		t.Errorf("err = %v; want 'does not fit'", err)
	}
}

// TestProfile_AcceptsDurationInsideDeadline guards the other side of that
// check: pod_profiler runs as a long action precisely so the UI's longest
// profile still fits, and rejecting those would be worse than the timeout.
func TestProfile_AcceptsDurationInsideDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()

	h := NewProfilerHandler(fake.NewClientset(), fakeRestConfig)
	// 600s is the maximum the profiler screen allows.
	_, err := h.Profile(ctx, ProfileRequest{Name: "cart-0", Namespace: "shop", Seconds: 600})
	if err != nil && strings.Contains(err.Error(), "does not fit") {
		t.Errorf("err = %v; a 600s profile must fit the long-action budget", err)
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
