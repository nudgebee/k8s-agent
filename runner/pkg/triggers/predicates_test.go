package triggers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// asObj is a tiny helper so test fixtures can be inline JSON strings
// rather than verbose map[string]any literals. The tests are
// fixture-heavy (one per condition); JSON keeps them readable + close
// to what kubewatch actually sends.
func asObj(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("test fixture parse: %v\n%s", err, s)
	}
	return m
}

// ---------- pod_crash_loop ----------

func TestPodCrashLoop_FiresOnRestartCountIncrease(t *testing.T) {
	// Transition: oldObj had restartCount=4, new obj has 5 → fire.
	oldPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":4,
			 "state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	newPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":5,
			 "state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off"}}}
		]}
	}`)
	m := podCrashLoopMatcher()
	if !m.Predicate(newPod, oldPod) {
		t.Fatal("predicate should fire when restartCount goes up while in CrashLoopBackOff")
	}
	if m.FingerprintFn(newPod) == "" {
		t.Error("fingerprint must be non-empty")
	}
}

func TestPodCrashLoop_FiresOnResyncAndCreate(t *testing.T) {
	// kubewatch's UpdateFunc shares one closure-scoped Event variable
	// (kubewatch/pkg/controller/controller.go:787) and the K8s informer
	// passes old/new as references that often share underlying memory, so
	// every kubewatch UPDATE has obj == oldObj on the wire. The predicate
	// must fire on the bare Pod state regardless; the engine's rate-limit
	// + restartCount-bucket fingerprint dedupes resync re-emits.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":5,
			 "state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	if !podCrashLoopMatcher().Predicate(pod, pod) {
		t.Error("predicate must fire on a CrashLooping Pod even when oldObj==obj (kubewatch's wire shape)")
	}
	if !podCrashLoopMatcher().Predicate(pod, nil) {
		t.Error("predicate must fire on a CrashLooping Pod even when oldObj==nil (CREATE / first observation)")
	}
}

func TestPodCrashLoop_DropsBelowRestartThreshold(t *testing.T) {
	// Pod just transitioned 0→1 restart. Below default minRestarts=2 →
	// suppressed even though restartCount went up.
	oldPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":0,
			 "state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	newPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":1,
			 "state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	if podCrashLoopMatcher().Predicate(newPod, oldPod) {
		t.Error("predicate should not fire below restart threshold (1 < 2)")
	}
}

func TestPodCrashLoop_DropsForOtherWaitingReasons(t *testing.T) {
	oldPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":4,
			 "state":{"waiting":{"reason":"ContainerCreating"}}}
		]}
	}`)
	newPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":5,
			 "state":{"waiting":{"reason":"ContainerCreating"}}}
		]}
	}`)
	if podCrashLoopMatcher().Predicate(newPod, oldPod) {
		t.Error("ContainerCreating must not fire crash_loop")
	}
}

func TestPodCrashLoop_FingerprintStableWithinHour(t *testing.T) {
	// A Pod stuck in CrashLoopBackOff produces ~1 Finding per hour. The
	// fingerprint pairs with a 1h rate limit and an hour-bucket, so
	// successive restarts in the same wall-clock hour collapse to one
	// fingerprint (and the rate limiter suppresses repeats).
	mk := func(rc int) map[string]any {
		return asObj(t, `{
			"metadata":{"name":"web-0","namespace":"prod"},
			"status":{"containerStatuses":[
				{"name":"app","restartCount":`+itoa(rc)+`,
				 "state":{"waiting":{"reason":"CrashLoopBackOff"}}}
			]}
		}`)
	}
	m := podCrashLoopMatcher()
	if m.FingerprintFn(mk(2)) != m.FingerprintFn(mk(7)) {
		t.Error("restartCount must not affect fingerprint within an hour")
	}
	if m.FingerprintFn(mk(2)) != m.FingerprintFn(mk(100)) {
		t.Error("restartCount must not affect fingerprint within an hour (rc=100 case)")
	}
}

// ---------- pod_crash_loop enrichment ----------

func TestCrashLoopEnrichBlocks_BuildsMarkdownPerCrashedContainer(t *testing.T) {
	// For each container with state.waiting + restartCount ≥ 1, emit
	// markdown blocks for restart count, waiting reason, and (when set)
	// state.terminated and lastState.terminated reasons. The trigger-
	// emitted Finding carries the same evidence the legacy playbook
	// produced (minus the previous-logs FileBlock, which needs a K8s
	// API call and is deferred to a relay-side enricher).
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":5,
			 "state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off"}},
			 "lastState":{"terminated":{"reason":"Error","exitCode":1}}}
		]}
	}`)
	blocks := crashLoopEnrichBlocks(pod, nil, EnrichContext{})
	want := []string{
		"*app* restart count: 5",
		"*app* waiting reason: CrashLoopBackOff",
		"*app* termination reason: Error",
	}
	if len(blocks) != len(want) {
		t.Fatalf("blocks = %d; want %d (got %#v)", len(blocks), len(want), blocks)
	}
	for i, b := range blocks {
		if b["type"] != "markdown" {
			t.Errorf("block[%d].type = %v; want markdown", i, b["type"])
		}
		if b["data"] != want[i] {
			t.Errorf("block[%d].data = %q; want %q", i, b["data"], want[i])
		}
	}
}

