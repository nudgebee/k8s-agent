package mutate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// revert.go implements `revert_workload`: undo a recorded configuration
// change by writing the previous value back at each changed field path.
//
// Why this is NOT replace_workload with the pre-change manifest: the diff
// evidence the api-server holds is serialized snake_case (Hikaru wire
// compat — see triggers.objectToYAML), so replaying it through the dynamic
// client makes the apiserver silently drop every renamed key and reject the
// remains ("spec.selector: empty selector is invalid for deployment").
// Replaying a snapshot is also wrong on its own terms: it clobbers every
// unrelated change made since the event and trips immutable-field
// validation. So we read the live object and write back only the fields the
// change actually touched.
const revertConflictRetries = 3

// RevertEntry is one recorded field change to undo. Path is the field path
// as it appears in the diff evidence's updated_values; Old is the value the
// field held before the change. HasOld is false when the change ADDED the
// field (evidence records `old: null`) — reverting then means removing it.
type RevertEntry struct {
	Path   string
	Old    any
	HasOld bool
}

// RevertWorkload reads the live workload, applies each RevertEntry to it,
// and writes it back. The read-modify-write is retried on conflict so a
// racing controller reconcile doesn't surface as a user-visible failure.
func (m *Mutator) RevertWorkload(ctx context.Context, kind, namespace, name string, entries []RevertEntry) (any, error) {
	if m.dynamic == nil {
		return nil, errors.New("mutate: dynamic client not configured")
	}
	if name == "" {
		return nil, errors.New("mutate: name required")
	}
	if len(entries) == 0 {
		return nil, errors.New("mutate: revert_paths required")
	}
	canonical, entry, ok := resolveWorkloadKind(kind)
	if !ok {
		return nil, fmt.Errorf("mutate: revert_workload not supported for kind %q", kind)
	}
	if entry.namespaced && namespace == "" {
		return nil, fmt.Errorf("mutate: namespace required for %s", canonical)
	}

	ri := m.dynamic.Resource(entry.gvr)
	var updated *unstructured.Unstructured
	for attempt := 0; ; attempt++ {
		var live *unstructured.Unstructured
		var err error
		if entry.namespaced {
			live, err = ri.Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		} else {
			live, err = ri.Get(ctx, name, metav1.GetOptions{})
		}
		if err != nil {
			return nil, fmt.Errorf("mutate: get existing %s/%s: %w", canonical, name, err)
		}

		for _, e := range entries {
			if err := applyRevert(live.Object, e); err != nil {
				return nil, fmt.Errorf("mutate: revert %s/%s: %w", canonical, name, err)
			}
		}

		if entry.namespaced {
			updated, err = ri.Namespace(namespace).Update(ctx, live, metav1.UpdateOptions{})
		} else {
			updated, err = ri.Update(ctx, live, metav1.UpdateOptions{})
		}
		if err == nil {
			break
		}
		if apierrors.IsConflict(err) && attempt < revertConflictRetries-1 {
			continue // re-read and re-apply against the newer object
		}
		return nil, fmt.Errorf("mutate: revert %s/%s: %w", canonical, name, err)
	}

	paths := make([]any, 0, len(entries))
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	loc := name
	if namespace != "" {
		loc = namespace + "/" + name
	}
	return map[string]any{
		"updated":        updated.UnstructuredContent(),
		"reverted_paths": paths,
		"message":        fmt.Sprintf("%s/%s reverted (%s)", canonical, loc, pluralFields(len(entries))),
	}, nil
}

func pluralFields(n int) string {
	if n == 1 {
		return "1 field"
	}
	return fmt.Sprintf("%d fields", n)
}

