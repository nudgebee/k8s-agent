package kube

// objectCopyAnnotations hold a serialized copy of the whole object, values
// included, so blanking the values while keeping them would send the values
// anyway.
var objectCopyAnnotations = []string{
	"kubectl.kubernetes.io/last-applied-configuration",
	// Rancher / k3s wrangler apply: gzipped, base64 copy of the applied object.
	"objectset.rio.cattle.io/applied",
}

// omitConfigMapValues blanks the values of a core/v1 ConfigMap in place,
// keeping its keys so callers can still count and name them, and drops the
// annotations that copy the whole object. A cluster's ConfigMaps are mostly
// value bytes; a caller that records only key names gets a reply a fraction
// of the size.
func omitConfigMapValues(obj map[string]any) {
	for _, field := range []string{"data", "binaryData"} {
		m, ok := obj[field].(map[string]any)
		if !ok {
			continue
		}
		for k := range m {
			m[k] = ""
		}
	}
	meta, _ := obj["metadata"].(map[string]any)
	if ann, ok := meta["annotations"].(map[string]any); ok {
		for _, k := range objectCopyAnnotations {
			delete(ann, k)
		}
	}
}
