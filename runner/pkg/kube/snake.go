package kube

import (
	"strings"
	"unicode"
)

// SnakeKeysDeep walks a JSON-shaped tree (map[string]any / []any / scalars)
// and converts every map key from camelCase to snake_case. Used on
// `get_resource` / `get_resource_yaml` output so the wire shape matches
// the legacy Hikaru-driven attribute naming (`accessModes` → `access_modes`,
// `creationTimestamp` → `creation_timestamp`, …).
//
// Why this exists: UI components that consume `get_resource`
// (KubernetesPVC.jsx, KubernetesPV.jsx, KubernetesPodYaml.jsx, …) read
// fields by their snake_case names. The legacy get_resource returned
// Hikaru-shaped JSON which is snake_case throughout. Our Go agent's
// client-go returns the K8s API's native camelCase. Without conversion,
// every `item.spec.access_modes.join(',')` in the UI throws because the
// field is actually `accessModes`, the table row is undefined, and the
// whole table renders empty even though the payload is correct.
//
// Skips recursion into known user-data containers (labels, annotations,
// configMap.data, secret.data, selector.matchLabels, …) — those keys
// are arbitrary user content like "app.kubernetes.io/name" and must
// not be munged.
func SnakeKeysDeep(v any) any {
	return convertWithParent(v, "")
}

// convertWithParent does the recursive walk. parentField is the
// name of the field this value is the value of (snake-cased) — used
// to decide whether the value's keys are user data (skip recursion)
// or part of the K8s schema (recurse and rename).
func convertWithParent(v any, parentField string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			newKey := toSnakeCase(k)
			if isUserDataContainer(parentField) {
				// Don't rename keys inside user-data containers, but DO
				// keep walking — values can be nested objects we need
				// to leave unchanged. Easiest: copy the value as-is.
				out[k] = val
				continue
			}
			out[newKey] = convertWithParent(val, newKey)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			// Array items inherit the parentField — e.g. spec.containers
			// is an array, each element is still a "containers" field
			// from the schema's POV.
			out[i] = convertWithParent(item, parentField)
		}
		return out
	default:
		return v
	}
}

// userDataContainerFields lists snake-case field names whose VALUES are
// user-supplied key/value maps where the keys are arbitrary strings
// (label names, annotation names, ConfigMap entries, etc.). When we hit
// one of these, we copy the inner map as-is to preserve user keys
// like "app.kubernetes.io/name" or "kubectl.kubernetes.io/last-applied-
// configuration".
var userDataContainerFields = map[string]struct{}{
	"labels":        {},
	"annotations":   {},
	"match_labels":  {}, // selector.matchLabels — values are user-chosen label names
	"node_selector": {}, // pod.spec.nodeSelector
	"data":          {}, // configmap.data, secret.data
	"string_data":   {}, // secret.stringData
	"binary_data":   {}, // configmap.binaryData
	"sysctls":       {}, // pod.spec.securityContext.sysctls — kernel param names
}

// Note on `selector`: deliberately NOT in this list. service.spec.selector's
// values ARE user labels, but its sibling at deployment.spec.selector wraps
// matchLabels/matchExpressions which DO need renaming. We let the walker
// recurse — user labels almost always contain "/", "-", or "." (caught by
// toSnakeCase's early return) so they pass through unchanged in practice.
// The rare camelCase Service selector key (e.g. "appName" → "app_name") is
// an accepted divergence from a pure passthrough.

func isUserDataContainer(field string) bool {
	_, ok := userDataContainerFields[field]
	return ok
}

// toSnakeCase converts a single field name. Idempotent: if the key
// already has any of `_`, `.`, `/`, `-`, `:`, returns as-is (already
// snake or non-schema).
func toSnakeCase(s string) string {
	if s == "" {
		return s
	}
	if strings.ContainsAny(s, "_./-:") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			// Insert underscore before the uppercase letter — except
			// when it follows another uppercase (handle acronyms like
			// "APIVersion" → "api_version" not "a_p_i_version").
			prev := []rune(s)[i-1]
			if unicode.IsLower(prev) || unicode.IsDigit(prev) {
				b.WriteByte('_')
			} else if i+1 < len(s) {
				next := []rune(s)[i+1]
				// "APIVersion" — at i=3 ('V'), prev='I' (upper),
				// next='e' (lower). Insert underscore before V to
				// separate the acronym from the next word.
				if unicode.IsLower(next) {
					b.WriteByte('_')
				}
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