func TestCrashLoopEnrichBlocks_StateAndLastStateBothPresent(t *testing.T) {
	// Emit two separate "termination reason" markdowns when both
	// state.terminated and lastState.terminated are set — state first,
	// then lastState. The resulting Finding evidence reads chronologically.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":3,
			 "state":{
				"waiting":{"reason":"CrashLoopBackOff"},
				"terminated":{"reason":"Error","exitCode":1}
			 },
			 "lastState":{"terminated":{"reason":"OOMKilled","exitCode":137}}}
		]}
	}`)
	blocks := crashLoopEnrichBlocks(pod, nil, EnrichContext{})
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d; want 4 (count, waiting, state.term, lastState.term)", len(blocks))
	}
	if blocks[2]["data"] != "*app* termination reason: Error" {
		t.Errorf("block[2] = %q; want state.terminated reason first", blocks[2]["data"])
	}
	if blocks[3]["data"] != "*app* termination reason: OOMKilled" {
		t.Errorf("block[3] = %q; want lastState.terminated reason second", blocks[3]["data"])
	}
}

func TestCrashLoopEnrichBlocks_SkipsNonCrashedContainers(t *testing.T) {
	// Multi-container pod: one container crashing, one running, one
	// init-container completed. Only the crashing one produces blocks —
	// running and completed containers are noise for a crash report.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{
			"containerStatuses":[
				{"name":"app","restartCount":2,
				 "state":{"waiting":{"reason":"CrashLoopBackOff"}}},
				{"name":"sidecar","restartCount":0,
				 "state":{"running":{"startedAt":"2026-05-08T03:00:00Z"}}}
			],
			"initContainerStatuses":[
				{"name":"init-db","restartCount":0,
				 "state":{"terminated":{"reason":"Completed","exitCode":0}}}
			]
		}
	}`)
	blocks := crashLoopEnrichBlocks(pod, nil, EnrichContext{})
	for _, b := range blocks {
		data, _ := b["data"].(string)
		if !strings.Contains(data, "*app*") {
			t.Errorf("unexpected block for non-crashed container: %q", data)
		}
	}
	if len(blocks) != 2 {
		t.Errorf("blocks = %d; want 2 (count + waiting reason for app only)", len(blocks))
	}
}

func TestCrashLoopEnrichBlocks_RestartCountZeroSkipped(t *testing.T) {
	// restartCount >= 1 — a container in waiting with 0 restarts is
	// freshly starting up, not crashing. Skip it.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":0,
			 "state":{"waiting":{"reason":"ContainerCreating"}}}
		]}
	}`)
	if got := crashLoopEnrichBlocks(pod, nil, EnrichContext{}); got != nil {
		t.Errorf("crashLoopEnrichBlocks = %v; want nil for restartCount=0", got)
	}
}

func TestCrashLoopEnrichBlocks_NoWaitingState(t *testing.T) {
	// Container is running again (state.running set) — even if it has
	// restart history, there's nothing currently waiting to crash-report.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":5,
			 "state":{"running":{"startedAt":"2026-05-08T03:00:00Z"}}}
		]}
	}`)
	if got := crashLoopEnrichBlocks(pod, nil, EnrichContext{}); got != nil {
		t.Errorf("crashLoopEnrichBlocks = %v; want nil when no waiting state", got)
	}
}

