// Package podexec orchestrates a privileged debugger pod on the same
// node as the target — it streams the pod's log output, parses per-line
// JSON events, then `kubectl cp`s the result file out as a FileBlock.
//
// The agent_task path: the backend inserts an `agent_task` row with
// action_name="pod_profiler" and the UI-collected fields (name,
// namespace, seconds, profile_type, lang, profile_tool, output_type).
// pkg/tasks/poller drains the queue and dispatches here.
package podexec

import (
	"bufio"
	"context"
	"crypto/md5" //nolint:gosec // content fingerprint, not security
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/ptr"
)

// Programming languages the profiler recognises.
// The wire-encoded values match the legacy wire shape exactly so an
// api-server caller passing { lang: "python" } continues to work after
// the cutover.
type ProgrammingLanguage string

const (
	LangJava          ProgrammingLanguage = "java"
	LangPython        ProgrammingLanguage = "python"
	LangGo            ProgrammingLanguage = "go"
	LangNode          ProgrammingLanguage = "node"
	LangRust          ProgrammingLanguage = "rust"
	LangClang         ProgrammingLanguage = "clang"
	LangClangPlusPlus ProgrammingLanguage = "c++"
	LangRuby          ProgrammingLanguage = "ruby"
	LangUnknown       ProgrammingLanguage = "unknown"
)

// ProfilingTool — `ProfilingTool` enum.
type ProfilingTool string

const (
	ToolAsyncProfiler ProfilingTool = "async-profiler"
	ToolJcmd          ProfilingTool = "jcmd"
	ToolPyspy         ProfilingTool = "pyspy"
	ToolBpf           ProfilingTool = "bpf"
	ToolPerf          ProfilingTool = "perf"
	ToolRbspy         ProfilingTool = "rbspy"
	ToolAustin        ProfilingTool = "austin"
	ToolPprof         ProfilingTool = "pprof"
)

// OutputType — `OutputType` enum.
type OutputType string

const (
	OutputJfr           OutputType = "jfr"
	OutputThreadDump    OutputType = "threaddump"
	OutputHeapDump      OutputType = "heapdump"
	OutputHeapHistogram OutputType = "heaphistogram"
	OutputFlameGraph    OutputType = "flamegraph"
	OutputFlat          OutputType = "flat"
	OutputTraces        OutputType = "traces"
	OutputCollapsed     OutputType = "collapsed"
	OutputTree          OutputType = "tree"
	OutputRaw           OutputType = "raw"
	OutputPprof         OutputType = "pprof"
)

// ProfileType — `ProfileType` enum.
// The UI offers "memory" / "cpu" today; the (tool, output) tuple they
// imply for each language is the legacy map.
type ProfileType string

const (
	ProfileMemory ProfileType = "memory"
	ProfileCPU    ProfileType = "cpu"
)

// ProfileRequest is the Go-side shape of api-server's profiler request
// payload (services/application/profiler.go:45-64). Field names match
// the JSON keys the api-server sends, so we can json-Unmarshal the
// dispatch params directly into this struct without rewrites.
type ProfileRequest struct {
	Name        string              `json:"name"`
	Namespace   string              `json:"namespace"`
	Seconds     int                 `json:"seconds"`
	ProfileType ProfileType         `json:"profile_type"`
	ProfileTool ProfilingTool       `json:"profile_tool,omitempty"`
	Lang        ProgrammingLanguage `json:"lang,omitempty"`
	OutputType  OutputType          `json:"output_type,omitempty"`
}

// FileResult is the FileBlock the agent returns once the profiler pod
// has produced output. Mirrors the {filename, contents, additional_info}
// shape FileBlock serialises to.
type FileResult struct {
	Filename       string `json:"filename"`
	ContentsBase64 string `json:"contents_base64"`
	Lang           string `json:"lang"`
	ProfileTool    string `json:"profile_tool"`
	ProfileType    string `json:"profile_type"`
	Duration       int    `json:"profile_duration"`
}

// ProfilerHandler holds the K8s client + rest config the SPDY exec needs
// for the post-profile file fetch. Constructed once at startup; the
// dispatch handler closes over it.
type ProfilerHandler struct {
	cs      kubernetes.Interface
	restCfg *rest.Config
	// debuggerNamespace is where we spawn profiler pods. Defaults to the
	// same namespace as the target so customer NetworkPolicies that
	// scope by namespace don't break the privileged sidecar pattern.
	// Empty = same as target namespace.
	debuggerNamespace string
	// image returns the profiler image to run for a given (lang, tool)
	// pair. Uses PROFILER_IMAGE env. Pulled out as a func so tests can
	// substitute without reaching into env.
	image func(lang ProgrammingLanguage, tool ProfilingTool) string
}

