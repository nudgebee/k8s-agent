package discovery

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// helmRelease produces the same blob shape Helm v3 stores in the secret's
// data["release"] field: base64( gzip( json(release) ) ).
func encodeHelmRelease(t *testing.T, rel map[string]any) []byte {
	t.Helper()
	plain, _ := json.Marshal(rel)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	return []byte(base64.StdEncoding.EncodeToString(gz.Bytes()))
}

func TestConvertHelmReleaseSecret_HappyPath(t *testing.T) {
	blob := encodeHelmRelease(t, map[string]any{
		"name":      "loki",
		"namespace": "monitoring",
		"version":   3,
		"info": map[string]any{
			"status":         "deployed",
			"first_deployed": "2025-01-01T00:00:00Z",
			"last_deployed":  "2025-04-15T12:00:00Z",
			"description":    "Upgrade complete",
		},
		"chart": map[string]any{
			"metadata": map[string]any{
				"name":       "loki",
				"version":    "5.41.0",
				"appVersion": "2.9.4",
			},
		},
	})
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.loki.v3", Namespace: "monitoring", Labels: map[string]string{"owner": "helm"}},
		Type:       "helm.sh/release.v1",
		Data:       map[string][]byte{"release": blob},
	}
	got, ok := convertHelmReleaseSecret(sec)
	if !ok {
		t.Fatal("expected ok=true for valid helm secret")
	}
	m := got.(map[string]any)
	if m["name"] != "loki" || m["namespace"] != "monitoring" || m["version"] != 3 {
		t.Errorf("identification: %+v", m)
	}
	if m["status"] != "deployed" || m["chart_name"] != "loki" || m["chart_version"] != "5.41.0" {
		t.Errorf("chart info: %+v", m)
	}
	if m["service_key"] != "monitoring/loki" {
		t.Errorf("service_key = %v", m["service_key"])
	}
}

func TestConvertHelmReleaseSecret_RejectsWrongType(t *testing.T) {
	if _, ok := convertHelmReleaseSecret(&corev1.Pod{}); ok {
		t.Error("non-Secret object should return ok=false")
	}
	notHelm := &corev1.Secret{
		Type: "Opaque",
		Data: map[string][]byte{"release": []byte("ignored")},
	}
	if _, ok := convertHelmReleaseSecret(notHelm); ok {
		t.Error("non-helm Secret should return ok=false")
	}
}

func TestConvertHelmReleaseSecret_NoReleaseField(t *testing.T) {
	s := &corev1.Secret{
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{},
	}
	if _, ok := convertHelmReleaseSecret(s); ok {
		t.Error("missing release data should return ok=false")
	}
}

func TestConvertHelmReleaseSecret_BadBlobSwallowed(t *testing.T) {
	s := &corev1.Secret{
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{"release": []byte("not-base64-and-not-gzip")},
	}
	if _, ok := convertHelmReleaseSecret(s); ok {
		t.Error("undecodable blob should return ok=false (logged elsewhere)")
	}
}

func TestHelmReleaseSelector(t *testing.T) {
	sel := HelmReleaseSelector()
	matches := sel.Matches(testLabels{"owner": "helm"})
	if !matches {
		t.Error("selector should match owner=helm")
	}
	if sel.Matches(testLabels{"owner": "argo"}) {
		t.Error("selector should NOT match owner=argo")
	}
}

// testLabels is a tiny labels.Labels impl for selector tests.
type testLabels map[string]string

func (l testLabels) Has(k string) bool   { _, ok := l[k]; return ok }
func (l testLabels) Get(k string) string { return l[k] }
func (l testLabels) Lookup(k string) (string, bool) {
	v, ok := l[k]
	return v, ok
}
