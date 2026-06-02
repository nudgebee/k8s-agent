package triggers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Builtins returns the default matcher set the agent ships with.
// Order doesn't matter — the engine evaluates every matcher
// independently and emits one Match per fired trigger (a Pod can be
// both ImagePullBackOff and later CrashLoopBackOff and produce both).
//
// New matchers added here automatically participate in the engine. When
// stage 2.3 ships DB-stored playbook config, this list moves into a
// per-tenant pull from the api-server; the matcher SHAPE doesn't change.
func Builtins() []MatcherSpec {
	out := []MatcherSpec{
		podCrashLoopMatcher(),
		podOOMKilledMatcher(),
		imagePullBackoffMatcher(),
		jobFailureMatcher(),
		nodeNotReadyMatcher(),
	}
	// One matcher per babysitter-watched kind. We register them as
	// separate specs (rather than `Kind: "Any"` with an in-predicate
	// kind switch) so the engine's cheap kind-filter still drops events
	// for the wrong kind before we touch the diff machinery.
	for _, kind := range []string{"Deployment", "DaemonSet", "StatefulSet", "Ingress", "Rollout"} {
		out = append(out, babysitterChangeMatcher(kind))
	}
	return out
}

// ------- Pod CrashLoopBackOff -------

// podCrashLoopMatcher implements PodCrashLoopTrigger: fires when any
// container is in waiting state with reason CrashLoopBackOff AND has
// restarted at least `restart_count` times (default 2). Operation=UPDATE
// only — kubewatch sees CrashLoopBackOff as an UPDATE.
//
// Note on de-dup: we used to require `obj.restartCount > oldObj.restartCount`
// to detect a real transition vs. a kubewatch resync re-emit. That doesn't
// work — kubewatch's UpdateFunc shares one closure-scoped `var newEvent
// Event` (kubewatch/pkg/controller/controller.go:787) and the K8s informer
// passes `old`/`new` as references that often share underlying memory.
// Live capture confirms: every kubewatch UPDATE for a CrashLoopBackOff
// Pod has obj.restartCount == oldObj.restartCount, so the transition gate
// suppressed every real event.
//
// Rate limit: 1h per (owner, hour-bucket). A Pod stuck in CrashLoopBackOff
// fires ~1 Finding per hour. Earlier the matcher bucketed by
// restartCount/5 with a 10m rate limit — a fast-crashing Pod (5 restarts
// every ~10m) advanced the bucket every window and produced 20+ findings
// in 6h. Hour-bucket pattern mirrors podOOMKilledMatcher.
func podCrashLoopMatcher() MatcherSpec {
	const minRestarts = 2
	return MatcherSpec{
		Name:           "pod_crash_loop",
		Kind:           "Pod",
		Operations:     []string{"update"},
		AggregationKey: "report_crash_loop",
		Priority:       "HIGH",
		FindingType:    "issue",
		RateLimit:      time.Hour,
		Predicate: func(obj, _ map[string]any) bool {
			for _, cs := range podContainerStatuses(obj) {
				st, _ := cs["state"].(map[string]any)
				w, _ := st["waiting"].(map[string]any)
				if w == nil {
					continue
				}
				if reason, _ := w["reason"].(string); reason != "CrashLoopBackOff" {
					continue
				}
				rc, _ := cs["restartCount"].(float64)
				if int(rc) >= minRestarts {
					return true
				}
			}
			return false
		},
		FingerprintFn: func(obj map[string]any) string {
			ns, name := metaNS(obj), metaName(obj)
			owner := ResolveOwner(obj)
			if owner.Name != "" {
				name = owner.Name
			}
			// Hour bucket pairs with the 1h rate limit. After the
			// limit expires, the bucket has rolled too — so the next
			// CrashLoopBackOff produces a fresh fingerprint and a
			// fresh Finding. Mirrors podOOMKilledMatcher.
			hourBucket := time.Now().UTC().Truncate(time.Hour).Unix()
			return fp("report_crash_loop", ns, name, fmt.Sprintf("h%d", hourBucket))
		},
		EnrichBlocks: crashLoopEnrichBlocks,
	}
}