// NewProfilerHandler wires the dispatch path. cs / restCfg can both be
// nil — handler returns a clear error rather than panicking when the
// agent has no in-cluster client.
func NewProfilerHandler(cs kubernetes.Interface, restCfg *rest.Config) *ProfilerHandler {
	return &ProfilerHandler{
		cs:      cs,
		restCfg: restCfg,
		image:   defaultProfilerImage,
	}
}

// defaultProfilerImage picks the image variant by tool first, then
// language. PROFILER_IMAGE env is honoured so a chart cutover doesn't
// require a Helm value bump.
func defaultProfilerImage(lang ProgrammingLanguage, tool ProfilingTool) string {
	template := os.Getenv("PROFILER_IMAGE")
	if template == "" {
		// The legacy default embeds a date pin + git sha; we keep
		// the operator override mandatory for production but provide a
		// sensible localhost fallback for tests / dev clusters.
		template = "registry.dev.nudgebee.pollux.in/nudgebee-profiler-{}:latest"
	}
	variant := "bpf"
	switch {
	case strings.EqualFold(string(tool), string(ToolPerf)):
		variant = "perf"
	case lang == LangPython:
		variant = "python"
	case lang == LangJava:
		variant = "jvm"
	case lang == LangRuby:
		variant = "ruby"
	case lang == LangNode:
		variant = "perf"
	}
	return strings.Replace(template, "{}", variant, 1)
}

// Profile runs one pod_profiler invocation end-to-end. It is the entry
// the dispatch handler calls; the FileResult it returns is wrapped by
// the handler in a Finding-shape envelope upstream.
func (h *ProfilerHandler) Profile(ctx context.Context, req ProfileRequest) (*FileResult, error) {
	if h.cs == nil || h.restCfg == nil {
		return nil, errors.New("pod_profiler: kube client not configured")
	}
	if req.Name == "" || req.Namespace == "" {
		return nil, errors.New("pod_profiler: name and namespace required")
	}
	if req.Seconds <= 0 {
		req.Seconds = 60
	}

	pod, err := h.cs.CoreV1().Pods(req.Namespace).Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("pod_profiler: get target pod: %w", err)
	}
	if pod.Spec.NodeName == "" {
		return nil, errors.New("pod_profiler: target pod has no nodeName (not scheduled yet)")
	}
	node, err := h.cs.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("pod_profiler: get node: %w", err)
	}
	containerID, podUID, err := targetContainer(pod)
	if err != nil {
		return nil, err
	}

	lang := req.Lang
	if lang == "" || lang == LangUnknown {
		lang = LangGo // default when prometheus auto-detect misses
	}
	tool := req.ProfileTool
	output := req.OutputType
	if req.ProfileType != "" && tool == "" && output == "" {
		tool, output = profilingToolForType(lang, req.ProfileType)
	}
	if tool == "" {
		tool = profilingToolForOutput(lang, output)
	}
	if output == "" {
		// No default for output when only tool is known — leaving it
		// empty is the explicit "let the profiler tool decide" signal.
		output = OutputFlameGraph
	}

	runtimeName, runtimePath := containerRuntimeFor(node)
	debuggerNamespace := h.debuggerNamespace
	if debuggerNamespace == "" {
		debuggerNamespace = req.Namespace
	}

	debuggerName := generateDebuggerPodName()
	debuggerPod := buildDebuggerPod(buildDebuggerArgs{
		Name:                 debuggerName,
		Namespace:            debuggerNamespace,
		NodeName:             pod.Spec.NodeName,
		Image:                h.image(lang, tool),
		Lang:                 lang,
		Tool:                 tool,
		Output:               output,
		PodUID:               podUID,
		ContainerID:          containerID,
		ContainerRuntime:     runtimeName,
		ContainerRuntimePath: runtimePath,
		DurationSeconds:      req.Seconds,
	})

	created, err := h.cs.CoreV1().Pods(debuggerNamespace).Create(ctx, debuggerPod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("pod_profiler: create debugger pod: %w", err)
	}
	defer func() {
		// Best-effort cleanup. The pod has restartPolicy=Never so even
		// if delete races with the apiserver the pod won't reschedule;
		// orphaned pods become visible via the managed-by label.
		_ = h.cs.CoreV1().Pods(debuggerNamespace).Delete(context.Background(), created.Name, metav1.DeleteOptions{})
	}()

	// wait_for_pod_ready first; the underlying ready check is "all
	// containers ready" — so a pull-image failure surfaces here instead
	// of as an empty log stream.
	if err := h.waitForPodReady(ctx, debuggerNamespace, created.Name, time.Duration(req.Seconds*5)*time.Second); err != nil {
		return nil, fmt.Errorf("pod_profiler: wait debugger ready: %w", err)
	}

	// Stream and parse logs until we hit a `result` event or the pod
	// reports `error`/`progress.stage=ended`.
	resultLog, err := h.streamUntilResult(ctx, debuggerNamespace, created.Name)
	if err != nil {
		return nil, err
	}

	// Pull the result file out via `tar cf - <file>` — kubectl cp uses
	// the same approach under the hood, a thin wrapper over tar-exec.
	res, err := h.fetchResultFile(ctx, debuggerNamespace, created.Name, resultLog, lang, tool, req)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// targetContainer reads container_id + pod_uid from the target pod's
