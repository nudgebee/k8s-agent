package podexec

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

// These tests drive Profile end to end against the fake clientset: the
// debugger pod is created, its status and log are served by reactors, and
// only the SPDY file copy is stubbed (copyFile).

const testPprof401 = `failed to fetch CPU profile for PID 7: failed to nsenter+wget ` +
	`"http://127.0.0.1:6060/debug/pprof/profile?seconds=10" error wget: server returned error: ` +
	`HTTP/1.1 401 Unauthorized` + "\n" + `: exit status 1`

// profileTarget is a scheduled, running target pod and its node.
func profileTarget() []runtime.Object {
	return []runtime.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "cart-0", Namespace: "shop", UID: "pod-uid"},
			Spec:       corev1.PodSpec{NodeName: "node-a"},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{Name: "app", ContainerID: "containerd://abc"}},
			},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
			Status: corev1.NodeStatus{
				NodeInfo: corev1.NodeSystemInfo{ContainerRuntimeVersion: "containerd://1.7.13"},
			},
		},
	}
}

// debuggerSim plays the kubelet and the profiler for the debugger pods a run
// creates: each one gets status(tool) as its status and serves logs(tool) as
// its log, tool being the --profiling-tool it was started with. Runs are
// sequential, so the most recently created pod is the one being read.
type debuggerSim struct {
	status  func(tool string) corev1.PodStatus
	logs    func(tool string) string
	tail    string // served for the tail read of a failed pod
	created []*corev1.Pod
	tool    string
}

func runningReady() corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  debuggerContainer,
			Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
}

func (s *debuggerSim) install(cs *fake.Clientset) {
	cs.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		s.tool = flagMap(pod.Spec.Containers[0].Command)["--profiling-tool"]
		if s.status != nil {
			pod.Status = s.status(s.tool)
		} else {
			pod.Status = runningReady()
		}
		s.created = append(s.created, pod.DeepCopy())
		return false, nil, nil // let the tracker store it
	})
	cs.PrependReactor("get", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "log" {
			return false, nil, nil
		}
		opts, _ := a.(k8stesting.GenericAction).GetValue().(*corev1.PodLogOptions)
		if opts != nil && !opts.Follow {
			return true, &runtime.Unknown{Raw: []byte(s.tail)}, nil
		}
		body := ""
		if s.logs != nil {
			body = s.logs(s.tool)
		}
		return true, &runtime.Unknown{Raw: []byte(body)}, nil
	})
}

func resultLine(file string) string {
	return `{"type":"progress","data":{"stage":"started"}}` + "\n" +
		`{"type":"result","data":{"file":"` + file + `","compressor-type":"gzip"}}` + "\n"
}

func errorLine(reason string) string {
	b, _ := json.Marshal(map[string]any{"type": "error", "data": map[string]string{"reason": reason}})
	return `{"type":"progress","data":{"stage":"started"}}` + "\n" + string(b) + "\n"
}

// newRunHandler wires a handler whose file copy returns contents for any
// path, with production's image-variant choice under a fixed template.
func newRunHandler(t *testing.T, cs *fake.Clientset, contents string) *ProfilerHandler {
	t.Helper()
	t.Setenv("PROFILER_IMAGE", "example.com/profiler-{}:latest")
	h := NewProfilerHandler(cs, fakeRestConfig)
	h.copyFile = func(context.Context, string, string, string, string) ([]byte, error) {
		return []byte(contents), nil
	}
	return h
}