// ---------- pod_oom_killed ----------

func TestPodOOMKilled_FiresWhenNewOOMObserved(t *testing.T) {
	// Container OOMed at a new finishedAt timestamp the previous obj
	// didn't carry → fresh OOM event → fire.
	oldPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":1,
			 "lastState":{"terminated":{"reason":"OOMKilled","exitCode":137,"finishedAt":"2026-05-07T13:00:00Z"}}}
		]}
	}`)
	newPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":2,
			 "state":{"running":{"startedAt":"2026-05-08T03:00:00Z"}},
			 "lastState":{"terminated":{"reason":"OOMKilled","exitCode":137,"finishedAt":"2026-05-08T02:59:30Z"}}}
		]}
	}`)
	if !podOOMKilledMatcher().Predicate(newPod, oldPod) {
		t.Error("new OOM (different finishedAt) must fire")
	}
}

func TestPodOOMKilled_FiresOnResyncAndCreate(t *testing.T) {
	// kubewatch's wire shape has obj==oldObj; resync de-dup is the rate
	// limiter's job, not the predicate's.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":1,
			 "lastState":{"terminated":{"reason":"OOMKilled","exitCode":137,"finishedAt":"2026-05-07T13:00:00Z"}}}
		]}
	}`)
	if !podOOMKilledMatcher().Predicate(pod, pod) {
		t.Error("predicate must fire when obj==oldObj on a Pod with an OOMKilled lastState")
	}
	if !podOOMKilledMatcher().Predicate(pod, nil) {
		t.Error("predicate must fire on first observation (oldObj=nil)")
	}
}

func TestPodOOMKilled_DropsForOtherTerminationReasons(t *testing.T) {
	oldPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":0,
			 "lastState":{"terminated":{"reason":"Completed","exitCode":0,"finishedAt":"2026-05-07T13:00:00Z"}}}
		]}
	}`)
	newPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":1,
			 "lastState":{"terminated":{"reason":"Completed","exitCode":0,"finishedAt":"2026-05-08T13:00:00Z"}}}
		]}
	}`)
	if podOOMKilledMatcher().Predicate(newPod, oldPod) {
		t.Error("Completed exit must not fire OOM matcher")
	}
}

// ---------- pod_oom_killed enrichment ----------

func TestOOMKilledEnrichBlocks_BuildsTableFromPodSpec(t *testing.T) {
	// The trigger-emitted Finding must carry a TableBlock with pod, ns,
	// node, container name, container memory request/limit, and
	// terminated.startedAt/finishedAt — every field extractable without
	// a Node API call.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"spec":{
			"nodeName":"ip-10-0-0-1",
			"containers":[
				{"name":"app","resources":{"requests":{"memory":"128Mi"},"limits":{"memory":"256Mi"}}}
			]
		},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":2,
			 "lastState":{"terminated":{
				"reason":"OOMKilled","exitCode":137,
				"startedAt":"2026-05-08T02:30:00Z",
				"finishedAt":"2026-05-08T02:59:30Z"
			 }}}
		]}
	}`)
	blocks := oomKilledEnrichBlocks(pod, nil, EnrichContext{})
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d; want 1", len(blocks))
	}
	b := blocks[0]
	if b["type"] != "table" {
		t.Fatalf("type = %v; want table", b["type"])
	}
	data, _ := b["data"].(map[string]any)
	if data["table_name"] != "*Pod and Node OOMKilled data*" {
		t.Errorf("table_name = %v", data["table_name"])
	}
	headers, _ := data["headers"].([]any)
	if len(headers) != 2 || headers[0] != "field" || headers[1] != "value" {
		t.Errorf("headers = %v; want [field value]", headers)
	}
	want := map[string]string{
		"Pod":                   "web-0",
		"Namespace":             "prod",
		"Node Name":             "ip-10-0-0-1",
		"Container name":        "app",
		"Container memory":      "128MB request, 256MB limit",
		"Container started at":  "2026-05-08T02:30:00Z",
		"Container finished at": "2026-05-08T02:59:30Z",
	}
	got := map[string]string{}
	rows, _ := data["rows"].([]any)
	for _, r := range rows {
		row, _ := r.([]any)
		if len(row) != 2 {
			t.Fatalf("row shape = %v; want [field, value]", row)
		}
		k, _ := row[0].(string)
		v, _ := row[1].(string)
		got[k] = v
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("row %q = %q; want %q", k, got[k], v)
		}
	}
}