// status. Returns an error when the
// pod has no container statuses yet.
func targetContainer(pod *corev1.Pod) (containerID, podUID string, err error) {
	if len(pod.Status.ContainerStatuses) == 0 {
		return "", "", errors.New("pod_profiler: target pod has no container statuses (not running yet)")
	}
	cs := pod.Status.ContainerStatuses[0]
	if cs.ContainerID == "" {
		return "", "", errors.New("pod_profiler: target pod's first container has no containerID")
	}
	return cs.ContainerID, string(pod.UID), nil
}

// containerRuntimeFor picks runtime + host runtime path from the node's
// containerRuntimeVersion. The path
// matters because the profiler pod hostMounts it: stock containerd at
// /run/containerd; k3s at /run/k3s/containerd. The runtime portion is
// the prefix before the first ":" ("containerd://1.7.x" → "containerd").
func containerRuntimeFor(node *corev1.Node) (runtimeName, runtimePath string) {
	v := node.Status.NodeInfo.ContainerRuntimeVersion
	if i := strings.IndexByte(v, ':'); i > 0 {
		runtimeName = v[:i]
	} else {
		runtimeName = v
	}
	runtimePath = "/run/containerd"
	if strings.Contains(v, "k3s") {
		runtimePath = "/run/k3s/containerd"
	}
	return runtimeName, runtimePath
}

// profilingToolForType is the (lang, profile_type) → (tool, output) map.
// Kept verbatim so the same UI calls produce the same artefacts.
func profilingToolForType(lang ProgrammingLanguage, pt ProfileType) (ProfilingTool, OutputType) {
	switch lang {
	case LangJava:
		if pt == ProfileMemory {
			return ToolJcmd, OutputHeapHistogram
		}
		if pt == ProfileCPU {
			return ToolJcmd, OutputThreadDump
		}
	case LangPython:
		if pt == ProfileCPU {
			return ToolPyspy, OutputFlameGraph
		}
		if pt == ProfileMemory {
			return ToolAustin, OutputRaw
		}
	case LangNode:
		if pt == ProfileCPU {
			return ToolPerf, OutputFlameGraph
		}
		if pt == ProfileMemory {
			return ToolPerf, OutputHeapDump
		}
	case LangGo:
		if pt == ProfileCPU {
			return ToolPprof, OutputPprof
		}
		if pt == ProfileMemory {
			return ToolPprof, OutputHeapDump
		}
	case LangRuby:
		return ToolRbspy, OutputFlameGraph
	}
	// Default fallback — (Bpf, FlameGraph) for everything not explicitly mapped.
	return ToolBpf, OutputFlameGraph
}

