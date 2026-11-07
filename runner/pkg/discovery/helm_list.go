package discovery

import (
	"context"
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// HelmReleaseMeta is what list_helm_releases returns for one release: chart
// identification and status, decoded inside the agent. The release Secret also
// holds the rendered manifests and the user-supplied values, credentials
// included; none of that leaves the agent.
type HelmReleaseMeta struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Revision     int    `json:"revision"`
	Status       string `json:"status"`
	ChartName    string `json:"chart_name"`
	ChartVersion string `json:"chart_version"`
	AppVersion   string `json:"app_version"`
	KubeVersion  string `json:"kube_version"`
	LastDeployed string `json:"last_deployed,omitempty"`
}

// helmListPageSize bounds memory: a release Secret carries the gzipped
// manifests and can run to hundreds of KiB, so the whole cluster's worth is
// never held at once.
const helmListPageSize = 50

// ErrHelmReleaseDiscoveryDisabled is returned when the agent's RBAC does not
// grant the cluster-wide secrets list this needs (runner.helmReleaseDiscovery).
var ErrHelmReleaseDiscoveryDisabled = errors.New("helm release discovery is not enabled on this agent (chart value runner.helmReleaseDiscovery): it needs cluster-wide list on secrets")

// ListHelmReleases returns the latest revision of every Helm v3 release in
// namespace ("" for all namespaces), sorted by namespace then name. Secrets
// that are not decodable Helm releases are skipped.
func ListHelmReleases(ctx context.Context, cs kubernetes.Interface, namespace string) ([]HelmReleaseMeta, error) {
	latest := map[string]HelmReleaseMeta{}
	opts := metav1.ListOptions{LabelSelector: HelmReleaseSelector().String(), Limit: helmListPageSize}
	for {
		page, err := cs.CoreV1().Secrets(namespace).List(ctx, opts)
		if apierrors.IsForbidden(err) {
			return nil, ErrHelmReleaseDiscoveryDisabled
		}
		if err != nil {
			return nil, fmt.Errorf("list helm release secrets: %w", err)
		}
		for i := range page.Items {
			m, ok := helmReleaseMeta(&page.Items[i])
			if !ok {
				continue
			}
			key := m.Namespace + "/" + m.Name
			if cur, seen := latest[key]; !seen || m.Revision > cur.Revision {
				latest[key] = m
			}
		}
		if page.Continue == "" {
			break
		}
		opts.Continue = page.Continue
	}

	out := make([]HelmReleaseMeta, 0, len(latest))
	for _, m := range latest {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func helmReleaseMeta(s *corev1.Secret) (HelmReleaseMeta, bool) {
	if s.Type != "helm.sh/release.v1" {
		return HelmReleaseMeta{}, false
	}
	raw, ok := s.Data["release"]
	if !ok {
		return HelmReleaseMeta{}, false
	}
	rel, err := decodeHelmRelease(raw)
	if err != nil {
		return HelmReleaseMeta{}, false
	}
	ns := rel.Namespace
	if ns == "" {
		ns = s.Namespace
	}
	return HelmReleaseMeta{
		Name:         rel.Name,
		Namespace:    ns,
		Revision:     rel.Version,
		Status:       rel.Info.Status,
		ChartName:    rel.Chart.Metadata.Name,
		ChartVersion: rel.Chart.Metadata.Version,
		AppVersion:   rel.Chart.Metadata.AppVersion,
		KubeVersion:  rel.Chart.Metadata.KubeVersion,
		LastDeployed: rel.Info.LastDeployed,
	}, true
}