func debuggerPodsLeft(t *testing.T, cs *fake.Clientset) int {
	t.Helper()
	list, err := cs.CoreV1().Pods("shop").List(context.Background(), metav1.ListOptions{
		LabelSelector: "nudgebee.com/role=pod-profiler",
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(list.Items)
}

// ---------- profile_type decides the tool ----------

// TestProfile_MemoryRequestRunsHeapProfiler is the profiler screen's Go
// "Memory" request: profile_type=memory with output_type=flamegraph. The
// debugger must be asked for a heap dump, not left to turn the flamegraph
// into a CPU profile.
func TestProfile_MemoryRequestRunsHeapProfiler(t *testing.T) {
	cs := fake.NewClientset(profileTarget()...)
	sim := &debuggerSim{logs: func(string) string { return resultLine("/tmp/agent-heapdump-7-1.pprof.gz") }}
	sim.install(cs)
	h := newRunHandler(t, cs, "heap")

	res, err := h.Profile(context.Background(), ProfileRequest{
		Name: "cart-0", Namespace: "shop", Seconds: 10, Lang: LangGo,
		ProfileType: ProfileMemory, OutputType: OutputFlameGraph,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sim.created) != 1 {
		t.Fatalf("debugger pods created = %d; want 1", len(sim.created))
	}
	args := flagMap(sim.created[0].Spec.Containers[0].Command)
	if args["--profiling-tool"] != "pprof" || args["--output-type"] != "heapdump" {
		t.Errorf("debugger asked for (%s, %s); want (pprof, heapdump)", args["--profiling-tool"], args["--output-type"])
	}
	if res.ProfileTool != "pprof" || res.ProfileType != "memory" || res.FallbackFrom != "" {
		t.Errorf("result = %+v", res)
	}
	if debuggerPodsLeft(t, cs) != 0 {
		t.Error("debugger pod was not cleaned up")
	}
}

// ---------- debugger pod failure cause ----------

// TestProfile_DebuggerFailureCarriesCause — a debugger pod that dies before
// it is ready used to end the run with a bare "entered Failed phase". The
// pod's own account of why (status reason, container exit, log tail) must
// reach the caller, and must be read before cleanup deletes the pod.
func TestProfile_DebuggerFailureCarriesCause(t *testing.T) {
	cs := fake.NewClientset(profileTarget()...)
	sim := &debuggerSim{
		status: func(string) corev1.PodStatus {
			return corev1.PodStatus{
				Phase:   corev1.PodFailed,
				Reason:  "Evicted",
				Message: "The node was low on resource: memory.",
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  debuggerContainer,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
				}},
			}
		},
		tail: `{"type":"progress","data":{"stage":"started"}}` + "\npanic: runtime error: invalid memory address\n",
	}
	sim.install(cs)
	h := newRunHandler(t, cs, "")

	_, err := h.Profile(context.Background(), ProfileRequest{
		Name: "cart-0", Namespace: "shop", Seconds: 10, Lang: LangPython, ProfileType: ProfileCPU,
	})
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{
		"debugger pod failed",
		"phase Failed (Evicted: The node was low on resource: memory.)",
		"container profiler exited: OOMKilled, exit code 137",
		"progress: started",
		"panic: runtime error: invalid memory address",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q; want it to contain %q", err, want)
		}
	}

	logIdx, deleteIdx := -1, -1
	for i, a := range cs.Actions() {
		switch {
		case a.GetVerb() == "get" && a.GetSubresource() == "log":
			logIdx = i
		case a.GetVerb() == "delete" && a.GetResource().Resource == "pods":
			deleteIdx = i
		}
	}
	if logIdx < 0 || deleteIdx < 0 || logIdx > deleteIdx {
		t.Errorf("log read at action %d, delete at %d; the log must be read before the pod is deleted", logIdx, deleteIdx)
	}
}

func waitTestPod(status corev1.PodStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "dbg", Namespace: "shop"},
		Status:     status,
	}
}

func waitingStatus(reason, message string) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  debuggerContainer,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
		}},
	}
}

