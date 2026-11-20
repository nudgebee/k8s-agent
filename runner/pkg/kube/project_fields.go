package kube

import (
	"slices"
	"strings"
)

// identityFields are kept whatever a projection asks for: without them a caller
// cannot tell objects apart, nor tell kinds apart in a comma-separated
// resource_type list.
var identityFields = []string{"apiVersion", "kind", "metadata.name", "metadata.namespace", "metadata.uid"}

// fieldTree is a set of dotted paths as a tree; a node with no children keeps
// the whole value found there.
type fieldTree map[string]fieldTree

// newFieldTree builds the tree for fields plus identityFields; nil when fields is
// empty, which means no projection.
//
// Paths are dotted Kubernetes API field names as the API server returns them
// (camelCase, case-sensitive): "metadata.ownerReferences", "spec.volumes",
// "spec.containers.env". The reply is snake_cased afterwards as always.
func newFieldTree(fields []string) fieldTree {
	if len(fields) == 0 {
		return nil
	}
	tree := fieldTree{}
	paths := make([]string, 0, len(identityFields)+len(fields))
	paths = append(paths, identityFields...)
	paths = append(paths, fields...)
	for _, path := range paths {
		segments := strings.Split(path, ".")
		if slices.Contains(segments, "") {
			continue // malformed ("spec.", "a..b"): keeping a prefix would keep too much
		}
		node := tree
		for i, seg := range segments {
			child, seen := node[seg]
			if seen && len(child) == 0 {
				break // an ancestor already keeps the whole subtree
			}
			if i == len(segments)-1 {
				node[seg] = fieldTree{} // keep everything here, overriding narrower paths
				break
			}
			if !seen {
				child = fieldTree{}
				node[seg] = child
			}
			node = child
		}
	}
	return tree
}

// projectFields returns obj with only the fields in tree.
//
//   - A path keeps the whole value at its last segment.
//   - When a segment lands on an array, the rest of the path applies to each
//     element: map elements are projected, other elements are kept as they are.
//   - A path that is not in the object adds nothing.
func projectFields(obj map[string]any, tree fieldTree) map[string]any {
	out := make(map[string]any, len(tree))
	for key, sub := range tree {
		value, ok := obj[key]
		if !ok {
			continue
		}
		if len(sub) == 0 {
			out[key] = value
			continue
		}
		if projected, keep := projectValue(value, sub); keep {
			out[key] = projected
		}
	}
	return out
}

func projectValue(value any, tree fieldTree) (any, bool) {
	switch v := value.(type) {
	case map[string]any:
		return projectFields(v, tree), true
	case []any:
		out := make([]any, len(v))
		for i, element := range v {
			if m, ok := element.(map[string]any); ok {
				out[i] = projectFields(m, tree)
			} else {
				out[i] = element
			}
		}
		return out, true
	default:
		// A scalar where the path expects more: the path does not exist here.
		return nil, false
	}
}
