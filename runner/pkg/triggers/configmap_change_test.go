package triggers

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The shape this matcher exists for: a value consumed with `envFrom`.
// Editing it leaves the consuming Deployment's spec byte-identical, so
// `kubectl rollout history` has no revision to diff, and Kubernetes emits
// no event for a ConfigMap write. This Finding is the only record that
// the value used to be 60.
const (
	sessionsConfigBefore = `{
		"metadata":{"name":"sessions-config","namespace":"sessions","resourceVersion":"100"},
		"data":{"CACHE_TTL_SECONDS":"60","WRITE_KB_PER_SEC":"512","LOG_LEVEL":"info"}
	}`
	sessionsConfigAfter = `{
		"metadata":{"name":"sessions-config","namespace":"sessions","resourceVersion":"104"},
		"data":{"CACHE_TTL_SECONDS":"1800","WRITE_KB_PER_SEC":"512","LOG_LEVEL":"info"}
	}`
)

func TestConfigMapChange_FiresOnDataValueChange(t *testing.T) {
	old := mustObj(t, sessionsConfigBefore)
	updated := mustObj(t, sessionsConfigAfter)

	m := configMapChangeMatcher()
	if !m.Predicate(updated, old) {
		t.Fatal("predicate must fire on a data value change")
	}
	if m.AggregationKey != "ConfigurationChange/KubernetesResource/Change" {
		t.Errorf("aggregation_key = %q; want the shared change key so this lands in the same change history as workload changes", m.AggregationKey)
	}
	if m.FindingType != "configuration_change" {
		t.Errorf("finding_type = %q; want configuration_change", m.FindingType)
	}

	diffs := ComputeSpecDiff(updated, old, ConfigMapDiffOptions())
	if len(diffs) != 1 {
		t.Fatalf("expected exactly the changed key; got %d: %+v", len(diffs), diffs)
	}
	if diffs[0].Path != "data.CACHE_TTL_SECONDS" {
		t.Errorf("path = %q; want data.CACHE_TTL_SECONDS", diffs[0].Path)
	}
	// The old value is the whole point — the cluster keeps no other copy.
	if diffs[0].Before != "60" || diffs[0].After != "1800" {
		t.Errorf("before/after = %v/%v; want 60/1800", diffs[0].Before, diffs[0].After)
	}
}

// The babysitter defaults monitor "spec", which a ConfigMap does not
// have. Registering ConfigMap against them would produce a matcher that
// silently never fires.
func TestConfigMapChange_DefaultSpecOptionsWouldNeverFire(t *testing.T) {
	old := mustObj(t, sessionsConfigBefore)
	updated := mustObj(t, sessionsConfigAfter)
	if diffs := ComputeSpecDiff(updated, old, DefaultSpecDiffOptions()); len(diffs) != 0 {
		t.Fatalf("guard test is wrong: spec-rooted options produced %+v", diffs)
	}
}

func TestConfigMapChange_IgnoresMetadataChurn(t *testing.T) {
	// resourceVersion bumps, a managedFields rewrite, and kubectl's
	// last-applied annotation (which carries a full copy of the object,
	// so an apply mutates it on every write).
	old := mustObj(t, `{
		"metadata":{"name":"c","namespace":"prod","resourceVersion":"100","managedFields":[{"manager":"old"}],
			"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"k\":\"v\"}}"}},
		"data":{"k":"v"}
	}`)
	updated := mustObj(t, `{
		"metadata":{"name":"c","namespace":"prod","resourceVersion":"101","managedFields":[{"manager":"new"}],
			"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"k\":\"v\"},\"x\":1}"}},
		"data":{"k":"v"}
	}`)
	if configMapChangeMatcher().Predicate(updated, old) {
		t.Error("metadata-only churn must not produce a change event")
	}
}

func TestConfigMapChange_DoesNotFireOnCreate(t *testing.T) {
	if configMapChangeMatcher().Predicate(mustObj(t, sessionsConfigAfter), nil) {
		t.Error("must not fire on CREATE (no previous value to diff against)")
	}
}