// crashLoopEnrichBlocks builds the markdown blocks report_crash_loop
// attaches to the Finding. For every container currently waiting with
// restartCount ≥ 1 it emits, in order:
//
//   - "*<name>* restart count: <count>"
//   - "*<name>* waiting reason: <reason>"            (when state.waiting set)
//   - "*<name>* termination reason: <reason>"        (state.terminated)
//   - "*<name>* termination reason: <reason>"        (lastState.terminated)
//
// The legacy enricher additionally shipped the previous container's logs
// as a FileBlock via pod.get_logs(previous=True). EnrichBlocks is a pure
// (obj, oldObj) function with no K8s API access, so the logs file is
// omitted here — closing it requires a relay-side enricher (plan §5b
// stage 2.2). The markdown blocks alone match what the UI's
// per-aggregation_key rendering already expects for `report_crash_loop`
// findings.
func crashLoopEnrichBlocks(obj, _ map[string]any, ec EnrichContext) []EvidenceBlock {
	if obj == nil {
		return nil
	}
	var blocks []EvidenceBlock
	for _, cs := range podContainerStatuses(obj) {
		name, _ := cs["name"].(string)
		st, _ := cs["state"].(map[string]any)
		// "Crashed" predicate: state.waiting set AND restartCount >= 1.
		if _, hasWaiting := st["waiting"].(map[string]any); !hasWaiting {
			continue
		}
		rc, _ := cs["restartCount"].(float64)
		if int(rc) < 1 {
			continue
		}
		if name == "" {
			continue
		}
		blocks = append(blocks, markdownBlock(fmt.Sprintf("*%s* restart count: %d", name, int(rc))))
		if w, _ := st["waiting"].(map[string]any); w != nil {
			if reason, _ := w["reason"].(string); reason != "" {
				blocks = append(blocks, markdownBlock(fmt.Sprintf("*%s* waiting reason: %s", name, reason)))
			}
		}
		if t, _ := st["terminated"].(map[string]any); t != nil {
			if reason, _ := t["reason"].(string); reason != "" {
				blocks = append(blocks, markdownBlock(fmt.Sprintf("*%s* termination reason: %s", name, reason)))
			}
		}
		if ls, _ := cs["lastState"].(map[string]any); ls != nil {
			if t, _ := ls["terminated"].(map[string]any); t != nil {
				if reason, _ := t["reason"].(string); reason != "" {
					blocks = append(blocks, markdownBlock(fmt.Sprintf("*%s* termination reason: %s", name, reason)))
				}
			}
		}
	}
	return blocks
}

// markdownBlock builds the structured-data shape for MarkdownBlock.
// additional_info is nil for this enricher (only set for
// callback/header/list).
func markdownBlock(text string) EvidenceBlock {
	return EvidenceBlock{
		"type":            "markdown",
		"data":            text,
		"additional_info": nil,
	}
}

// ------- Pod OOMKilled -------

// podOOMKilledMatcher implements PodOomKilledTrigger: fires when any
// container's lastState.terminated.reason == "OOMKilled". Fires whenever
// any container's lastState.terminated.reason == OOMKilled.
// Same kubewatch oldObj/obj pointer-aliasing problem as pod_crash_loop —
// the previous transition gate (`prev[name] != obj[name]`) never tripped
// because oldObj is the same snapshot as obj. We rely on the 1h rate
// limit + hour-bucket fingerprint to dedupe: at most one Finding per
// (owner, hour-bucket), so a Pod stuck OOMing produces ~1 fire/hour.
func podOOMKilledMatcher() MatcherSpec {
	return MatcherSpec{
		Name:           "pod_oom_killed",
		Kind:           "Pod",
		Operations:     []string{"update"},
		AggregationKey: "pod_oom_killer_enricher",
		Priority:       "HIGH",
		FindingType:    "issue",
		RateLimit:      time.Hour,
		Predicate: func(obj, _ map[string]any) bool {
			for _, ts := range oomKilledFinishedAts(obj) {
				if ts != "" {
					return true
				}
			}
			return false
		},
		FingerprintFn: func(obj map[string]any) string {
			ns, name := metaNS(obj), metaName(obj)
			owner := ResolveOwner(obj)
			if owner.Name != "" {
				name = owner.Name
			}
			// Hour bucket pairs with the 1h rate limit. After the limit
			// expires, the bucket has rolled too — so the next OOM
			// produces a fresh fingerprint and a fresh Finding.
			hourBucket := time.Now().UTC().Truncate(time.Hour).Unix()
			return fp("pod_oom_killer_enricher", ns, name, fmt.Sprintf("h%d", hourBucket))
		},
		EnrichBlocks: oomKilledEnrichBlocks,
	}
}

