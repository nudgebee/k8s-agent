package triggers

import (
	"strings"
	"testing"
)

// TestComputeSpecDiff_PicksUpReplicaChange exercises the headline
// babysitter case: a Deployment had its replicas bumped from 2 to 5.
// One DiffEntry expected at path "spec.replicas".
func TestComputeSpecDiff_PicksUpReplicaChange(t *testing.T) {
	old := mustObj(t, `{"metadata":{"name":"d"},"spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","image":"v1"}]}}}}`)
	new := mustObj(t, `{"metadata":{"name":"d"},"spec":{"replicas":5,"template":{"spec":{"containers":[{"name":"app","image":"v1"}]}}}}`)
	diffs := ComputeSpecDiff(new, old, DefaultSpecDiffOptions())
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff; got %d: %+v", len(diffs), diffs)
	}
	if diffs[0].Path != "spec.replicas" {
		t.Errorf("path = %q; want spec.replicas", diffs[0].Path)
	}
	if diffs[0].Before.(float64) != 2 || diffs[0].After.(float64) != 5 {
		t.Errorf("before/after = %v/%v; want 2/5", diffs[0].Before, diffs[0].After)
	}
}

// TestComputeSpecDiff_OmitsManagedFieldsAndStatus covers
// BabysitterConfig.omitted_fields. Without this filter, every kubectl-
// apply (which mutates managedFields) and every controller heartbeat
// (which mutates status) would fire a Finding.
func TestComputeSpecDiff_OmitsManagedFieldsAndStatus(t *testing.T) {
	old := mustObj(t, `{
		"metadata":{"name":"d","resourceVersion":"100","generation":1,"managedFields":[{"manager":"old"}]},
		"spec":{"replicas":2},
		"status":{"replicas":2,"availableReplicas":2}
	}`)
	new := mustObj(t, `{
		"metadata":{"name":"d","resourceVersion":"101","generation":2,"managedFields":[{"manager":"new"}]},
		"spec":{"replicas":2},
		"status":{"replicas":1,"availableReplicas":1}
	}`)
	diffs := ComputeSpecDiff(new, old, DefaultSpecDiffOptions())
	if len(diffs) != 0 {
		t.Errorf("expected 0 diffs (only metadata + status changed); got %+v", diffs)
	}
}

// TestComputeSpecDiff_NestedSpecChange — a container image bump deep
// inside spec.template.spec.containers[0].image must show up.
func TestComputeSpecDiff_NestedSpecChange(t *testing.T) {
	old := mustObj(t, `{"metadata":{"name":"d"},"spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","image":"v1"}]}}}}`)
	new := mustObj(t, `{"metadata":{"name":"d"},"spec":{"replicas":2,"template":{"spec":{"containers":[{"name":"app","image":"v2"}]}}}}`)
	diffs := ComputeSpecDiff(new, old, DefaultSpecDiffOptions())
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff; got %d", len(diffs))
	}
	if diffs[0].Path != "spec.template.spec.containers[0].image" {
		t.Errorf("path = %q; want spec.template.spec.containers[0].image", diffs[0].Path)
	}
}

func TestComputeSpecDiff_NoDiffOnNoChange(t *testing.T) {
	obj := mustObj(t, `{"metadata":{"name":"d"},"spec":{"replicas":2}}`)
	if d := ComputeSpecDiff(obj, obj, DefaultSpecDiffOptions()); len(d) != 0 {
		t.Errorf("identical objects must produce no diffs; got %+v", d)
	}
}

// TestRenderDiffMarkdown_StableFormat — the renderer must produce a
// table the UI's markdown renderer can parse. Smoke test the
// header rows + value cells.
func TestRenderDiffMarkdown_StableFormat(t *testing.T) {
	md := RenderDiffMarkdown([]DiffEntry{
		{Path: "spec.replicas", Before: float64(2), After: float64(5)},
	})
	for _, want := range []string{"**Changed fields:**", "| Path | Before | After |", "| `spec.replicas` |", " | 2 | 5 |"} {
		if !strings.Contains(md, want) {
			t.Errorf("rendered markdown missing %q\n%s", want, md)
		}
	}
}

func TestRenderDiffMarkdown_HandlesNilUnsetSide(t *testing.T) {
	md := RenderDiffMarkdown([]DiffEntry{
		{Path: "spec.tolerations", Before: nil, After: []any{}},
	})
	if !strings.Contains(md, "_(unset)_") {
		t.Errorf("nil side must render as (unset); got: %s", md)
	}
}

// ---------- babysitter matcher integration ----------