func TestOOMKilledEnrichBlocks_EmptyWhenNoOOMContainer(t *testing.T) {
	// Pod restarted from a non-OOM termination (Completed) → nothing for
	// the enricher to surface. Returning nil keeps the matcher cheap when
	// it fires for a different reason later (it shouldn't, but defensive).
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","lastState":{"terminated":{"reason":"Completed","exitCode":0}}}
		]}
	}`)
	if got := oomKilledEnrichBlocks(pod, nil, EnrichContext{}); got != nil {
		t.Errorf("oomKilledEnrichBlocks = %v; want nil for non-OOM termination", got)
	}
}

func TestOOMKilledEnrichBlocks_PrefersStateTerminatedOverLastState(t *testing.T) {
	// state.terminated is the **current** OOM (container hasn't restarted
	// yet); lastState.terminated is a stale OOM that the container
	// recovered from. pod_most_recent_oom_killed_container picks state
	// first — the table reflects the live OOM.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app",
			 "state":{"terminated":{"reason":"OOMKilled","finishedAt":"2026-05-08T03:00:00Z"}},
			 "lastState":{"terminated":{"reason":"OOMKilled","finishedAt":"2026-05-07T13:00:00Z"}}}
		]}
	}`)
	blocks := oomKilledEnrichBlocks(pod, nil, EnrichContext{})
	rows, _ := blocks[0]["data"].(map[string]any)["rows"].([]any)
	for _, r := range rows {
		row, _ := r.([]any)
		k, _ := row[0].(string)
		v, _ := row[1].(string)
		if k == "Container finished at" && v != "2026-05-08T03:00:00Z" {
			t.Errorf("Container finished at = %q; want state.terminated value", v)
		}
	}
}

func TestOOMKilledEnrichBlocks_PicksMostRecentAcrossContainers(t *testing.T) {
	// Two containers OOMed at different times. Pick the most recent
	// (highest finishedAt) so the table tells the operator about the
	// freshest signal.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"spec":{"containers":[
			{"name":"app","resources":{"limits":{"memory":"256Mi"}}},
			{"name":"sidecar","resources":{"limits":{"memory":"128Mi"}}}
		]},
		"status":{"containerStatuses":[
			{"name":"app",
			 "lastState":{"terminated":{"reason":"OOMKilled","finishedAt":"2026-05-07T13:00:00Z"}}},
			{"name":"sidecar",
			 "lastState":{"terminated":{"reason":"OOMKilled","finishedAt":"2026-05-08T01:00:00Z"}}}
		]}
	}`)
	blocks := oomKilledEnrichBlocks(pod, nil, EnrichContext{})
	rows, _ := blocks[0]["data"].(map[string]any)["rows"].([]any)
	got := map[string]string{}
	for _, r := range rows {
		row, _ := r.([]any)
		k, _ := row[0].(string)
		v, _ := row[1].(string)
		got[k] = v
	}
	if got["Container name"] != "sidecar" {
		t.Errorf("Container name = %q; want sidecar (most recent OOM)", got["Container name"])
	}
	if !strings.Contains(got["Container memory"], "128MB limit") {
		t.Errorf("memory row = %q; want sidecar's 128MB limit", got["Container memory"])
	}
}

func TestOOMKilledEnrichBlocks_NoMemoryResources(t *testing.T) {
	// Container declares no resources block at all — render
	// "No request, No limit" in this case.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"spec":{"containers":[{"name":"app"}]},
		"status":{"containerStatuses":[
			{"name":"app","lastState":{"terminated":{"reason":"OOMKilled","finishedAt":"2026-05-08T01:00:00Z"}}}
		]}
	}`)
	blocks := oomKilledEnrichBlocks(pod, nil, EnrichContext{})
	rows, _ := blocks[0]["data"].(map[string]any)["rows"].([]any)
	for _, r := range rows {
		row, _ := r.([]any)
		k, _ := row[0].(string)
		v, _ := row[1].(string)
		if k == "Container memory" && v != "No request, No limit" {
			t.Errorf("Container memory = %q; want \"No request, No limit\"", v)
		}
	}
}