func TestWaitForPodReady(t *testing.T) {
	cases := []struct {
		name      string
		status    corev1.PodStatus
		pullGrace time.Duration
		timeout   time.Duration
		want      []string // substrings; empty = success
	}{
		{name: "ready", status: runningReady(), timeout: time.Second},
		// An exit 0 before the first poll is not a failure: the profiler's
		// events are in the log, which the stream reads to the end.
		{name: "succeeded", status: corev1.PodStatus{Phase: corev1.PodSucceeded}, timeout: time.Second},
		{name: "image pull failure past grace",
			status:    waitingStatus("ImagePullBackOff", `Back-off pulling image "example.com/profiler-bpf:missing"`),
			pullGrace: 0, timeout: time.Minute,
			want: []string{"debugger pod cannot start", "container profiler waiting: ImagePullBackOff", "example.com/profiler-bpf:missing"}},
		{name: "image pull failure within grace waits, then says why it timed out",
			status:    waitingStatus("ErrImagePull", "rpc error: not found"),
			pullGrace: time.Hour, timeout: 50 * time.Millisecond,
			want: []string{"debugger pod not ready after", "ErrImagePull: rpc error: not found"}},
		{name: "unstartable fails without grace",
			status:    waitingStatus("InvalidImageName", `couldn't parse image name "bad::name"`),
			pullGrace: time.Hour, timeout: time.Minute,
			want: []string{"debugger pod cannot start", "InvalidImageName"}},
		{name: "container exit while pod still running",
			status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: debuggerContainer,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Reason: "StartError", ExitCode: 128, Message: "exec: \"/app/agent\": no such file or directory",
					}},
				}},
			},
			timeout: time.Minute,
			want:    []string{"debugger pod failed", "StartError, exit code 128", "/app/agent", "fake logs"}},
		{name: "stuck pending times out with state",
			status:  waitingStatus("ContainerCreating", ""),
			timeout: 50 * time.Millisecond,
			want:    []string{"debugger pod not ready after", "phase Pending", "waiting: ContainerCreating"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset(waitTestPod(tc.status))
			h := NewProfilerHandler(cs, fakeRestConfig)
			h.imagePullGrace = tc.pullGrace
			start := time.Now()
			err := h.waitForPodReady(context.Background(), "shop", "dbg", tc.timeout)
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("err = %v; want ready", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want an error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("err = %q; want it to contain %q", err, w)
				}
			}
			if tc.timeout >= time.Minute && time.Since(start) > 10*time.Second {
				t.Errorf("took %s; a terminal state must end the wait at once", time.Since(start))
			}
		})
	}
}

// TestStreamUntilResult_ClosedStreamCarriesPodState — the debugger died
// mid-profile: the log stream just stops, and only the pod status says the
// container was OOM-killed.
func TestStreamUntilResult_ClosedStreamCarriesPodState(t *testing.T) {
	cs := fake.NewClientset(waitTestPod(corev1.PodStatus{
		Phase: corev1.PodFailed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  debuggerContainer,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
		}},
	}))
	sim := &debuggerSim{logs: func(string) string { return `{"type":"progress","data":{"stage":"started"}}` + "\n" }}
	sim.install(cs)
	h := NewProfilerHandler(cs, fakeRestConfig)

	_, err := h.streamUntilResult(context.Background(), "shop", "dbg")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"log stream closed before result", "OOMKilled, exit code 137", "progress: started"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q; want it to contain %q", err, want)
		}
	}
}

// TestStreamUntilResult_EndedWithoutResultKeepsLog — the profiler said it
// was done but published nothing; its log is the only clue.
func TestStreamUntilResult_EndedWithoutResultKeepsLog(t *testing.T) {
	cs := fake.NewClientset(waitTestPod(runningReady()))
	sim := &debuggerSim{logs: func(string) string {
		return "flamegraph.pl: command not found\n" +
			`{"type":"progress","data":{"stage":"ended"}}` + "\n"
	}}
	sim.install(cs)
	h := NewProfilerHandler(cs, fakeRestConfig)

	_, err := h.streamUntilResult(context.Background(), "shop", "dbg")
	if err == nil || !strings.Contains(err.Error(), "ended without emitting a result event") ||
		!strings.Contains(err.Error(), "flamegraph.pl: command not found") {
		t.Errorf("err = %v", err)
	}
}

// ---------- target pod gone ----------

func replicaSetFor(name, deployment string, labels map[string]string) *appsv1.ReplicaSet {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
		Spec:       appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}
	if deployment != "" {
		rs.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: deployment, UID: "dep-uid", Controller: ptr.To(true),
		}}
	}
	return rs
}

