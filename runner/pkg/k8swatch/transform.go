package k8swatch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// maxConfigMapBytes is the size of a ConfigMap's data and binaryData above
// which the cache keeps only the head of each value. The evidence carries the
// object before and after a change verbatim only while the two together stay
// under 256 KiB (alerts.maxRawPayloadBytes); one object over 256 KiB can never
// be carried, so trimming it loses nothing a reader could have seen.
const maxConfigMapBytes = 256 << 10

// configMapHeadBytes is how much of each value a trimmed ConfigMap keeps: the
// diff evidence shows at most this much of a value (triggers.maxConfigMapValueBytes).
const configMapHeadBytes = 2048

// OwnTransform trims the objects only the watcher caches before they enter
// the cache. managedFields goes from all of them: no matcher or evidence
// reader uses it, and kubewatch stripped it too. Oversized ConfigMaps keep
// the head of each value plus a hash of the whole value, so an edit anywhere
// in a value still changes what the diff compares.
//
// Objects in the cache are shared with every reader, so this only removes
// fields; it runs at most once per object version and must be idempotent.
func OwnTransform(obj any) (any, error) {
	switch o := obj.(type) {
	case *corev1.ConfigMap:
		o.ManagedFields = nil
		trimConfigMap(o)
	case *corev1.Service:
		o.ManagedFields = nil
	case *networkingv1.Ingress:
		o.ManagedFields = nil
	case *unstructured.Unstructured:
		unstructured.RemoveNestedField(o.Object, "metadata", "managedFields")
	}
	return obj, nil
}

func trimConfigMap(cm *corev1.ConfigMap) {
	size := 0
	for _, v := range cm.Data {
		size += len(v)
	}
	for _, v := range cm.BinaryData {
		size += len(v)
	}
	if size <= maxConfigMapBytes {
		return
	}
	for k, v := range cm.Data {
		cm.Data[k] = trimValue(v)
	}
	for k, v := range cm.BinaryData {
		cm.BinaryData[k] = []byte(trimValue(string(v)))
	}
}

// trimValue keeps a value up to configMapHeadBytes as it is; a longer one
// becomes its head plus a marker naming its size and hash. Already-trimmed
// values are short enough to pass through unchanged.
func trimValue(v string) string {
	if len(v) <= configMapHeadBytes+128 {
		return v
	}
	head := configMapHeadBytes
	for head > 0 && !utf8.RuneStart(v[head]) { // do not split a character
		head--
	}
	sum := sha256.Sum256([]byte(v))
	return fmt.Sprintf("%s\n... [trimmed by the agent: %d bytes, sha256:%s]",
		v[:head], len(v), hex.EncodeToString(sum[:]))
}