func TestParseMemMB(t *testing.T) {
	// Coverage of the units Kubernetes accepts in memory quantities. Mi
	// and Gi are by far the most common — verify the binary suffix wins
	// over the SI single-letter parse, otherwise "Mi" would resolve to
	// the "M" branch (decimal MB instead of MiB) and the table would
	// under-report by ~5%.
	cases := []struct {
		in  string
		out int
	}{
		{"", 0},
		{"128Mi", 128},
		{"1Gi", 1024},
		{"512M", 488},      // 512_000_000 / 1024^2 = 488
		{"1G", 953},        // 1_000_000_000 / 1024^2 = 953
		{"104857600", 100}, // raw bytes → 100 MiB
		{"4Ki", 0},         // 4096 bytes < 1 MiB → 0
		{"abc", 0},         // unparseable → 0
		{"512.5Mi", 512},   // fractional truncated (int(float(x)))
	}
	for _, tc := range cases {
		if got := parseMemMB(tc.in); got != tc.out {
			t.Errorf("parseMemMB(%q) = %d; want %d", tc.in, got, tc.out)
		}
	}
}

// ---------- image_pull_backoff ----------

func TestImagePullBackoff_FiresOnTransitionIntoBackoff(t *testing.T) {
	for _, reason := range []string{"ImagePullBackOff", "ErrImagePull"} {
		oldPod := asObj(t, `{
			"metadata":{"name":"web-0","namespace":"prod"},
			"status":{"containerStatuses":[
				{"name":"app","image":"registry/web:bad",
				 "state":{"waiting":{"reason":"ContainerCreating"}}}
			]}
		}`)
		newPod := asObj(t, `{
			"metadata":{"name":"web-0","namespace":"prod"},
			"status":{"containerStatuses":[
				{"name":"app","image":"registry/web:bad",
				 "state":{"waiting":{"reason":"`+reason+`"}}}
			]}
		}`)
		if !imagePullBackoffMatcher().Predicate(newPod, oldPod) {
			t.Errorf("predicate did not fire on transition into reason=%s", reason)
		}
	}
}

func TestImagePullBackoff_FiresOnResyncAndCreate(t *testing.T) {
	// kubewatch's wire shape has obj==oldObj; resync de-dup is the rate
	// limiter's job, not the predicate's.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","image":"registry/web:bad",
			 "state":{"waiting":{"reason":"ImagePullBackOff"}}}
		]}
	}`)
	if !imagePullBackoffMatcher().Predicate(pod, pod) {
		t.Error("predicate must fire when obj==oldObj on a Pod stuck in ImagePullBackOff")
	}
	if !imagePullBackoffMatcher().Predicate(pod, nil) {
		t.Error("predicate must fire on first observation (oldObj=nil)")
	}
}

func TestImagePullBackoff_FingerprintIncludesImage(t *testing.T) {
	// Same workload, different bad image → different finding.
	mk := func(img string) map[string]any {
		return asObj(t, `{
			"metadata":{"name":"web-0","namespace":"prod"},
			"status":{"containerStatuses":[
				{"name":"app","image":"`+img+`",
				 "state":{"waiting":{"reason":"ImagePullBackOff"}}}
			]}
		}`)
	}
	m := imagePullBackoffMatcher()
	if m.FingerprintFn(mk("registry/web:bad-v1")) == m.FingerprintFn(mk("registry/web:bad-v2")) {
		t.Error("different bad images must produce different fingerprints")
	}
}

// ---------- image_pull_backoff enrichment ----------

func TestImagePullBackoffEnrichBlocks_BuildsHeaderAndImageBlocks(t *testing.T) {
	// For every container in ImagePullBackOff/ErrImagePull, emit a header
	// with the container name and a markdown line with the image. The
	// trigger-emitted Finding carries the same evidence the legacy
	// playbook produced (minus the events-driven Investigator analysis,
	// which needs a K8s API call and is deferred to a relay-side enricher).
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","image":"registry/web:bad-v1",
			 "state":{"waiting":{"reason":"ImagePullBackOff","message":"back-off pulling image"}}}
		]}
	}`)
	blocks := imagePullBackoffEnrichBlocks(pod, nil, EnrichContext{})
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d; want 2 (header + image)", len(blocks))
	}
	if blocks[0]["type"] != "header" || blocks[0]["data"] != "ImagePullBackOff in container app" {
		t.Errorf("blocks[0] = %#v; want header for container app", blocks[0])
	}
	if blocks[1]["type"] != "markdown" || blocks[1]["data"] != "*Image:* registry/web:bad-v1" {
		t.Errorf("blocks[1] = %#v; want markdown with image", blocks[1])
	}
}

