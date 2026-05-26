package discovery

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
)

// Helm release detection.
//
// Helm v3 stores each release as a Secret in the release's namespace, with:
//
//	type:   helm.sh/release.v1
//	labels: owner=helm, status=deployed|...
//	data["release"]: double-encoded — base64(gzip(json(release)))
//
// We reuse the existing Secret informer (filtered by label) so this
// doesn't add a new watch type.

// HelmReleaseSelector returns the listoptions selector. Use it when wiring
// a tweakable list-watch (e.g. via informers.WithTweakListOptions) so the
// agent only ingests Helm secrets, not all secrets in the cluster (which
// would be a privilege escalation surface anyway).
func HelmReleaseSelector() labels.Selector {
	r, _ := labels.NewRequirement("owner", selection.Equals, []string{"helm"})
	return labels.NewSelector().Add(*r)
}

// helmReleaseEnvelope is the subset of the Helm release JSON we care about.
// The full schema is much bigger; we only extract identification + status.
type helmReleaseEnvelope struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
	Info      struct {
		Status        string `json:"status"`
		FirstDeployed string `json:"first_deployed"`
		LastDeployed  string `json:"last_deployed"`
		Description   string `json:"description"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			AppVersion string `json:"appVersion"`
		} `json:"metadata"`
	} `json:"chart"`
}

// convertHelmReleaseSecret turns a "owner=helm" Secret into the wire shape
// the collector consumes (HelmRelease model). Returns ok=false if the
// secret is not a Helm release or can't be decoded.
func convertHelmReleaseSecret(obj any) (any, bool) {
	s, ok := obj.(*corev1.Secret)
	if !ok {
		return nil, false
	}
	if s.Type != "helm.sh/release.v1" {
		return nil, false
	}
	raw, ok := s.Data["release"]
	if !ok {
		return nil, false
	}
	rel, err := decodeHelmRelease(raw)
	if err != nil {
		return nil, false
	}
	return map[string]any{
		"name":           rel.Name,
		"namespace":      rel.Namespace,
		"version":        rel.Version,
		"status":         rel.Info.Status,
		"first_deployed": rel.Info.FirstDeployed,
		"last_deployed":  rel.Info.LastDeployed,
		"description":    rel.Info.Description,
		"chart_name":     rel.Chart.Metadata.Name,
		"chart_version":  rel.Chart.Metadata.Version,
		"app_version":    rel.Chart.Metadata.AppVersion,
		// Stable cross-revision key so the collector can dedupe to the latest.
		"service_key": fmt.Sprintf("%s/%s", rel.Namespace, rel.Name),
	}, true
}

// decodeHelmRelease unwraps base64(gzip(json)). Helm v3 stores the data
// field as the raw bytes (already base64-decoded by the apiserver into
// .Data), but inside that's gzip-wrapped JSON with one extra base64 layer.
//
// raw can be either:
//   - the decoded byte slice (k8s API has already base64-decoded once); in
//     that case the bytes are still base64+gzip+json (yes, double-base64)
//   - a string of base64 bytes
//
// We try both.
func decodeHelmRelease(raw []byte) (*helmReleaseEnvelope, error) {
	// First decode: Helm wraps with an extra base64 layer.
	decoded, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		// Maybe already raw (some installations).
		decoded = raw
	}
	// Second: gunzip.
	gz, err := gzip.NewReader(bytes.NewReader(decoded))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	plain, err := io.ReadAll(gz)
	if err != nil {
		return nil, fmt.Errorf("gunzip: %w", err)
	}
	// Third: parse JSON (we only care about a subset).
	var env helmReleaseEnvelope
	if err := json.Unmarshal(plain, &env); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return &env, nil
}