// oomKilledEnrichBlocks builds the TableBlock pod_oom_killer_enricher
// attaches to the Finding: pod / namespace / node + the OOM-killed
// container's name, memory requests/limits, and
// terminated.startedAt/finishedAt timestamps.
//
// The legacy enricher additionally fetched the Node and reported
// "Node allocated memory" as a row. We can't reach the K8s API from
// EnrichBlocks (it's a pure (obj, oldObj) function), so that row is
// omitted here — the caller still gets every Pod-derivable field.
// Closing that gap requires a relay-side enricher (plan §5b stage 2.2);
// for the trigger-emitted Finding, the rest of the table is enough for
// the UI's existing per-aggregation_key rendering.
func oomKilledEnrichBlocks(obj, _ map[string]any, ec EnrichContext) []EvidenceBlock {
	if obj == nil {
		return nil
	}
	cs := mostRecentOOMKilledContainerStatus(obj)
	if cs == nil {
		return nil
	}
	rows := [][]string{
		{"Pod", metaName(obj)},
		{"Namespace", metaNS(obj)},
	}
	if node := podNodeName(obj); node != "" {
		rows = append(rows, []string{"Node Name", node})
	}
	cName, _ := cs["name"].(string)
	if cName != "" {
		rows = append(rows, []string{"Container name", cName})
		req, lim := containerMemoryResourcesMB(obj, cName)
		memReq := "No request"
		if req > 0 {
			memReq = fmt.Sprintf("%dMB request", req)
		}
		memLim := "No limit"
		if lim > 0 {
			memLim = fmt.Sprintf("%dMB limit", lim)
		}
		rows = append(rows, []string{"Container memory", memReq + ", " + memLim})
	}
	// Pull timestamps from whichever terminated state the most-recent-OOM
	// scan picked (state preferred over lastState — same precedence
	// pod_most_recent_oom_killed_container applies).
	if t, _ := containerOOMTermination(cs); t != nil {
		if v, _ := t["startedAt"].(string); v != "" {
			rows = append(rows, []string{"Container started at", v})
		}
		if v, _ := t["finishedAt"].(string); v != "" {
			rows = append(rows, []string{"Container finished at", v})
		}
	}

	rowsAny := make([]any, len(rows))
	for i, r := range rows {
		rowsAny[i] = []any{r[0], r[1]}
	}
	return []EvidenceBlock{{
		"type": "table",
		"data": map[string]any{
			"table_name":       "*Pod and Node OOMKilled data*",
			"headers":          []any{"field", "value"},
			"rows":             rowsAny,
			"column_renderers": map[string]any{},
		},
		"additional_info": nil,
	}}
}

// ------- Image pull backoff -------

// imagePullBackoffMatcher fires when any container is currently in
// ImagePullBackOff or ErrImagePull. Same kubewatch pointer-aliasing
// caveat as pod_crash_loop — we can't rely on oldObj→obj transition
// detection. The fingerprint (`owner, image`) dedupes: distinct bad
// images each fire once per 10-min window.
func imagePullBackoffMatcher() MatcherSpec {
	return MatcherSpec{
		Name:           "image_pull_backoff",
		Kind:           "Pod",
		Operations:     []string{"update"},
		AggregationKey: "image_pull_backoff_reporter",
		Priority:       "MEDIUM",
		FindingType:    "issue",
		RateLimit:      10 * time.Minute,
		Predicate: func(obj, _ map[string]any) bool {
			return len(containersInImagePullBackoff(obj)) > 0
		},
		FingerprintFn: func(obj map[string]any) string {
			ns, name := metaNS(obj), metaName(obj)
			owner := ResolveOwner(obj)
			if owner.Name != "" {
				name = owner.Name
			}
			// Include the bad image — different bad images on the same
			// workload should be distinct Findings (operator just typo'd
			// one container, the rest are fine).
			image := firstFailingImage(obj)
			return fp("image_pull_backoff_reporter", ns, name, image)
		},
		EnrichBlocks: imagePullBackoffEnrichBlocks,
	}
}