func TestBabysitter_FiresOnReplicaChange(t *testing.T) {
	old := mustObj(t, `{"metadata":{"name":"d","namespace":"prod","resourceVersion":"100"},"spec":{"replicas":2}}`)
	new := mustObj(t, `{"metadata":{"name":"d","namespace":"prod","resourceVersion":"101"},"spec":{"replicas":5}}`)
	m := babysitterChangeMatcher("Deployment")
	if !m.Predicate(new, old) {
		t.Fatal("predicate must fire on spec.replicas change")
	}
	if m.AggregationKey != "ConfigurationChange/KubernetesResource/Change" {
		t.Errorf("aggregation_key = %q; want ConfigurationChange/KubernetesResource/Change", m.AggregationKey)
	}
}

func TestBabysitter_DoesNotFireOnStatusOnlyChange(t *testing.T) {
	old := mustObj(t, `{"metadata":{"name":"d","namespace":"prod"},"spec":{"replicas":2},"status":{"availableReplicas":2}}`)
	new := mustObj(t, `{"metadata":{"name":"d","namespace":"prod"},"spec":{"replicas":2},"status":{"availableReplicas":1}}`)
	if babysitterChangeMatcher("Deployment").Predicate(new, old) {
		t.Error("status-only change must not fire babysitter")
	}
}

func TestBabysitter_DoesNotFireOnCreate(t *testing.T) {
	// Predicate receives nil oldObj on CREATE — nothing to diff against.
	new := mustObj(t, `{"metadata":{"name":"d","namespace":"prod"},"spec":{"replicas":2}}`)
	if babysitterChangeMatcher("Deployment").Predicate(new, nil) {
		t.Error("babysitter must not fire on CREATE (no oldObj)")
	}
}

func TestBabysitter_EnrichBlocksAttachesDiffBlock(t *testing.T) {
	// UI's KubernetesTable2.jsx:1184-1199 filters evidence blocks for
	// type==='diff' and feeds data.old / data.new into CodeMirrorDiffViewer.
	// Anything else (e.g. "markdown") shows "No diff available."
	old := mustObj(t, `{"metadata":{"name":"d","namespace":"prod","resourceVersion":"99"},"spec":{"replicas":2}}`)
	new := mustObj(t, `{"metadata":{"name":"d","namespace":"prod","resourceVersion":"100"},"spec":{"replicas":5}}`)
	m := babysitterChangeMatcher("Deployment")
	blocks := m.EnrichBlocks(new, old, EnrichContext{})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 evidence block; got %d", len(blocks))
	}
	b := blocks[0]
	if b["type"] != "diff" {
		t.Fatalf("block type = %v; want diff", b["type"])
	}
	data, _ := b["data"].(map[string]any)
	if data == nil {
		t.Fatalf("block.data missing or wrong type: %v", b["data"])
	}
	oldYAML, _ := data["old"].(string)
	newYAML, _ := data["new"].(string)
	if !strings.Contains(oldYAML, "replicas: 2") {
		t.Errorf("data.old missing 'replicas: 2': %s", oldYAML)
	}
	if !strings.Contains(newYAML, "replicas: 5") {
		t.Errorf("data.new missing 'replicas: 5': %s", newYAML)
	}
	if data["resource_name"] != "deployment/prod/d.yaml" {
		t.Errorf("resource_name = %v; want deployment/prod/d.yaml", data["resource_name"])
	}
	// metadata.resourceVersion is in the omit list — must NOT leak into
	// the rendered YAML; otherwise every Finding shows the bumped rv as
	// a "change" the user has to ignore.
	if strings.Contains(newYAML, "resourceVersion") || strings.Contains(newYAML, "resource_version") {
		t.Errorf("resourceVersion leaked into YAML: %s", newYAML)
	}
	paths, _ := data["updated_paths"].([]any)
	if len(paths) == 0 {
		t.Error("updated_paths must list the changed paths")
	}
	values, _ := data["updated_values"].([]any)
	if len(values) == 0 {
		t.Error("updated_values must carry the path/old/new triples")
	}
}

func TestBabysitter_FingerprintIncludesResourceVersion(t *testing.T) {
	// Two distinct spec changes (different resourceVersions) → two
	// distinct fingerprints → two Findings, not deduped to one.
	mk := func(rv string, replicas int) map[string]any {
		return mustObj(t, `{"metadata":{"name":"d","namespace":"prod","resourceVersion":"`+rv+`"},"spec":{"replicas":`+itoa(replicas)+`}}`)
	}
	m := babysitterChangeMatcher("Deployment")
	if m.FingerprintFn(mk("100", 2)) == m.FingerprintFn(mk("101", 5)) {
		t.Error("different resourceVersions must produce distinct fingerprints")
	}
}

// ---------- helpers ----------

func mustObj(t *testing.T, s string) map[string]any { t.Helper(); return asObj(t, s) }