func TestConfigMapChange_FiresOnBinaryData(t *testing.T) {
	old := mustObj(t, `{"metadata":{"name":"c","namespace":"prod"},"binaryData":{"cert":"YWJj"}}`)
	updated := mustObj(t, `{"metadata":{"name":"c","namespace":"prod"},"binaryData":{"cert":"eHl6"}}`)
	if !configMapChangeMatcher().Predicate(updated, old) {
		t.Error("binaryData is payload too — a change to it must fire")
	}
}

func TestConfigMapChange_SkipsSelfRewritingConfigMaps(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		metadata string
	}{
		{
			desc:     "pre-Lease leader election rewrites the holder every few seconds",
			metadata: `{"name":"ingress-controller-leader","namespace":"ingress","annotations":{"control-plane.alpha.kubernetes.io/leader":"{\"holderIdentity\":\"a\"}"}}`,
		},
		{
			desc:     "root CA bundle, written into every namespace by a controller",
			metadata: `{"name":"kube-root-ca.crt","namespace":"prod"}`,
		},
		{
			desc:     "legacy Helm release bookkeeping, not user-facing config",
			metadata: `{"name":"release-state","namespace":"prod","labels":{"OWNER":"TILLER"}}`,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			old := mustObj(t, `{"metadata":`+tc.metadata+`,"data":{"k":"v1"}}`)
			updated := mustObj(t, `{"metadata":`+tc.metadata+`,"data":{"k":"v2"}}`)
			if configMapChangeMatcher().Predicate(updated, old) {
				t.Error("must be filtered out of the change feed")
			}
		})
	}
}

func TestConfigMapChange_EnrichAttachesDiffBlock(t *testing.T) {
	old := mustObj(t, sessionsConfigBefore)
	updated := mustObj(t, sessionsConfigAfter)

	blocks := configMapChangeMatcher().EnrichBlocks(updated, old, EnrichContext{})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 evidence block; got %d", len(blocks))
	}
	b := blocks[0]
	// The UI renders only type=="diff" side-by-side; anything else shows
	// "No diff available."
	if b["type"] != "diff" {
		t.Fatalf("block type = %v; want diff", b["type"])
	}
	data, _ := b["data"].(map[string]any)
	if data == nil {
		t.Fatalf("block.data missing: %v", b["data"])
	}
	if data["resource_name"] != "configmap/sessions/sessions-config.yaml" {
		t.Errorf("resource_name = %v; want configmap/sessions/sessions-config.yaml", data["resource_name"])
	}
	oldYAML, _ := data["old"].(string)
	newYAML, _ := data["new"].(string)
	if !strings.Contains(oldYAML, "60") || !strings.Contains(newYAML, "1800") {
		t.Errorf("rendered YAML must carry both values.\nold: %s\nnew: %s", oldYAML, newYAML)
	}
	paths, _ := data["updated_paths"].([]any)
	if len(paths) != 1 || paths[0] != "data.CACHE_TTL_SECONDS" {
		t.Errorf("updated_paths = %v; want [data.CACHE_TTL_SECONDS]", paths)
	}
}

func TestConfigMapChange_CapsLargeValues(t *testing.T) {
	// A ConfigMap holding a whole file — the diff block renders old AND
	// new, so an uncapped change here ships megabytes of evidence.
	bigOld := strings.Repeat("a", maxConfigMapValueBytes*3)
	bigNew := strings.Repeat("b", maxConfigMapValueBytes*3)
	old := mustObj(t, `{"metadata":{"name":"app","namespace":"prod"},"data":{"app.py":"`+bigOld+`"}}`)
	updated := mustObj(t, `{"metadata":{"name":"app","namespace":"prod"},"data":{"app.py":"`+bigNew+`"}}`)

	blocks := configMapChangeMatcher().EnrichBlocks(updated, old, EnrichContext{})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 evidence block; got %d", len(blocks))
	}
	data, _ := blocks[0]["data"].(map[string]any)
	newYAML, _ := data["new"].(string)
	if len(newYAML) > maxConfigMapValueBytes*2 {
		t.Errorf("rendered YAML not capped: %d bytes", len(newYAML))
	}
	if !strings.Contains(newYAML, "truncated") {
		t.Error("truncation must be stated in the evidence, not silent")
	}

	values, _ := data["updated_values"].([]any)
	if len(values) != 1 {
		t.Fatalf("expected 1 updated_value; got %d", len(values))
	}
	entry, _ := values[0].(map[string]any)
	after, _ := entry["new"].(string)
	if len(after) > maxConfigMapValueBytes*2 {
		t.Errorf("updated_values entry not capped: %d bytes", len(after))
	}
	// The path is what identifies the change — it must never be cut.
	if entry["path"] != "data.app.py" {
		t.Errorf("path = %v; want data.app.py", entry["path"])
	}
}