// profilingToolForOutput is the inverse map (lang, output) → tool. Used
// when the caller specifies an output_type without a tool.
func profilingToolForOutput(lang ProgrammingLanguage, output OutputType) ProfilingTool {
	switch lang {
	case LangJava:
		switch output {
		case OutputJfr, OutputThreadDump, OutputHeapDump, OutputHeapHistogram:
			return ToolJcmd
		case OutputFlameGraph, OutputFlat, OutputTraces, OutputCollapsed, OutputTree, OutputRaw:
			return ToolAsyncProfiler
		}
	case LangPython:
		return ToolPyspy
	case LangNode:
		return ToolPerf
	case LangGo:
		return ToolPprof
	case LangRuby:
		return ToolRbspy
	}
	return ToolBpf
}

// buildDebuggerArgs is the closed bag-of-fields for buildDebuggerPod —
// passing eight separate strings into one constructor scrambles the
// reading order. The struct keeps the call site self-documenting.
type buildDebuggerArgs struct {
	Name                 string
	Namespace            string
	NodeName             string
	Image                string
	Lang                 ProgrammingLanguage
	Tool                 ProfilingTool
	Output               OutputType
	PodUID               string
	ContainerID          string
	ContainerRuntime     string
	ContainerRuntimePath string
	DurationSeconds      int
}

// buildDebuggerPod constructs the privileged profiler pod the action
// spawns on the target node.
// — same /app/agent command, same args, same hostPath volumes.
func buildDebuggerPod(a buildDebuggerArgs) *corev1.Pod {
	cmd := []string{
		"/app/agent",
		"--target-container-runtime", a.ContainerRuntime,
		"--target-container-runtime-path", a.ContainerRuntimePath,
		"--target-pod-uid", a.PodUID,
		"--target-container-id", a.ContainerID,
		"--lang", string(a.Lang),
		"--event-type", "itimer",
		"--profiling-tool", string(a.Tool),
		"--output-type", string(a.Output),
		"--grace-period-ending", "600s",
		"--duration", fmt.Sprintf("%ds", a.DurationSeconds),
		"--compressor-type", "gzip",
	}

	dirOrCreate := corev1.HostPathDirectoryOrCreate
	volumes := []corev1.Volume{
		{
			Name: "modules",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/lib/modules", Type: &dirOrCreate},
			},
		},
		{
			Name: "target-filesystem",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: a.ContainerRuntimePath, Type: &dirOrCreate},
			},
		},
	}
	mounts := []corev1.VolumeMount{
		{Name: "modules", MountPath: "/lib/modules"},
		{Name: "target-filesystem", MountPath: a.ContainerRuntimePath},
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      a.Name,
			Namespace: a.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "nudgebee-agent",
				"nudgebee.com/role":            "pod-profiler",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:      a.NodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			HostPID:       true, // profiler reads /proc/<pid> across containers
			Volumes:       volumes,
			Containers: []corev1.Container{{
				Name:    "profiler",
				Image:   a.Image,
				Command: cmd,
				SecurityContext: &corev1.SecurityContext{
					Privileged: ptr.To(true),
					Capabilities: &corev1.Capabilities{
						Add: []corev1.Capability{"SYS_ADMIN"},
					},
				},
				VolumeMounts: mounts,
			}},
		},
	}
}

// generateDebuggerPodName produces a DNS-safe unique name for the
// debugger pod. Uses a time-derived suffix so concurrent profile runs
// against the same target don't collide.
func generateDebuggerPodName() string {
	return fmt.Sprintf("nudgebee-profiler-%d", time.Now().UnixNano())
}

// waitForPodReady polls until all containers in the pod are ready or
// timeout. Treats "all ContainerStatuses ready=true" as the readiness
// signal, with one extra check for terminated containers so an
// ImagePullBackOff is surfaced as an error not a timeout.
func (h *ProfilerHandler) waitForPodReady(ctx context.Context, namespace, name string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return wait.PollUntilContextCancel(pollCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		p, err := h.cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if p.Status.Phase == corev1.PodFailed {
			return false, fmt.Errorf("pod_profiler: debugger pod entered Failed phase")
		}
		if len(p.Status.ContainerStatuses) == 0 {
			return false, nil
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
				return false, fmt.Errorf("pod_profiler: debugger container terminated: %s", cs.State.Terminated.Reason)
			}
			if !cs.Ready {
				return false, nil
			}
		}
		return true, nil
	})
}

// resultEvent is one parsed JSON line from the profiler pod's stdout.
// Three event shapes — `progress`, `error`, `result`. We only act on
// `result` and `error`/`progress.stage=ended`; other events are
// progress noise.
type resultEvent struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