// applyRevert writes one RevertEntry into the (live) object.
func applyRevert(root map[string]any, e RevertEntry) error {
	steps, err := tokenizePath(e.Path, root)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return fmt.Errorf("path %q is empty", e.Path)
	}
	last := steps[len(steps)-1]

	// Intermediate containers are created only when we are writing a value:
	// removing a field whose parent is already gone is a no-op, not an error.
	parent, err := navigate(root, steps[:len(steps)-1], e.HasOld)
	if err != nil {
		if !e.HasOld {
			return nil
		}
		return fmt.Errorf("path %q: %w", e.Path, err)
	}

	if last.isIndex {
		arr, ok := parent.([]any)
		if !ok {
			return fmt.Errorf("path %q: [%d] applied to a non-array", e.Path, last.index)
		}
		if !e.HasOld {
			// The change appended an element. Dropping it by index would
			// renumber the rest of the array and silently corrupt sibling
			// paths in the same revert, so refuse rather than guess.
			return fmt.Errorf("path %q: cannot revert an added array element automatically", e.Path)
		}
		if last.index < 0 || last.index >= len(arr) {
			return fmt.Errorf("path %q: index %d out of range (len %d)", e.Path, last.index, len(arr))
		}
		arr[last.index] = normalizeJSONValue(e.Old)
		return nil
	}

	obj, ok := parent.(map[string]any)
	if !ok {
		return fmt.Errorf("path %q: %q applied to a non-object", e.Path, last.key)
	}
	if !e.HasOld {
		delete(obj, last.key)
		return nil
	}
	obj[last.key] = normalizeJSONValue(e.Old)
	return nil
}

// pathStep is one accessor in a field path: a map key or an array index.
type pathStep struct {
	key     string
	index   int
	isIndex bool
}