// ConfigMaps routinely hold UTF-8 — a properties file with accented text,
// a message catalogue. Cutting at a byte boundary can split a rune, and the
// half character then travels through JSON and YAML encoding into the UI.
func TestConfigMapChange_TruncationCutsAtARuneBoundary(t *testing.T) {
	// "é" is two bytes, so a run of them puts a rune boundary at every
	// second byte and guarantees the cap lands mid-character for one of
	// these two lengths.
	for _, pad := range []int{0, 1} {
		body := strings.Repeat("a", pad) + strings.Repeat("é", maxConfigMapValueBytes)
		old := mustObj(t, `{"metadata":{"name":"c","namespace":"prod"},"data":{"k":"short"}}`)
		updated := mustObj(t, `{"metadata":{"name":"c","namespace":"prod"},"data":{"k":"`+body+`"}}`)

		blocks := configMapChangeMatcher().EnrichBlocks(updated, old, EnrichContext{})
		data, _ := blocks[0]["data"].(map[string]any)
		values, _ := data["updated_values"].([]any)
		entry, _ := values[0].(map[string]any)
		after, _ := entry["new"].(string)

		if !utf8.ValidString(after) {
			t.Errorf("pad=%d: truncated value is not valid UTF-8", pad)
		}
		newYAML, _ := data["new"].(string)
		if !utf8.ValidString(newYAML) {
			t.Errorf("pad=%d: rendered YAML is not valid UTF-8", pad)
		}
	}
}

func TestConfigMapChange_DropsLastAppliedAnnotationFromRenderedYAML(t *testing.T) {
	// kubectl's copy of the whole object. Rendered inside a diff of that
	// same object it doubles the payload and shows the change twice.
	old := mustObj(t, `{
		"metadata":{"name":"c","namespace":"prod","annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"k\":\"v1\"}}","owner":"platform"}},
		"data":{"k":"v1"}
	}`)
	updated := mustObj(t, `{
		"metadata":{"name":"c","namespace":"prod","annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"k\":\"v2\"}}","owner":"platform"}},
		"data":{"k":"v2"}
	}`)
	blocks := configMapChangeMatcher().EnrichBlocks(updated, old, EnrichContext{})
	data, _ := blocks[0]["data"].(map[string]any)
	newYAML, _ := data["new"].(string)
	if strings.Contains(newYAML, "last-applied-configuration") {
		t.Errorf("last-applied annotation leaked into rendered YAML: %s", newYAML)
	}
	if !strings.Contains(newYAML, "platform") {
		t.Errorf("other annotations must survive: %s", newYAML)
	}
	// The caller's map must not be mutated — the engine hands the same
	// object to every matcher it evaluates.
	meta, _ := updated["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	if _, present := ann["kubectl.kubernetes.io/last-applied-configuration"]; !present {
		t.Error("enrichment mutated the caller's object")
	}
}

