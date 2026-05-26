package triggers

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/nudgebee/nudgebee-agent/pkg/kube"
)

// DiffEntry is one path-level change between two K8s objects. `Path` is
// dot-separated (e.g. "spec.replicas", "spec.template.spec.containers[0].image").
// Before/After are the two values; either can be nil for added / removed paths.
type DiffEntry struct {
	Path   string `json:"path"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
}

// SpecDiffOptions controls field filtering in ComputeSpecDiff. Defaults
// mirror the BabysitterConfig: monitor "spec" (and any sub-path), omit
// metadata noise + status churn that updates on every controller
// heartbeat.
type SpecDiffOptions struct {
	// FieldsToMonitor is the set of top-level (or dotted) prefixes a
	// diff path must match to be reported. A path "matches" when it
	// either equals or starts with one of these followed by "." or "[".
	FieldsToMonitor []string

	// FieldsToOmit overrides FieldsToMonitor — paths matching any of
	// these are dropped even if they also match FieldsToMonitor.
	FieldsToOmit []string
}

// DefaultSpecDiffOptions returns the babysitter defaults — exact parity
// with BabysitterConfig.fields_to_monitor / omitted_fields.
func DefaultSpecDiffOptions() SpecDiffOptions {
	return SpecDiffOptions{
		FieldsToMonitor: []string{"spec", "spec.replicas"},
		FieldsToOmit: []string{
			"status",
			"metadata.generation",
			"metadata.resourceVersion",
			"metadata.managedFields",
		},
	}
}

// ComputeSpecDiff walks obj + oldObj recursively and returns the filtered
// path-level diffs. Returns nil when oldObj is nil (no diff possible) or
// when no monitored field changed.
//
// Implementation: marshals both objects to JSON to get deterministic key
// order, then walks the parsed maps. We don't use reflect because both
// inputs are already `map[string]any` from JSON unmarshalling (kubewatch
// payload).
func ComputeSpecDiff(obj, oldObj map[string]any, opt SpecDiffOptions) []DiffEntry {
	if oldObj == nil || obj == nil {
		return nil
	}
	if len(opt.FieldsToMonitor) == 0 {
		opt = DefaultSpecDiffOptions()
	}
	var diffs []DiffEntry
	walkDiff("", oldObj, obj, &diffs)
	if len(diffs) == 0 {
		return nil
	}
	return filterDiffs(diffs, opt)
}

// walkDiff appends one DiffEntry per leaf-level difference between
// before and after. Compares maps by union of keys, slices by index.
func walkDiff(path string, before, after any, out *[]DiffEntry) {
	if jsonEqual(before, after) {
		return
	}
	bMap, bIsMap := before.(map[string]any)
	aMap, aIsMap := after.(map[string]any)
	if bIsMap && aIsMap {
		// Union of keys, deterministic order for stable diff output.
		keys := unionKeys(bMap, aMap)
		for _, k := range keys {
			walkDiff(joinPath(path, k), bMap[k], aMap[k], out)
		}
		return
	}
	bArr, bIsArr := before.([]any)
	aArr, aIsArr := after.([]any)
	if bIsArr && aIsArr {
		// Compare by index. K8s array semantics differ (containers are
		// keyed by name, but volumeMounts by index, etc.); the legacy
		// hikaru-driven diff sidestepped this — we walk by index too.
		// The result is "noisier" for reorderings but is correct
		// (every leaf change still shows up).
		max := len(bArr)
		if len(aArr) > max {
			max = len(aArr)
		}
		for i := 0; i < max; i++ {
			var bi, ai any
			if i < len(bArr) {
				bi = bArr[i]
			}
			if i < len(aArr) {
				ai = aArr[i]
			}
			walkDiff(fmt.Sprintf("%s[%d]", path, i), bi, ai, out)
		}
		return
	}
	// Scalar (or type changed) — emit one diff entry.
	*out = append(*out, DiffEntry{Path: path, Before: before, After: after})
}

// filterDiffs keeps entries whose path matches at least one
// FieldsToMonitor prefix AND none of the FieldsToOmit prefixes.
func filterDiffs(diffs []DiffEntry, opt SpecDiffOptions) []DiffEntry {
	out := diffs[:0]
	for _, d := range diffs {
		if !pathMatchesAny(d.Path, opt.FieldsToMonitor) {
			continue
		}
		if pathMatchesAny(d.Path, opt.FieldsToOmit) {
			continue
		}
		out = append(out, d)
	}
	return out
}

func pathMatchesAny(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+".") || strings.HasPrefix(path, p+"[") {
			return true
		}
	}
	return false
}

// jsonEqual compares two JSON-shaped values via canonical-marshal +
// bytes-equal. Robust against map-key ordering differences in the input.
func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func unionKeys(a, b map[string]any) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// BuildKubernetesDiffBlock returns the `type: "diff"` evidence block the
// UI's CodeMirrorDiffViewer
// (app/src/components1/k8s/common/KubernetesTable2.jsx:1184-1199) reads
// to render the old/new YAML side-by-side.
//
// `kind` is used to build resource_name; pass the K8s kind as kubewatch
// reports it ("Deployment", "DaemonSet", ...).
func BuildKubernetesDiffBlock(obj, oldObj map[string]any, kind string, diffs []DiffEntry) EvidenceBlock {
	// Strip a small set of "noisy" fields before serializing
	// (status, metadata.generation/resourceVersion/managedFields). Same
	// fields the spec-diff filter omits — single source of truth.
	omit := DefaultSpecDiffOptions().FieldsToOmit

	objYAML := objectToYAML(obj, omit)
	oldYAML := objectToYAML(oldObj, omit)

	// resource_name follows the _obj_to_name convention:
	// "<kind>/<namespace>/<name>.yaml" (kind lowercased, namespace
	// elided when empty).
	name, namespace := metaName(obj), metaNS(obj)
	if name == "" {
		name, namespace = metaName(oldObj), metaNS(oldObj)
	}
	var resourceName string
	if k := strings.ToLower(kind); k != "" {
		resourceName = k + "/"
	}
	if namespace != "" {
		resourceName += namespace + "/"
	}
	resourceName += name + ".yaml"

	// updated_paths / updated_values carry per-diff entries.
	// We don't classify diffs as added/removed/modified, so
	// num_additions/num_deletions stay 0 and the count goes into
	// num_modifications. The UI displays the YAML diff regardless.
	paths := make([]any, 0, len(diffs))
	values := make([]any, 0, len(diffs))
	for _, d := range diffs {
		paths = append(paths, d.Path)
		values = append(values, map[string]any{
			"path":   d.Path,
			"old":    d.Before,
			"new":    d.After,
			"report": "",
		})
	}

	return EvidenceBlock{
		"type": "diff",
		"data": map[string]any{
			"old":               oldYAML,
			"new":               objYAML,
			"resource_name":     resourceName,
			"num_additions":     0,
			"num_deletions":     0,
			"num_modifications": len(diffs),
			"updated_paths":     paths,
			"updated_values":    values,
		},
		"additional_info": nil,
	}
}

// objectToYAML returns YAML for a K8s object: omitted fields stripped,
// keys snake-cased to match Hikaru's output (the wire shape historically
// produced via hikaru.get_yaml).
func objectToYAML(obj map[string]any, omit []string) string {
	if obj == nil {
		return ""
	}
	stripped := stripOmittedFields(obj, omit)
	snaked := kube.SnakeKeysDeep(stripped)
	b, err := yaml.Marshal(snaked)
	if err != nil {
		return ""
	}
	return string(b)
}

// stripOmittedFields returns a deep copy of obj with `omit` paths
// removed (top-level keys + dotted sub-paths). Used by the diff-block
// builder so the YAML the UI renders matches the
// duplicate_without_fields output.
func stripOmittedFields(obj map[string]any, omit []string) map[string]any {
	if obj == nil {
		return nil
	}
	// Cheap deep copy via JSON round-trip — obj is already JSON-shaped
	// from the kubewatch payload, so this is fast and avoids reaching
	// for reflect.
	b, err := json.Marshal(obj)
	if err != nil {
		return obj
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return obj
	}
	for _, path := range omit {
		deletePath(out, strings.Split(path, "."))
	}
	return out
}

func deletePath(m map[string]any, parts []string) {
	if len(parts) == 0 || m == nil {
		return
	}
	if len(parts) == 1 {
		delete(m, parts[0])
		return
	}
	next, _ := m[parts[0]].(map[string]any)
	if next == nil {
		return
	}
	deletePath(next, parts[1:])
}

// RenderDiffMarkdown turns a slice of DiffEntry into a small Markdown
// table — one row per changed path. Kept for tests and any caller that
// wants a human-readable summary; the babysitter Finding now ships a
// `type: "diff"` block (BuildKubernetesDiffBlock) so the UI can render
// the full old/new YAML side-by-side.
func RenderDiffMarkdown(diffs []DiffEntry) string {
	if len(diffs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("**Changed fields:**\n\n")
	b.WriteString("| Path | Before | After |\n")
	b.WriteString("|---|---|---|\n")
	for _, d := range diffs {
		b.WriteString("| `")
		b.WriteString(d.Path)
		b.WriteString("` | ")
		b.WriteString(renderValueCell(d.Before))
		b.WriteString(" | ")
		b.WriteString(renderValueCell(d.After))
		b.WriteString(" |\n")
	}
	return b.String()
}

// renderValueCell formats a value for a single Markdown table cell.
// Long values are truncated; nil renders as the literal "(unset)".
func renderValueCell(v any) string {
	if v == nil {
		return "_(unset)_"
	}
	bytes, _ := json.Marshal(v)
	s := string(bytes)
	// Clamp absurd payloads (large nested specs) so the cell stays
	// inspectable. Full diff is in the JSON evidence block.
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	// Markdown table cells can't contain unescaped pipes.
	return strings.ReplaceAll(s, "|", `\|`)
}