// imagePullBackoffEnrichBlocks builds the HeaderBlock + MarkdownBlock pair
// image_pull_backoff_reporter attaches per failing container:
//
//   - "ImagePullBackOff in container <name>"  (header)
//   - "*Image:* <image>"                       (markdown)
//
// The legacy enricher additionally called the K8s Events API and ran an
// ImagePullBackoffInvestigator over Warning/Failed events to classify the
// reason (RepoDoesntExist / NotAuthorized / ImageDoesntExist / TagNotFound)
// and emit a "*Reason:* X" or "*Possible reasons:* …" markdown.
// EnrichBlocks is a pure (obj, oldObj) function with no API access, so
// the investigator block is omitted here — closing it requires a
// relay-side enricher (plan §5b stage 2.2). The header + image lines
// alone match what the UI's per-aggregation_key rendering already expects.
func imagePullBackoffEnrichBlocks(obj, _ map[string]any, ec EnrichContext) []EvidenceBlock {
	if obj == nil {
		return nil
	}
	var blocks []EvidenceBlock
	for _, cs := range podContainerStatuses(obj) {
		st, _ := cs["state"].(map[string]any)
		w, _ := st["waiting"].(map[string]any)
		if w == nil {
			continue
		}
		reason, _ := w["reason"].(string)
		// Both ImagePullBackOff and ErrImagePull count (same as
		// get_image_pull_backoff_container_statuses). The matcher's
		// predicate uses the same set, so the enricher must too —
		// otherwise the trigger fires with no blocks.
		if reason != "ImagePullBackOff" && reason != "ErrImagePull" {
			continue
		}
		name, _ := cs["name"].(string)
		image, _ := cs["image"].(string)
		if name == "" {
			continue
		}
		blocks = append(blocks,
			headerBlock(fmt.Sprintf("ImagePullBackOff in container %s", name)),
			markdownBlock(fmt.Sprintf("*Image:* %s", image)),
		)
	}
	return blocks
}

// headerBlock builds the structured-data shape for HeaderBlock.
// additional_info is nil — only set for callback/list blocks.
func headerBlock(text string) EvidenceBlock {
	return EvidenceBlock{
		"type":            "header",
		"data":            text,
		"additional_info": nil,
	}
}

// ------- Job failure (transition) -------

// jobFailureMatcher implements JobFailedTrigger: fires only on the
// transition into Failed (the new obj has a
// `status.conditions[].type=Failed` and the old obj did not). Without
// the transition gate we'd fire repeatedly for as long as the failed
// Job lingers in the cluster.
func jobFailureMatcher() MatcherSpec {
	return MatcherSpec{
		Name:           "job_failure",
		Kind:           "Job",
		Operations:     []string{"update"},
		AggregationKey: "job_failure",
		Priority:       "MEDIUM",
		FindingType:    "issue",
		RateLimit:      0, // terminal — fingerprint by UID is enough
		Predicate: func(obj, oldObj map[string]any) bool {
			return jobHasFailedCondition(obj) && !jobHasFailedCondition(oldObj)
		},
		FingerprintFn: func(obj map[string]any) string {
			ns := metaNS(obj)
			uid := metaUID(obj)
			return fp("job_failure", ns, uid)
		},
	}
}

// ------- Node NotReady (transition) -------

// nodeNotReadyMatcher fires when a Node's Ready condition transitions
// from True to False. Without the transition gate, a permanently-broken
// Node would re-fire every kubelet heartbeat.
func nodeNotReadyMatcher() MatcherSpec {
	return MatcherSpec{
		Name:           "node_not_ready",
		Kind:           "Node",
		Operations:     []string{"update"},
		AggregationKey: "node_not_ready",
		Priority:       "HIGH",
		FindingType:    "issue",
		RateLimit:      0, // transition-gated, no need
		Predicate: func(obj, oldObj map[string]any) bool {
			return nodeReadyStatus(obj) == "False" && nodeReadyStatus(oldObj) != "False"
		},
		FingerprintFn: func(obj map[string]any) string {
			name := metaName(obj)
			// Include lastTransitionTime so a flapping Node produces
			// distinct Findings per transition rather than one forever.
			ts := nodeReadyLastTransition(obj)
			return fp("node_not_ready", name, ts)
		},
	}
}