func TestConfigMapChange_FingerprintIdentity(t *testing.T) {
	m := configMapChangeMatcher()
	cm := func(ns, name, rv string) map[string]any {
		return mustObj(t, `{"metadata":{"name":"`+name+`","namespace":"`+ns+`","resourceVersion":"`+rv+`"},"data":{"k":"v"}}`)
	}

	// Repeat edits to one ConfigMap chain into a single recurring entry.
	if m.FingerprintFn(cm("sessions", "sessions-config", "1")) != m.FingerprintFn(cm("sessions", "sessions-config", "9")) {
		t.Error("edits to the same ConfigMap must share a fingerprint")
	}
	// Two ConfigMaps in one namespace stay distinct — otherwise the real
	// change and a decoy edited seconds apart collapse into one entry.
	if m.FingerprintFn(cm("sessions", "sessions-config", "1")) == m.FingerprintFn(cm("sessions", "sessions-worker-config", "1")) {
		t.Error("different ConfigMaps must not share a fingerprint")
	}
	// A ConfigMap and a Deployment of the same name in the same namespace
	// is an extremely common pairing (chart name used for both).
	deployFP := babysitterChangeMatcher("Deployment").FingerprintFn(cm("sessions", "sessions-api", "1"))
	if m.FingerprintFn(cm("sessions", "sessions-api", "1")) == deployFP {
		t.Error("ConfigMap and Deployment of the same name must not share a fingerprint")
	}
}

// kubewatch's per-resource handlers are not consistent about kind casing,
// and a spec that never matches is invisible — the events just land in
// the unmatched counter.
func TestEngine_MatchesConfigMapKindRegardlessOfCase(t *testing.T) {
	for _, kind := range []string{"ConfigMap", "configmap", "configMap"} {
		t.Run(kind, func(t *testing.T) {
			eng := NewEngine(Builtins(), time.Now())
			matches := eng.Match(IncomingK8sEvent{
				Operation: "update",
				Kind:      kind,
				Obj:       mustObj(t, sessionsConfigAfter),
				OldObj:    mustObj(t, sessionsConfigBefore),
			})
			if len(matches) != 1 {
				t.Fatalf("expected 1 match for kind %q; got %d", kind, len(matches))
			}
			if matches[0].SubjectKind != "configmap" {
				t.Errorf("subject kind = %q; want configmap (lowercased for the backend)", matches[0].SubjectKind)
			}
			if matches[0].SubjectName != "sessions-config" || matches[0].SubjectNamespace != "sessions" {
				t.Errorf("subject = %s/%s; want sessions/sessions-config",
					matches[0].SubjectNamespace, matches[0].SubjectName)
			}
		})
	}
}

// System namespaces hold the platform's own moving parts — an autoscaler's
// status, a webhook heartbeat — which are state in a ConfigMap, not config.
func TestConfigMapChange_SkipsSystemNamespaces(t *testing.T) {
	mk := func(ns, name, v string) map[string]any {
		return mustObj(t, `{"metadata":{"name":"`+name+`","namespace":"`+ns+`"},"data":{"k":"`+v+`"}}`)
	}
	for _, ns := range []string{"kube-system", "kube-public", "kube-node-lease", "gmp-system"} {
		if configMapChangeMatcher().Predicate(mk(ns, "some-status", "b"), mk(ns, "some-status", "a")) {
			t.Errorf("%s must be excluded by default", ns)
		}
	}
	if !configMapChangeMatcher().Predicate(mk("payments", "app-config", "b"), mk("payments", "app-config", "a")) {
		t.Error("an ordinary namespace must still be tracked")
	}
}

// Excluding kube-system wholesale would drop the highest-consequence config in
// the cluster with the noise: coredns decides name resolution, aws-auth decides
// who can reach the API at all.
func TestConfigMapChange_TracksCriticalSystemConfigMaps(t *testing.T) {
	mk := func(name, v string) map[string]any {
		return mustObj(t, `{"metadata":{"name":"`+name+`","namespace":"kube-system"},"data":{"Corefile":"`+v+`"}}`)
	}
	for _, name := range []string{"coredns", "aws-auth", "kube-proxy"} {
		if !configMapChangeMatcher().Predicate(mk(name, "b"), mk(name, "a")) {
			t.Errorf("%s must be reported despite living in kube-system", name)
		}
	}
}

// The matcher must opt into frequency suppression; without it the namespace
// filter alone leaves the churn in other namespaces (datadog-token produced 46
// changes in 30 minutes from the `datadog` namespace).
func TestConfigMapChange_OptsIntoChurnSuppression(t *testing.T) {
	if !configMapChangeMatcher().SuppressChurn {
		t.Error("ConfigMap changes must opt into churn suppression")
	}
	if babysitterChangeMatcher("Deployment").SuppressChurn {
		t.Error("workload changes do not churn and must not be suppressed by frequency")
	}
}

