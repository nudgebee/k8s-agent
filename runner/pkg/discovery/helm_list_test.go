package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func helmSecret(t *testing.T, ns, name string, rev int, status string) *corev1.Secret {
	t.Helper()
	blob := encodeHelmRelease(t, map[string]any{
		"name": name, "namespace": ns, "version": rev,
		"info": map[string]any{"status": status, "last_deployed": "2026-11-01T00:00:00Z"},
		"chart": map[string]any{"metadata": map[string]any{
			"name": name, "version": fmt.Sprintf("1.%d.0", rev), "appVersion": "2.0",
			"kubeVersion": ">=1.25.0-0",
		}},
		// Release values: the part that must never leave the agent.
		"config": map[string]any{"db": map[string]any{"password": "hunter2"}},
	})
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("sh.helm.release.v1.%s.v%d", name, rev), Namespace: ns,
			Labels: map[string]string{"owner": "helm", "name": name},
		},
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{"release": blob},
	}
}

func TestListHelmReleases_LatestRevisionMetadataOnly(t *testing.T) {
	cs := fake.NewSimpleClientset(
		helmSecret(t, "monitoring", "loki", 2, "superseded"),
		helmSecret(t, "monitoring", "loki", 3, "deployed"),
		helmSecret(t, "app", "api", 1, "failed"),
		// Decodes, but names no release (blob is JSON null).
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.empty.v1", Namespace: "app", Labels: map[string]string{"owner": "helm"}},
			Type:       "helm.sh/release.v1",
			Data:       map[string][]byte{"release": encodeHelmRelease(t, nil)},
		},
		// Not a release: same label, other type.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "app", Labels: map[string]string{"owner": "helm"}},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"release": []byte("nope")},
		},
	)

	got, err := ListHelmReleases(context.Background(), cs, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []HelmReleaseMeta{
		{Name: "api", Namespace: "app", Revision: 1, Status: "failed", ChartName: "api", ChartVersion: "1.1.0", AppVersion: "2.0", KubeVersion: ">=1.25.0-0", LastDeployed: "2026-11-01T00:00:00Z"},
		{Name: "loki", Namespace: "monitoring", Revision: 3, Status: "deployed", ChartName: "loki", ChartVersion: "1.3.0", AppVersion: "2.0", KubeVersion: ">=1.25.0-0", LastDeployed: "2026-11-01T00:00:00Z"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "hunter2") {
		t.Errorf("release values leaked: %s", b)
	}
}

func TestListHelmReleases_ForbiddenMeansOptInMissing(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, "", errors.New("rbac"))
	})
	if _, err := ListHelmReleases(context.Background(), cs, ""); !errors.Is(err, ErrHelmReleaseDiscoveryDisabled) {
		t.Fatalf("err = %v; want ErrHelmReleaseDiscoveryDisabled", err)
	}
}