// tokenizePath splits a diff-evidence field path into accessors, resolving
// it against `root` as it goes. Three accessor syntaxes appear in real
// evidence and all three are handled:
//
//	spec.template.spec.containers[0].image                       (array index)
//	spec.template.metadata.annotations['checksum/config']        (quoted key)
//	spec.template.metadata.annotations.kubectl.kubernetes.io/restartedAt
//
// The third is why this walks the live object instead of splitting on ".":
// the Go agent emits unquoted keys, and annotation/label keys contain dots,
// so "annotations.kubectl.kubernetes.io/restartedAt" is ONE key, not four.
// Unquoted segments therefore take the longest key that actually exists at
// the current level, falling back to the shortest segment when nothing
// matches (the field may be absent precisely because the change added it).
//
// A leading "<Kind>." qualifier (the legacy agent emits
// "DaemonSet.spec.template...") is stripped first.
func tokenizePath(path string, root any) ([]pathStep, error) {
	path = strings.TrimPrefix(path, ".")
	if dot := strings.IndexByte(path, '.'); dot > 0 {
		if _, _, ok := resolveWorkloadKind(path[:dot]); ok {
			path = path[dot+1:]
		}
	}

	var steps []pathStep
	node := root
	prevKey := ""
	for i := 0; i < len(path); {
		switch path[i] {
		case '.':
			i++
		case '[':
			end := strings.IndexByte(path[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("path %q: unterminated '['", path)
			}
			tok := path[i+1 : i+end]
			i += end + 1
			if len(tok) >= 2 && (tok[0] == '\'' || tok[0] == '"') && tok[len(tok)-1] == tok[0] {
				key := tok[1 : len(tok)-1]
				steps = append(steps, pathStep{key: key})
				node = descendKey(node, key)
				prevKey = key
				continue
			}
			idx, err := strconv.Atoi(tok)
			if err != nil {
				return nil, fmt.Errorf("path %q: bad accessor [%s]", path, tok)
			}
			steps = append(steps, pathStep{index: idx, isIndex: true})
			node = descendIndex(node, idx)
			prevKey = ""
		default:
			var key string
			var consumed int
			if isFlatKeyMap(prevKey) {
				// Inside a flat user-keyed map the whole remainder is one key,
				// whether or not it is still present on the live object. Without
				// this, reverting a DELETED annotation like
				// "annotations.app.kubernetes.io/version" has nothing to
				// longest-match against and gets split at the dots.
				key, consumed = path[i:], len(path)-i
			} else {
				key, consumed = matchKey(node, path[i:])
			}
			steps = append(steps, pathStep{key: key})
			node = descendKey(node, key)
			prevKey = key
			i += consumed
		}
	}
	return steps, nil
}

// flatKeyMapFields are the fields whose value is a flat map keyed by
// user-chosen strings — label/annotation names, extended resource names like
// "nvidia.com/gpu", ConfigMap entries. Those keys routinely contain dots, and
// nothing nests below them, so the rest of the path is exactly one key. Mirrors
// kube.userDataContainerFields, in the camelCase spelling the diff paths use.
var flatKeyMapFields = map[string]struct{}{
	"labels":       {},
	"annotations":  {},
	"matchLabels":  {},
	"nodeSelector": {},
	"limits":       {},
	"requests":     {},
	"data":         {},
	"stringData":   {},
	"binaryData":   {},
}

func isFlatKeyMap(field string) bool {
	_, ok := flatKeyMapFields[field]
	return ok
}

// matchKey picks the map key an unquoted path segment refers to, preferring
// the longest key present on `node` so dotted annotation/label keys survive.
func matchKey(node any, rest string) (string, int) {
	if obj, ok := node.(map[string]any); ok {
		best := ""
		for k := range obj {
			if len(k) <= len(best) {
				continue
			}
			if rest == k || strings.HasPrefix(rest, k+".") || strings.HasPrefix(rest, k+"[") {
				best = k
			}
		}
		if best != "" {
			return best, len(best)
		}
	}
	end := len(rest)
	if dot := strings.IndexByte(rest, '.'); dot >= 0 {
		end = dot
	}
	if br := strings.IndexByte(rest, '['); br >= 0 && br < end {
		end = br
	}
	return rest[:end], end
}

func descendKey(node any, key string) any {
	obj, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	return obj[key]
}

func descendIndex(node any, idx int) any {
	arr, ok := node.([]any)
	if !ok || idx < 0 || idx >= len(arr) {
		return nil
	}
	return arr[idx]
}

// navigate walks `steps` from root and returns the container they address.
// With create=true, missing (or null) intermediate map keys are filled in
// with empty objects so a field the change deleted can be written back.
func navigate(root map[string]any, steps []pathStep, create bool) (any, error) {
	var cur any = root
	for _, s := range steps {
		if s.isIndex {
			arr, ok := cur.([]any)
			if !ok {
				return nil, fmt.Errorf("[%d] applied to a non-array", s.index)
			}
			if s.index < 0 || s.index >= len(arr) {
				return nil, fmt.Errorf("index %d out of range (len %d)", s.index, len(arr))
			}
			cur = arr[s.index]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%q applied to a non-object", s.key)
		}
		next, exists := obj[s.key]
		if !exists || next == nil {
			if !create {
				return nil, fmt.Errorf("%q not found", s.key)
			}
			next = map[string]any{}
			obj[s.key] = next
		}
		cur = next
	}
	return cur, nil
}

// normalizeJSONValue coerces values decoded from JSON into the types
// unstructured objects are required to hold: integral float64 becomes
// int64 (json.Number-style whole numbers would otherwise re-encode as
// "2" but violate the unstructured contract for typed conversions).
func normalizeJSONValue(v any) any {
	switch x := v.(type) {
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
		return x
	case float32:
		return normalizeJSONValue(float64(x))
	case int:
		return int64(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = normalizeJSONValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = normalizeJSONValue(item)
		}
		return out
	default:
		return v
	}
}

// parseRevertEntries reads the `revert_paths` param: a list of
// {path, old} objects. A missing or null `old` marks a field the change
// added, which reverts to "remove the field".
func parseRevertEntries(v any) ([]RevertEntry, error) {
	raw, ok := v.([]any)
	if !ok || len(raw) == 0 {
		return nil, errors.New("revert_workload: revert_paths required (list of {path, old})")
	}
	entries := make([]RevertEntry, 0, len(raw))
	for i, item := range raw {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("revert_workload: revert_paths[%d] is not an object", i)
		}
		path, _ := obj["path"].(string)
		if path == "" {
			return nil, fmt.Errorf("revert_workload: revert_paths[%d] has no path", i)
		}
		old, present := obj["old"]
		entries = append(entries, RevertEntry{
			Path:   path,
			Old:    old,
			HasOld: present && old != nil,
		})
	}
	return entries, nil
}