// flagsFile builds a pretty-printed flags file held in one ConfigMap key,
// with flag "featureN" for each n. Flags late in the file sit well past
// maxConfigMapValueBytes.
func flagsFile(n int, override map[string]string) string {
	var sb strings.Builder
	sb.WriteString("{\n  \"flags\": {\n")
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("feature%02d", i)
		v := "off"
		if o, ok := override[name]; ok {
			v = o
		}
		fmt.Fprintf(&sb, "    %q: {\n      \"defaultVariant\": %q,\n      \"state\": \"ENABLED\",\n      \"variants\": {\n        \"off\": false,\n        \"on\": true\n      }\n    },\n", name, v)
	}
	sb.WriteString("    \"last\": {}\n  }\n}\n")
	return sb.String()
}

func configMapWith(t *testing.T, key, value string) map[string]any {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"name": "flags", "namespace": "prod"},
		"data":     map[string]any{key: value},
	})
	if err != nil {
		t.Fatal(err)
	}
	return mustObj(t, string(b))
}

func diffEntry(t *testing.T, blocks []EvidenceBlock) (oldYAML, newYAML, before, after string) {
	t.Helper()
	if len(blocks) != 1 {
		t.Fatalf("expected 1 evidence block; got %d", len(blocks))
	}
	data, _ := blocks[0]["data"].(map[string]any)
	values, _ := data["updated_values"].([]any)
	if len(values) != 1 {
		t.Fatalf("expected 1 updated_value; got %d", len(values))
	}
	entry, _ := values[0].(map[string]any)
	oldYAML, _ = data["old"].(string)
	newYAML, _ = data["new"].(string)
	before, _ = entry["old"].(string)
	after, _ = entry["new"].(string)
	return
}

// An edit past the cap used to leave two identical head excerpts: the
// event said the value changed and showed nothing that did. Both sides now
// keep the changed lines, framed by enough context to name what changed.
func TestConfigMapChange_KeepsAnEditPastTheCap(t *testing.T) {
	before := flagsFile(40, nil)
	after := flagsFile(40, map[string]string{"feature30": "on"})
	if i := strings.Index(before, `"feature30"`); i < maxConfigMapValueBytes {
		t.Fatalf("fixture must put the edit past the cap; found at %d", i)
	}

	blocks := configMapChangeMatcher().EnrichBlocks(configMapWith(t, "flags.json", after), configMapWith(t, "flags.json", before), EnrichContext{})
	oldYAML, newYAML, b, a := diffEntry(t, blocks)

	for _, side := range []struct{ name, text, want string }{
		{"updated_values old", b, `"defaultVariant": "off"`},
		{"updated_values new", a, `"defaultVariant": "on"`},
		{"rendered old", oldYAML, `"defaultVariant": "off"`},
		{"rendered new", newYAML, `"defaultVariant": "on"`},
	} {
		if !strings.Contains(side.text, `"feature30": {`) || !strings.Contains(side.text, side.want) {
			t.Errorf("%s must carry the edited flag and its value:\n%s", side.name, side.text)
		}
		if !strings.Contains(side.text, "truncated: showing lines") {
			t.Errorf("%s must say it is an excerpt", side.name)
		}
	}
	if b == a {
		t.Error("the two excerpts must differ — they are the change")
	}
	if len(b) > maxConfigMapValueBytes || len(a) > maxConfigMapValueBytes {
		t.Errorf("excerpts over the cap: %d / %d bytes", len(b), len(a))
	}
}

func TestConfigMapChange_KeepsEachEditedRange(t *testing.T) {
	before := flagsFile(60, nil)
	after := flagsFile(60, map[string]string{"feature05": "on", "feature50": "on"})

	blocks := configMapChangeMatcher().EnrichBlocks(configMapWith(t, "flags.json", after), configMapWith(t, "flags.json", before), EnrichContext{})
	_, _, b, a := diffEntry(t, blocks)
	if strings.Count(a, "truncated: showing lines") != 2 || strings.Count(b, "truncated: showing lines") != 2 {
		t.Fatalf("two distant edits must be two ranges:\nold: %s\nnew: %s", b, a)
	}
	for _, name := range []string{`"feature05": {`, `"feature50": {`} {
		if !strings.Contains(a, name) || !strings.Contains(b, name) {
			t.Errorf("both sides must keep %s", name)
		}
	}
	if strings.Contains(a, `"feature25"`) {
		t.Error("the unchanged middle of the file must be left out")
	}
}