// ------- Babysitter (config-change with diff) -------

// babysitterChangeMatcher implements resource_babysitter. Fires
// on UPDATE when the diff between obj and oldObj contains at least one
// monitored field (default: "spec" + sub-paths) that isn't in the
// omitted list (status, metadata.generation, .resourceVersion,
// .managedFields). Emits aggregation_key
// "ConfigurationChange/KubernetesResource/Change"
// (FindingAggregationKey.CONFIGURATION_CHANGE_KUBERNETES_RESOURCE_CHANGE).
//
// Unlike the "drop without diff body is useless" concern flagged in
// stage-2.1 planning, this matcher computes the diff in the agent and
// attaches it as a `markdown` evidence block via EnrichBlocks. The
// Finding therefore carries the actual change set on its own — no
// server-side enricher needed.
func babysitterChangeMatcher(kind string) MatcherSpec {
	diffOpt := DefaultSpecDiffOptions()
	return MatcherSpec{
		Name:           "babysitter_" + strings.ToLower(kind),
		Kind:           kind,
		Operations:     []string{"update"},
		AggregationKey: "ConfigurationChange/KubernetesResource/Change",
		Priority:       "INFO",
		FindingType:    "configuration_change",
		// Each spec change has its own resourceVersion → its own
		// fingerprint → no rate-limit needed for dedup. We do set a
		// short rate-limit to absorb the rare spurious double-fire
		// (kubewatch occasionally re-delivers the same event).
		RateLimit: 30 * time.Second,
		Predicate: func(obj, oldObj map[string]any) bool {
			if oldObj == nil {
				return false
			}
			diffs := ComputeSpecDiff(obj, oldObj, diffOpt)
			return len(diffs) > 0
		},
		FingerprintFn: func(obj map[string]any) string {
			ns := metaNS(obj)
			name := metaName(obj)
			// Include resourceVersion so each distinct spec change gets
			// its own Finding (Plan agent: "include obj.metadata.
			// resourceVersion so each spec change is a distinct
			// finding").
			meta, _ := obj["metadata"].(map[string]any)
			rv, _ := meta["resourceVersion"].(string)
			return fp("ConfigurationChange/KubernetesResource/Change", ns, name, rv)
		},
		EnrichBlocks: func(obj, oldObj map[string]any, _ EnrichContext) []EvidenceBlock {
			diffs := ComputeSpecDiff(obj, oldObj, diffOpt)
			if len(diffs) == 0 {
				return nil
			}
			// Emit the `type: "diff"` block — the only shape the UI
			// renders as a side-by-side YAML diff. KubernetesTable2.jsx:1184-1199
			// filters on type==='diff' and feeds data.old/data.new to the
			// CodeMirrorDiffViewer; any other block ("markdown", etc.) shows
			// "No diff available."
			return []EvidenceBlock{
				BuildKubernetesDiffBlock(obj, oldObj, kind, diffs),
			}
		},
	}
}

// -------- helpers --------

func podContainerStatuses(obj map[string]any) []map[string]any {
	st, _ := obj["status"].(map[string]any)
	if st == nil {
		return nil
	}
	all := []map[string]any{}
	for _, key := range []string{"containerStatuses", "initContainerStatuses", "ephemeralContainerStatuses"} {
		raw, _ := st[key].([]any)
		for _, item := range raw {
			cs, _ := item.(map[string]any)
			if cs != nil {
				all = append(all, cs)
			}
		}
	}
	return all
}

func firstFailingImage(obj map[string]any) string {
	for _, cs := range podContainerStatuses(obj) {
		st, _ := cs["state"].(map[string]any)
		w, _ := st["waiting"].(map[string]any)
		if w == nil {
			continue
		}
		reason, _ := w["reason"].(string)
		if reason == "ImagePullBackOff" || reason == "ErrImagePull" {
			img, _ := cs["image"].(string)
			return img
		}
	}
	return ""
}