func TestImagePullBackoffEnrichBlocks_AcceptsErrImagePull(t *testing.T) {
	// ErrImagePull is the transient cousin of ImagePullBackOff (kubelet
	// emits it on the first failure, before the backoff kicks in).
	// get_image_pull_backoff_container_statuses accepts both.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","image":"registry/web:bad",
			 "state":{"waiting":{"reason":"ErrImagePull"}}}
		]}
	}`)
	blocks := imagePullBackoffEnrichBlocks(pod, nil, EnrichContext{})
	if len(blocks) != 2 {
		t.Errorf("ErrImagePull must produce blocks; got %d", len(blocks))
	}
}

func TestImagePullBackoffEnrichBlocks_PerContainerMultiContainer(t *testing.T) {
	// Two containers fail to pull, one runs fine. Two header/image pairs,
	// nothing for the running container. Order matches container order.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","image":"registry/web:bad",
			 "state":{"waiting":{"reason":"ImagePullBackOff"}}},
			{"name":"sidecar","image":"registry/log:ok",
			 "state":{"running":{"startedAt":"2026-05-08T03:00:00Z"}}},
			{"name":"agent","image":"registry/agent:bad",
			 "state":{"waiting":{"reason":"ErrImagePull"}}}
		]}
	}`)
	blocks := imagePullBackoffEnrichBlocks(pod, nil, EnrichContext{})
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d; want 4 (2 failing × header+markdown)", len(blocks))
	}
	if blocks[0]["data"] != "ImagePullBackOff in container app" {
		t.Errorf("blocks[0] = %v; want app header first", blocks[0]["data"])
	}
	if blocks[2]["data"] != "ImagePullBackOff in container agent" {
		t.Errorf("blocks[2] = %v; want agent header second", blocks[2]["data"])
	}
}