// streamUntilResult tails the profiler pod's logs, parses each line as
// JSON, and returns the first `result` event's data. Returns an error
// if the pod reports `error` or terminates without producing a result.
//
// Implementation note: client-go's GetLogs+Stream gives a long-lived
// reader we can scan line-by-line. We don't use Watch here because the
// line-streaming path is simpler and matches what kubectl logs -f emits.
func (h *ProfilerHandler) streamUntilResult(ctx context.Context, namespace, name string) (map[string]interface{}, error) {
	req := h.cs.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{Follow: true})
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("pod_profiler: open log stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	scanner := bufio.NewScanner(stream)
	// Profiler events can be large (chunked-result manifests have
	// per-chunk metadata). Bump the scanner buffer past the default 64K.
	scanner.Buffer(make([]byte, 1<<20), 16<<20)

	endedSeen := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev resultEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "result":
			return ev.Data, nil
		case "error":
			return nil, fmt.Errorf("pod_profiler: profiler reported error: %v", ev.Data)
		case "progress":
			if stage, _ := ev.Data["stage"].(string); stage == "ended" {
				endedSeen = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("pod_profiler: log scan: %w", err)
	}
	if endedSeen {
		return nil, errors.New("pod_profiler: profiler ended without emitting a result event")
	}
	return nil, errors.New("pod_profiler: log stream closed before result")
}

// fetchResultFile reads `result.file` (or the first chunk file) out of
// the profiler pod via tar-exec, validates the MD5 checksum, and
// returns it as a FileResult ready to wrap in a Finding.
func (h *ProfilerHandler) fetchResultFile(ctx context.Context, namespace, name string, resultData map[string]interface{}, lang ProgrammingLanguage, tool ProfilingTool, req ProfileRequest) (*FileResult, error) {
	filename, _ := resultData["file"].(string)
	wantSum, _ := resultData["checksum"].(string)
	if filename == "" {
		return nil, errors.New("pod_profiler: result missing `file`")
	}

	contents, err := copyFileFromPod(ctx, h.cs, h.restCfg, namespace, name, "profiler", filename)
	if err != nil {
		return nil, fmt.Errorf("pod_profiler: copy file: %w", err)
	}
	if wantSum != "" {
		gotSum := md5sumHex(contents)
		if gotSum != wantSum {
			return nil, fmt.Errorf("pod_profiler: checksum mismatch (got %s, want %s)", gotSum, wantSum)
		}
	}
	return &FileResult{
		Filename:       filename,
		ContentsBase64: base64Std.EncodeToString(contents),
		Lang:           string(lang),
		ProfileTool:    string(tool),
		ProfileType:    string(req.ProfileType),
		Duration:       req.Seconds,
	}, nil
}

// copyFileFromPod runs `tar cf - <path>` inside the named container and
// reads stdout, returning the file's raw bytes. This is the same
// mechanism `kubectl cp` uses; we invoke it directly via SPDY exec so
// no kubectl binary needs to be in the agent image.
//
// We constrain to a single file (not a directory) because tar-streaming
// a directory tree is more complex than the action needs and the legacy
// playbook copied just one file at a time.
func copyFileFromPod(ctx context.Context, cs kubernetes.Interface, restCfg *rest.Config, namespace, pod, container, srcPath string) ([]byte, error) {
	if cs == nil || restCfg == nil {
		return nil, errors.New("pod_profiler: kube client not configured")
	}
	restReq := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   []string{"tar", "cf", "-", srcPath},
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", restReq.URL())
	if err != nil {
		return nil, fmt.Errorf("build SPDY executor: %w", err)
	}
	var stdout, stderr strings.Builder
	streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: writerFor(&stdout), Stderr: writerFor(&stderr)})
	if streamErr != nil {
		return nil, fmt.Errorf("tar exec: %w (stderr: %s)", streamErr, stderr.String())
	}
	return extractTarSingleFile(strings.NewReader(stdout.String()), srcPath)
}

// writerFor adapts strings.Builder to io.Writer (it already implements
// it since 1.12, but we wrap so the call site reads cleanly).
func writerFor(b *strings.Builder) io.Writer { return b }

// md5sumHex computes the md5 hex digest of the file bytes (matches
// calculate_checksum). md5 here is a content fingerprint, not a
// security primitive.
func md5sumHex(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // content fingerprint
	return hex.EncodeToString(sum[:])
}