func podWith(name string, labels map[string]string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: labels},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func TestProfile_TargetGone(t *testing.T) {
	appLabels := map[string]string{"app": "web"}
	oldRS := map[string]string{"app": "web", "pod-template-hash": "6d8f7c9b4"}
	newRS := map[string]string{"app": "web", "pod-template-hash": "58c9d7f6b"}
	terminating := podWith("web-6d8f7c9b4-zzzzz", oldRS, corev1.PodRunning)
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	terminating.Finalizers = []string{"example.com/hold"}

	cases := []struct {
		name    string
		pod     string
		objects []runtime.Object
		forbid  bool
		want    string
	}{
		{
			// The usual stale name: a rollout scaled the old ReplicaSet to
			// zero, the Deployment's pods now come from the new one.
			name: "rolled deployment lists the replacements",
			pod:  "web-6d8f7c9b4-x2v4k",
			objects: []runtime.Object{
				replicaSetFor("web-6d8f7c9b4", "web", oldRS),
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
					Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: appLabels}},
				},
				podWith("web-58c9d7f6b-p9q8z", newRS, corev1.PodPending),
				podWith("web-58c9d7f6b-b7n4m", newRS, corev1.PodRunning),
				terminating,
				podWith("other-0", map[string]string{"app": "other"}, corev1.PodRunning),
			},
			want: "pod_profiler: pod shop/web-6d8f7c9b4-x2v4k no longer exists; deployment web currently runs " +
				"web-58c9d7f6b-b7n4m (Running), web-58c9d7f6b-p9q8z (Pending)",
		},
		{
			name: "bare replicaset lists its own pods",
			pod:  "web-6d8f7c9b4-x2v4k",
			objects: []runtime.Object{
				replicaSetFor("web-6d8f7c9b4", "", oldRS),
				podWith("web-6d8f7c9b4-k5l6m", oldRS, corev1.PodRunning),
			},
			want: "pod_profiler: pod shop/web-6d8f7c9b4-x2v4k no longer exists; replicaset web-6d8f7c9b4 currently runs " +
				"web-6d8f7c9b4-k5l6m (Running)",
		},
		{
			name:    "replicaset with no pods says so",
			pod:     "web-6d8f7c9b4-x2v4k",
			objects: []runtime.Object{replicaSetFor("web-6d8f7c9b4", "", oldRS)},
			want:    "pod_profiler: pod shop/web-6d8f7c9b4-x2v4k no longer exists; replicaset web-6d8f7c9b4 currently has no pods",
		},
		{
			name: "replicaset gone too",
			pod:  "web-6d8f7c9b4-x2v4k",
			want: "pod_profiler: pod shop/web-6d8f7c9b4-x2v4k no longer exists",
		},
		{
			name: "statefulset-style name gets no lookup",
			pod:  "db-0",
			want: "pod_profiler: pod shop/db-0 no longer exists",
		},
		{
			name:    "rbac denial degrades to the plain message",
			pod:     "web-6d8f7c9b4-x2v4k",
			objects: []runtime.Object{replicaSetFor("web-6d8f7c9b4", "", oldRS)},
			forbid:  true,
			want:    "pod_profiler: pod shop/web-6d8f7c9b4-x2v4k no longer exists",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset(tc.objects...)
			if tc.forbid {
				cs.PrependReactor("get", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "replicasets"}, "web-6d8f7c9b4", nil)
				})
			}
			h := NewProfilerHandler(cs, fakeRestConfig)
			_, err := h.Profile(context.Background(), ProfileRequest{Name: tc.pod, Namespace: "shop", Lang: LangGo})
			if err == nil || err.Error() != tc.want {
				t.Errorf("err = %v\nwant  %s", err, tc.want)
			}
		})
	}
}

// TestLivePodsHint_CapsList — a large Deployment lists a handful of pods,
// not all of them.
func TestLivePodsHint_CapsList(t *testing.T) {
	labels := map[string]string{"app": "web"}
	objects := []runtime.Object{replicaSetFor("web-6d8f7c9b4", "", labels)}
	for _, n := range []string{"bbbbb", "ccccc", "ddddd", "fffff", "ggggg", "hhhhh", "jjjjj"} {
		objects = append(objects, podWith("web-6d8f7c9b4-"+n, labels, corev1.PodRunning))
	}
	h := NewProfilerHandler(fake.NewClientset(objects...), fakeRestConfig)
	hint := h.livePodsHint(context.Background(), "shop", "web-6d8f7c9b4-x2v4k")
	if strings.Count(hint, "(Running)") != maxLivePods || !strings.HasSuffix(hint, " and 2 more") {
		t.Errorf("hint = %q", hint)
	}
}

// ---------- Go pprof endpoint fallback ----------