func jobHasFailedCondition(obj map[string]any) bool {
	if obj == nil {
		return false
	}
	st, _ := obj["status"].(map[string]any)
	if st == nil {
		return false
	}
	conds, _ := st["conditions"].([]any)
	for _, c := range conds {
		cm, _ := c.(map[string]any)
		t, _ := cm["type"].(string)
		s, _ := cm["status"].(string)
		if t == "Failed" && s == "True" {
			return true
		}
	}
	return false
}

func nodeReadyStatus(obj map[string]any) string {
	if obj == nil {
		return ""
	}
	st, _ := obj["status"].(map[string]any)
	if st == nil {
		return ""
	}
	conds, _ := st["conditions"].([]any)
	for _, c := range conds {
		cm, _ := c.(map[string]any)
		t, _ := cm["type"].(string)
		if t == "Ready" {
			s, _ := cm["status"].(string)
			return s
		}
	}
	return ""
}

func nodeReadyLastTransition(obj map[string]any) string {
	st, _ := obj["status"].(map[string]any)
	conds, _ := st["conditions"].([]any)
	for _, c := range conds {
		cm, _ := c.(map[string]any)
		t, _ := cm["type"].(string)
		if t == "Ready" {
			ts, _ := cm["lastTransitionTime"].(string)
			return ts
		}
	}
	return ""
}

func metaName(obj map[string]any) string {
	m, _ := obj["metadata"].(map[string]any)
	n, _ := m["name"].(string)
	return n
}

func metaNS(obj map[string]any) string {
	m, _ := obj["metadata"].(map[string]any)
	n, _ := m["namespace"].(string)
	return n
}

func metaUID(obj map[string]any) string {
	m, _ := obj["metadata"].(map[string]any)
	u, _ := m["uid"].(string)
	return u
}

// fp produces a stable sha256 hex of joined fields. Used by FingerprintFn
// so all matchers produce same-shape fingerprints (inspectable, hex-safe).
func fp(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, ":")))
	return hex.EncodeToString(h[:])
}

// oomKilledFinishedAts returns {container_name: lastState.terminated.finishedAt}
// for every container whose most recent termination was OOMKilled. The
// pod_oom_killed predicate uses the presence of any non-empty entry as
// the fire signal; we keep the timestamp on the value so the enricher
// path can differentiate which container OOMed when a Pod has several.
func oomKilledFinishedAts(obj map[string]any) map[string]string {
	out := map[string]string{}
	if obj == nil {
		return out
	}
	for _, cs := range podContainerStatuses(obj) {
		name, _ := cs["name"].(string)
		ls, _ := cs["lastState"].(map[string]any)
		term, _ := ls["terminated"].(map[string]any)
		if term == nil {
			continue
		}
		if reason, _ := term["reason"].(string); reason != "OOMKilled" {
			continue
		}
		ts, _ := term["finishedAt"].(string)
		out[name] = ts // empty ts is fine; oldObj will also have empty ts → no-op
	}
	return out
}

// mostRecentOOMKilledContainerStatus walks every container status
// (regular + init) and returns the one whose most recent termination
// was OOMKilled with the highest finishedAt timestamp.
// state.terminated is preferred over lastState.terminated (a container
// currently in OOMKilled state is a fresher signal than one that recovered).
// Returns nil when no container in the pod was OOMKilled.
func mostRecentOOMKilledContainerStatus(obj map[string]any) map[string]any {
	var best map[string]any
	var bestTS string
	for _, cs := range podContainerStatuses(obj) {
		t, ts := containerOOMTermination(cs)
		if t == nil {
			continue
		}
		if best == nil || ts > bestTS {
			best, bestTS = cs, ts
		}
	}
	return best
}

// containerOOMTermination returns (terminated_state, finishedAt) when the
// container's most recent termination was OOMKilled. state wins over
// lastState (current OOM is fresher than recovered OOM). Returns (nil, "")
// when neither side has an OOMKilled terminated state.
func containerOOMTermination(cs map[string]any) (map[string]any, string) {
	if t := terminatedIfOOM(cs["state"]); t != nil {
		ts, _ := t["finishedAt"].(string)
		return t, ts
	}
	if t := terminatedIfOOM(cs["lastState"]); t != nil {
		ts, _ := t["finishedAt"].(string)
		return t, ts
	}
	return nil, ""
}

