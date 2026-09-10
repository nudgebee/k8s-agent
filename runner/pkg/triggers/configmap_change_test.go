package triggers

import (
	"strings"
	"testing"
	"time"
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