func TestImagePullBackoffEnrichBlocks_SkipsOtherWaitingReasons(t *testing.T) {
	// ContainerCreating, CrashLoopBackOff, etc. — not our concern.
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","image":"registry/web",
			 "state":{"waiting":{"reason":"CrashLoopBackOff"}}},
			{"name":"sidecar","image":"registry/log",
			 "state":{"waiting":{"reason":"ContainerCreating"}}}
		]}
	}`)
	if got := imagePullBackoffEnrichBlocks(pod, nil, EnrichContext{}); got != nil {
		t.Errorf("imagePullBackoffEnrichBlocks = %v; want nil for non-IPB waiting reasons", got)
	}
}

func TestImagePullBackoffEnrichBlocks_NilPod(t *testing.T) {
	if got := imagePullBackoffEnrichBlocks(nil, nil, EnrichContext{}); got != nil {
		t.Errorf("nil obj must produce nil blocks; got %v", got)
	}
}

// ---------- job_failure ----------

func TestJobFailure_FiresOnTransitionToFailed(t *testing.T) {
	oldJob := asObj(t, `{
		"metadata":{"name":"j","namespace":"ns","uid":"u-1"},
		"status":{"conditions":[]}
	}`)
	newJob := asObj(t, `{
		"metadata":{"name":"j","namespace":"ns","uid":"u-1"},
		"status":{"conditions":[
			{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}
		]}
	}`)
	if !jobFailureMatcher().Predicate(newJob, oldJob) {
		t.Fatal("predicate should fire on transition to Failed=True")
	}
}

func TestJobFailure_DoesNotRefireWhilePersistentlyFailed(t *testing.T) {
	// Job that's already failed shouldn't keep firing on every kubewatch
	// update event.
	persistent := asObj(t, `{
		"metadata":{"name":"j","namespace":"ns","uid":"u-1"},
		"status":{"conditions":[{"type":"Failed","status":"True"}]}
	}`)
	if jobFailureMatcher().Predicate(persistent, persistent) {
		t.Error("predicate must not fire when oldObj already had Failed")
	}
}

// ---------- node_not_ready ----------

func TestNodeNotReady_FiresOnTransitionTrueToFalse(t *testing.T) {
	oldNode := asObj(t, `{
		"metadata":{"name":"n1"},
		"status":{"conditions":[{"type":"Ready","status":"True"}]}
	}`)
	newNode := asObj(t, `{
		"metadata":{"name":"n1"},
		"status":{"conditions":[{"type":"Ready","status":"False","lastTransitionTime":"2026-05-07T13:00:00Z"}]}
	}`)
	if !nodeNotReadyMatcher().Predicate(newNode, oldNode) {
		t.Error("predicate should fire on Ready True→False")
	}
}

func TestNodeNotReady_DoesNotRefireWhilePersistentlyNotReady(t *testing.T) {
	notReady := asObj(t, `{
		"metadata":{"name":"n1"},
		"status":{"conditions":[{"type":"Ready","status":"False"}]}
	}`)
	if nodeNotReadyMatcher().Predicate(notReady, notReady) {
		t.Error("predicate must not refire when oldObj was already NotReady")
	}
}

// ---------- engine integration ----------

func TestEngine_FiresMultipleMatchersForSamePod(t *testing.T) {
	// Pod is BOTH ImagePullBackOff (one container, just transitioned in)
	// AND Crashlooping (another container, restartCount went up). Both
	// matchers should fire — the engine doesn't stop at the first match.
	oldPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"sidecar","image":"r/sidecar:bad",
			 "state":{"waiting":{"reason":"ContainerCreating"}}},
			{"name":"app","restartCount":4,
			 "state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	newPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"sidecar","image":"r/sidecar:bad",
			 "state":{"waiting":{"reason":"ImagePullBackOff"}}},
			{"name":"app","restartCount":5,
			 "state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	eng := NewEngine(Builtins(), time.Now().Add(-time.Hour))
	matches := eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: newPod, OldObj: oldPod})
	names := matchNames(matches)
	if !contains(names, "pod_crash_loop") || !contains(names, "image_pull_backoff") {
		t.Errorf("expected both pod_crash_loop and image_pull_backoff to fire; got %v", names)
	}
}

func TestEngine_RateLimitSuppressesRepeats(t *testing.T) {
	// Two identical transitions back-to-back. First one fires, second is
	// rate-limit-suppressed (10-min window per fingerprint).
	oldPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":4,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	newPod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":5,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	eng := NewEngine(Builtins(), time.Now().Add(-time.Hour))
	first := eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: newPod, OldObj: oldPod})
	second := eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: newPod, OldObj: oldPod})
	if !contains(matchNames(first), "pod_crash_loop") {
		t.Fatal("first event should produce a crash_loop match")
	}
	if contains(matchNames(second), "pod_crash_loop") {
		t.Error("second event within rate-limit window should be suppressed")
	}
}

// TestEngine_RateLimiterSuppressesResyncFlood: kubewatch sends one
// UPDATE per resourceVersion bump for an already-broken Pod (and obj
// always equals oldObj on the wire). The engine's predicate fires every
// time, but the rate-limiter dedupes by (matcher, fingerprint) and the
// second fire inside the 10-min window is suppressed.
func TestEngine_RateLimiterSuppressesResyncFlood(t *testing.T) {
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod","creationTimestamp":"2025-01-01T00:00:00Z"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":1158,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}
		]}
	}`)
	eng := NewEngine(Builtins(), time.Now())
	first := eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: pod, OldObj: pod})
	if !contains(matchNames(first), "pod_crash_loop") {
		t.Fatal("first resync must fire crash_loop (rate-limit cache fresh)")
	}
	second := eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: pod, OldObj: pod})
	if contains(matchNames(second), "pod_crash_loop") {
		t.Error("second resync inside the 10-min window must be suppressed by rate-limiter")
	}
}