func TestClassifyPprofFailure(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		ok     bool
		want   string
	}{
		{name: "401", reason: testPprof401, ok: true,
			want: "the pprof endpoint on :6060 requires authentication (HTTP 401)"},
		{name: "403", reason: `failed to nsenter+wget "http://127.0.0.1:8080/debug/pprof/heap?gc=1" error wget: server returned error: HTTP/1.1 403 Forbidden`,
			ok: true, want: "the pprof endpoint on :8080 refused the request (HTTP 403)"},
		{name: "404", reason: `failed to nsenter+wget "http://127.0.0.1:8080/debug/pprof/profile?seconds=30" error wget: server returned error: HTTP/1.1 404 Not Found`,
			ok: true, want: "the target serves no /debug/pprof on :8080 (HTTP 404)"},
		{name: "refused", reason: `failed to nsenter+wget "http://127.0.0.1:8080/debug/pprof/profile?seconds=30" error wget: can't connect to remote host (127.0.0.1): Connection refused`,
			ok: true, want: "nothing in the target accepted a connection to its pprof endpoint on :8080"},
		// A gRPC (HTTP/2-only) listener drops an HTTP/1.1 request.
		{name: "reset", reason: `failed to fetch CPU profile for PID 7: failed to nsenter+wget "http://127.0.0.1:8080/debug/pprof/profile?seconds=15" error wget: error getting response: Connection reset by peer`,
			ok: true, want: "the target reset the connection to its pprof endpoint on :8080 without answering — that port does not serve /debug/pprof over HTTP"},
		{name: "no port", reason: "failed to find listening port: no listening port found for PID",
			ok: true, want: "nothing in the target accepted a connection to its pprof endpoint"},
		{name: "unrelated", reason: "no PIDs found for container ID: abc"},
		{name: "500 is not an endpoint problem", reason: `failed to nsenter+wget "http://127.0.0.1:8080/debug/pprof/profile" error wget: server returned error: HTTP/1.1 500 Internal Server Error`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := classifyPprofFailure(tc.reason)
			if ok != tc.ok {
				t.Fatalf("ok = %v; want %v", ok, tc.ok)
			}
			if ok && f.String() != tc.want {
				t.Errorf("String() = %q; want %q", f.String(), tc.want)
			}
		})
	}
}

func goPprofLogs(bpfLog string) func(string) string {
	return func(tool string) string {
		if tool == string(ToolPprof) {
			return errorLine(testPprof401)
		}
		return bpfLog
	}
}