// One long line has no line ranges to keep; the excerpt opens just before
// the first byte that differs instead of at the head.
func TestConfigMapChange_OneLineValueKeepsTheFirstDifference(t *testing.T) {
	head := strings.Repeat("x", maxConfigMapValueBytes*2)
	blocks := configMapChangeMatcher().EnrichBlocks(
		configMapWith(t, "blob", head+"NEW"+strings.Repeat("y", 100)),
		configMapWith(t, "blob", head+"OLD"+strings.Repeat("y", 100)), EnrichContext{})
	_, _, b, a := diffEntry(t, blocks)
	if !strings.Contains(b, "OLD") || !strings.Contains(a, "NEW") {
		t.Errorf("excerpts must hold the differing bytes:\nold: %.120s…\nnew: %.120s…", b, a)
	}
	if !strings.Contains(a, "truncated: showing bytes") {
		t.Error("a byte excerpt must say so")
	}
}

// Only a changed value gets a change window. An over-cap key that did not
// change is still cut to its head, and a key that is new has no old side
// to align with.
func TestConfigMapChange_UnchangedAndAddedKeysKeepTheHeadCap(t *testing.T) {
	big := strings.Repeat("a\n", maxConfigMapValueBytes)
	old := mustObj(t, `{"metadata":{"name":"c","namespace":"prod"},"data":{"same":"`+strings.ReplaceAll(big, "\n", `\n`)+`","k":"1"}}`)
	updated := mustObj(t, `{"metadata":{"name":"c","namespace":"prod"},"data":{"same":"`+strings.ReplaceAll(big, "\n", `\n`)+`","k":"2","added":"`+strings.ReplaceAll(big, "\n", `\n`)+`"}}`)

	windows := changedValueWindows(old, updated)
	if len(windows) != 0 {
		t.Fatalf("no changed over-cap value here; got windows for %v", windows)
	}
	blocks := configMapChangeMatcher().EnrichBlocks(updated, old, EnrichContext{})
	data, _ := blocks[0]["data"].(map[string]any)
	newYAML, _ := data["new"].(string)
	if !strings.Contains(newYAML, "truncated: 2048 of") {
		t.Errorf("unchanged and added over-cap values keep the head cap:\n%.300s", newYAML)
	}
}

func TestConfigMapChange_ChangeWindowsCutOnRuneBoundaries(t *testing.T) {
	for _, pad := range []int{0, 1} {
		head := strings.Repeat("a", pad) + strings.Repeat("é", maxConfigMapValueBytes)
		b, a := changeWindows(head+"1", head+"2")
		if !utf8.ValidString(b) || !utf8.ValidString(a) {
			t.Errorf("pad=%d: excerpt is not valid UTF-8", pad)
		}
	}
}

// Many scattered edits to a large value with repeated lines made the line
// matcher take minutes. Past maxMatchedLines the changed span is one range,
// which the byte window then cuts down.
func TestConfigMapChange_LargeValueWithManyEditsStaysFast(t *testing.T) {
	var b, a strings.Builder
	for i := 0; i < 30000; i++ {
		fmt.Fprintf(&b, "    \"k%d\": %d,\n  },\n", i, i)
		v := i
		if i%97 == 0 {
			v = -i
		}
		fmt.Fprintf(&a, "    \"k%d\": %d,\n  },\n", i, v)
	}
	start := time.Now()
	before, after := changeWindows(b.String(), a.String())
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("changeWindows took %s on a %d-byte value", d, b.Len())
	}
	if before == after || len(before) > maxConfigMapValueBytes+64 || len(after) > maxConfigMapValueBytes+64 {
		t.Errorf("excerpts must differ and stay near the cap: %d / %d bytes", len(before), len(after))
	}
}