func TestEngine_KindFilter(t *testing.T) {
	// Pod-shaped matcher must not fire on a Deployment event.
	deploy := asObj(t, `{"metadata":{"name":"d","namespace":"ns"}}`)
	eng := NewEngine(Builtins(), time.Now().Add(-time.Hour))
	matches := eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Deployment", Obj: deploy})
	for _, n := range matchNames(matches) {
		if strings.HasPrefix(n, "pod_") {
			t.Errorf("Pod matcher %q fired on Deployment event", n)
		}
	}
}

func TestEngine_ReturnsEmptyForNoMatch(t *testing.T) {
	// Healthy Pod → no matchers fire → no Findings emitted (the whole point).
	pod := asObj(t, `{
		"metadata":{"name":"web-0","namespace":"prod"},
		"status":{"containerStatuses":[
			{"name":"app","restartCount":0,
			 "state":{"running":{"startedAt":"2026-05-07T13:00:00Z"}}}
		]}
	}`)
	eng := NewEngine(Builtins(), time.Now().Add(-time.Hour))
	matches := eng.Match(IncomingK8sEvent{Operation: "update", Kind: "Pod", Obj: pod})
	if len(matches) != 0 {
		t.Errorf("healthy Pod should produce 0 matches; got %d: %v", len(matches), matchNames(matches))
	}
}

// ---------- owner-walk ----------

func TestResolveOwner_ReplicaSetStripsHash(t *testing.T) {
	pod := asObj(t, `{
		"metadata":{"ownerReferences":[
			{"kind":"ReplicaSet","name":"web-7f9d8c5b6","controller":true}
		]}
	}`)
	o := ResolveOwner(pod)
	if o.Name != "web" || o.Kind != "deployment" {
		t.Errorf("owner = %+v; want {web deployment}", o)
	}
}

func TestResolveOwner_DaemonSetPassesThrough(t *testing.T) {
	pod := asObj(t, `{
		"metadata":{"ownerReferences":[
			{"kind":"DaemonSet","name":"node-agent","controller":true}
		]}
	}`)
	o := ResolveOwner(pod)
	if o.Name != "node-agent" || o.Kind != "daemonset" {
		t.Errorf("owner = %+v; want {node-agent daemonset}", o)
	}
}

func TestResolveOwner_BarePod(t *testing.T) {
	pod := asObj(t, `{"metadata":{"name":"naked"}}`)
	o := ResolveOwner(pod)
	if o.Name != "" || o.Kind != "" {
		t.Errorf("bare Pod owner should be zero; got %+v", o)
	}
}

func TestResolveOwner_PrefersControllerRef(t *testing.T) {
	// Multiple ownerRefs — only the controller=true one wins (k8s convention).
	pod := asObj(t, `{
		"metadata":{"ownerReferences":[
			{"kind":"DaemonSet","name":"unrelated","controller":false},
			{"kind":"ReplicaSet","name":"web-7f9d8c5b6","controller":true}
		]}
	}`)
	o := ResolveOwner(pod)
	if o.Name != "web" || o.Kind != "deployment" {
		t.Errorf("expected controller ref to win; got %+v", o)
	}
}

// ---------- helpers ----------

func matchNames(ms []Match) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Spec.Name
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	// avoid pulling in strconv just for tests where the values are small
	// digit-by-digit serialise (tests use restartCount 1-9 and a few 100s)
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