func terminatedIfOOM(v any) map[string]any {
	st, _ := v.(map[string]any)
	if st == nil {
		return nil
	}
	t, _ := st["terminated"].(map[string]any)
	if t == nil {
		return nil
	}
	if reason, _ := t["reason"].(string); reason != "OOMKilled" {
		return nil
	}
	return t
}

// podNodeName returns spec.nodeName, or "" when unset (pod not yet scheduled).
func podNodeName(obj map[string]any) string {
	spec, _ := obj["spec"].(map[string]any)
	n, _ := spec["nodeName"].(string)
	return n
}

// containerMemoryResourcesMB walks spec.containers (and initContainers /
// ephemeralContainers) for the named container and returns (requests_mb,
// limits_mb). Either or both can be 0 when the container declares no
// requests/limits.
func containerMemoryResourcesMB(obj map[string]any, containerName string) (int, int) {
	spec, _ := obj["spec"].(map[string]any)
	if spec == nil {
		return 0, 0
	}
	for _, key := range []string{"containers", "initContainers", "ephemeralContainers"} {
		raw, _ := spec[key].([]any)
		for _, item := range raw {
			c, _ := item.(map[string]any)
			if c == nil {
				continue
			}
			if name, _ := c["name"].(string); name != containerName {
				continue
			}
			res, _ := c["resources"].(map[string]any)
			req, _ := res["requests"].(map[string]any)
			lim, _ := res["limits"].(map[string]any)
			reqMem, _ := req["memory"].(string)
			limMem, _ := lim["memory"].(string)
			return parseMemMB(reqMem), parseMemMB(limMem)
		}
	}
	return 0, 0
}

// parseMemMB parses a Kubernetes memory quantity ("128Mi", "1Gi", "512M",
// raw bytes, …) into MB (1024*1024 bytes). Empty / unparseable input → 0
// (the row just renders as "No request").
func parseMemMB(s string) int {
	bytes := parseMemBytes(s)
	return int(bytes / (1024 * 1024))
}

func parseMemBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// Two-letter binary suffixes (Ki, Mi, Gi, …) take precedence over the
	// single-letter SI suffixes since "Mi" ends in "i" but contains "M".
	binary := map[string]int64{
		"Ki": 1024,
		"Mi": 1024 * 1024,
		"Gi": 1024 * 1024 * 1024,
		"Ti": 1024 * 1024 * 1024 * 1024,
		"Pi": 1024 * 1024 * 1024 * 1024 * 1024,
		"Ei": 1024 * 1024 * 1024 * 1024 * 1024 * 1024,
	}
	if len(s) > 2 {
		if mult, ok := binary[s[len(s)-2:]]; ok {
			return parseInt64(s[:len(s)-2]) * mult
		}
	}
	si := map[byte]int64{
		'k': 1000,
		'K': 1000,
		'M': 1000 * 1000,
		'G': 1000 * 1000 * 1000,
		'T': 1000 * 1000 * 1000 * 1000,
		'P': 1000 * 1000 * 1000 * 1000 * 1000,
		'E': 1000 * 1000 * 1000 * 1000 * 1000 * 1000,
	}
	if len(s) > 1 {
		if mult, ok := si[s[len(s)-1]]; ok {
			return parseInt64(s[:len(s)-1]) * mult
		}
	}
	return parseInt64(s)
}

func parseInt64(s string) int64 {
	var n int64
	var seenDigit bool
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			n = n*10 + int64(c-'0')
			seenDigit = true
		case c == '.':
			// truncate fractional part — equivalent to int(float(x))
			if !seenDigit {
				return 0
			}
			return n
		default:
			return 0
		}
	}
	if !seenDigit {
		return 0
	}
	return n
}

// containersInImagePullBackoff returns the set of container names whose
// current state is waiting with reason ImagePullBackOff or ErrImagePull.
func containersInImagePullBackoff(obj map[string]any) map[string]bool {
	out := map[string]bool{}
	if obj == nil {
		return out
	}
	for _, cs := range podContainerStatuses(obj) {
		name, _ := cs["name"].(string)
		st, _ := cs["state"].(map[string]any)
		w, _ := st["waiting"].(map[string]any)
		if w == nil {
			continue
		}
		reason, _ := w["reason"].(string)
		if reason == "ImagePullBackOff" || reason == "ErrImagePull" {
			out[name] = true
		}
	}
	return out
}