// TestProfile_GoCPUFallsBackToBpf — a Go CPU profile whose pprof endpoint
// sits behind auth is retaken with eBPF sampling, and the result says so.
func TestProfile_GoCPUFallsBackToBpf(t *testing.T) {
	cs := fake.NewClientset(profileTarget()...)
	sim := &debuggerSim{logs: goPprofLogs(resultLine("/tmp/agent-flamegraph-7-1.svg.gz"))}
	sim.install(cs)
	h := newRunHandler(t, cs, "<svg/>")

	res, err := h.Profile(context.Background(), ProfileRequest{
		Name: "cart-0", Namespace: "shop", Seconds: 10, Lang: LangGo,
		ProfileType: ProfileCPU, OutputType: OutputFlameGraph,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sim.created) != 2 {
		t.Fatalf("debugger pods created = %d; want 2", len(sim.created))
	}
	first := flagMap(sim.created[0].Spec.Containers[0].Command)
	second := flagMap(sim.created[1].Spec.Containers[0].Command)
	if first["--profiling-tool"] != "pprof" || first["--output-type"] != "pprof" {
		t.Errorf("first run = (%s, %s); want (pprof, pprof)", first["--profiling-tool"], first["--output-type"])
	}
	if second["--profiling-tool"] != "bpf" || second["--output-type"] != "flamegraph" {
		t.Errorf("fallback run = (%s, %s); want (bpf, flamegraph)", second["--profiling-tool"], second["--output-type"])
	}
	if img := sim.created[1].Spec.Containers[0].Image; img != "example.com/profiler-bpf:latest" {
		t.Errorf("fallback image = %q; want the bpf variant", img)
	}
	if res.ProfileTool != "bpf" || res.FallbackFrom != "pprof" ||
		res.FallbackReason != "the pprof endpoint on :6060 requires authentication (HTTP 401)" {
		t.Errorf("result = %+v", res)
	}
	if res.Filename != "/tmp/agent-flamegraph-7-1.svg.gz" || res.ContentsBase64 != base64Std.EncodeToString([]byte("<svg/>")) {
		t.Errorf("file = %s / %s", res.Filename, res.ContentsBase64)
	}
	if debuggerPodsLeft(t, cs) != 0 {
		t.Error("debugger pods were not cleaned up")
	}
}

// TestProfile_GoPprofEndpointErrors covers the endpoint failures that are
// not retried: a heap profile (no eBPF equivalent), a tool the caller named,
// and a fallback that fails itself. Each must read as an endpoint problem,
// not as the raw wget text.
func TestProfile_GoPprofEndpointErrors(t *testing.T) {
	cases := []struct {
		name      string
		req       ProfileRequest
		bpfLog    string
		wantRuns  int
		wantErr   string
		wantNotIn string
	}{
		{
			name:     "heap profile",
			req:      ProfileRequest{ProfileType: ProfileMemory, OutputType: OutputFlameGraph},
			wantRuns: 1,
			wantErr: "pod_profiler: the pprof endpoint on :6060 requires authentication (HTTP 401); " +
				"a Go heap profile can only be read from that endpoint",
			wantNotIn: "wget",
		},
		{
			name:     "caller named pprof",
			req:      ProfileRequest{ProfileType: ProfileCPU, ProfileTool: ToolPprof, OutputType: OutputPprof},
			wantRuns: 1,
			wantErr:  "pod_profiler: the pprof endpoint on :6060 requires authentication (HTTP 401)",
		},
		{
			name:     "fallback fails too",
			req:      ProfileRequest{ProfileType: ProfileCPU},
			bpfLog:   errorLine("could not launch profiler: perf_event_open failed"),
			wantRuns: 2,
			wantErr: "pod_profiler: the pprof endpoint on :6060 requires authentication (HTTP 401), " +
				"and the eBPF fallback failed too: profiler reported error: could not launch profiler: perf_event_open failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset(profileTarget()...)
			sim := &debuggerSim{logs: goPprofLogs(tc.bpfLog)}
			sim.install(cs)
			h := newRunHandler(t, cs, "")
			req := tc.req
			req.Name, req.Namespace, req.Seconds, req.Lang = "cart-0", "shop", 10, LangGo

			_, err := h.Profile(context.Background(), req)
			if err == nil || !strings.HasPrefix(err.Error(), tc.wantErr) {
				t.Errorf("err = %v\nwant prefix %s", err, tc.wantErr)
			}
			if tc.wantNotIn != "" && err != nil && strings.Contains(err.Error(), tc.wantNotIn) {
				t.Errorf("err = %q; should not quote %q", err, tc.wantNotIn)
			}
			if len(sim.created) != tc.wantRuns {
				t.Errorf("debugger pods created = %d; want %d", len(sim.created), tc.wantRuns)
			}
		})
	}
}

// TestProfile_GoPprofOtherErrorNotRetried — only an endpoint failure
// triggers the fallback; any other profiler error is returned as is.
func TestProfile_GoPprofOtherErrorNotRetried(t *testing.T) {
	cs := fake.NewClientset(profileTarget()...)
	sim := &debuggerSim{logs: func(string) string { return errorLine("no PIDs found for container ID: abc") }}
	sim.install(cs)
	h := newRunHandler(t, cs, "")

	_, err := h.Profile(context.Background(), ProfileRequest{
		Name: "cart-0", Namespace: "shop", Seconds: 10, Lang: LangGo, ProfileType: ProfileCPU,
	})
	if err == nil || err.Error() != "pod_profiler: profiler reported error: no PIDs found for container ID: abc" {
		t.Errorf("err = %v", err)
	}
	if len(sim.created) != 1 {
		t.Errorf("debugger pods created = %d; want 1", len(sim.created))
	}
}

// TestFileResult_FallbackFieldsOptional — the fallback fields are omitted
// on an ordinary run, so existing readers see the shape they always did.
func TestFileResult_FallbackFieldsOptional(t *testing.T) {
	plain, _ := json.Marshal(FileResult{Filename: "f", ProfileTool: "pprof"})
	if strings.Contains(string(plain), "fallback") {
		t.Errorf("plain result = %s; want no fallback fields", plain)
	}
	fell, _ := json.Marshal(FileResult{Filename: "f", ProfileTool: "bpf", FallbackFrom: "pprof", FallbackReason: "r"})
	if !strings.Contains(string(fell), `"fallback_from":"pprof"`) || !strings.Contains(string(fell), `"fallback_reason":"r"`) {
		t.Errorf("fallback result = %s", fell)
	}
}
