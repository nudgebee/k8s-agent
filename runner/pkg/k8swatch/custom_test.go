package k8swatch

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	fakedynamic "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var certGVR = schema.GroupVersionResource{Group: "cert-manager.io", Version: "v1", Resource: "certificates"}

func cert(name, rv, dns string, created time.Time) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata": map[string]any{
			"name": name, "namespace": "shop", "resourceVersion": rv,
			"creationTimestamp": created.UTC().Format(time.RFC3339),
			"managedFields":     []any{map[string]any{"manager": "kubectl"}},
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"password":"hunter2"}}`,
				"team": "payments",
			},
		},
		"spec":   map[string]any{"dnsNames": []any{dns}, "secretName": "web-tls"},
		"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
		"extra":  map[string]any{"password": "hunter2"},
	}}
	return u
}

// What is not cached is never encoded: status, managedFields, the applied
// copy and top-level fields outside diffPaths must all be gone.
func TestCustomResourceTransform_KeepsOnlyMetadataAndDiffPaths(t *testing.T) {
	out, err := CustomResourceTransform([]string{"spec"})(cert("web", "1", "a.example.com", time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	u := out.(*unstructured.Unstructured)
	for _, k := range []string{"status", "extra"} {
		if _, ok := u.Object[k]; ok {
			t.Errorf("%s must not be cached", k)
		}
	}
	if _, ok := u.Object["spec"]; !ok {
		t.Error("spec (in diffPaths) must be kept")
	}
	if _, ok, _ := unstructured.NestedFieldNoCopy(u.Object, "metadata", "managedFields"); ok {
		t.Error("managedFields must be dropped")
	}
	ann := u.GetAnnotations()
	if _, ok := ann["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Error("the applied copy must be dropped: it holds every field, including ones outside diffPaths")
	}
	if ann["team"] != "payments" {
		t.Error("other annotations must be kept")
	}
}

func fakeDiscovery(resources ...*metav1.APIResourceList) *fakediscovery.FakeDiscovery {
	cs := fake.NewSimpleClientset()
	d := cs.Discovery().(*fakediscovery.FakeDiscovery)
	d.Resources = resources
	return d
}

type crMetrics struct {
	mu    sync.Mutex
	state map[string]int
}

func (m *crMetrics) OnK8sWatchEvent(string, string) {}
func (m *crMetrics) SetK8sWatchPending(int)         {}
func (m *crMetrics) SetK8sWatchCustomResources(state string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == nil {
		m.state = map[string]int{}
	}
	m.state[state] = n
}
func (m *crMetrics) OnK8sWatchCustomResourceError(string) {}

func TestResolveCustomResources_States(t *testing.T) {
	disco := fakeDiscovery(
		&metav1.APIResourceList{GroupVersion: "cert-manager.io/v1", APIResources: []metav1.APIResource{
			{Name: "certificates", Kind: "Certificate", Namespaced: true},
		}},
		&metav1.APIResourceList{GroupVersion: "external-secrets.io/v1beta1", APIResources: []metav1.APIResource{
			{Name: "externalsecrets", Kind: "ExternalSecret", Namespaced: true},
		}},
	)
	scheme := runtime.NewScheme()
	dyn := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		certGVR: "CertificateList",
	})
	m := &crMetrics{}
	got := ResolveCustomResources(context.Background(), disco, dyn, "", []CustomResourceEntry{
		{Group: "cert-manager.io", Resource: "certificates"},
		{Group: "cert-manager.io", Resource: "certificates"}, // duplicate: counted once
		{Group: "external-secrets.io", Resource: "externalsecrets"},
		{Group: "operator.victoriametrics.com", Resource: "vmusers"},
		{Group: "networking.istio.io", Resource: "virtualservices"}, // not served
	}, m, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 resolved, got %d (%+v)", len(got), got)
	}
	r := got[0]
	if r.GVR != certGVR || r.Kind != "Certificate" || !r.Namespaced || r.QualifiedKind() != "Certificate.cert-manager.io" {
		t.Errorf("resolved = %+v", r)
	}
	if len(r.DiffPaths) != 1 || r.DiffPaths[0] != "spec" {
		t.Errorf("default diffPaths = %v", r.DiffPaths)
	}
	want := map[string]int{CustomResourceWatching: 1, CustomResourceRefused: 2, CustomResourceUnresolved: 1}
	for k, v := range want {
		if m.state[k] != v {
			t.Errorf("gauge %s = %d, want %d (all: %v)", k, m.state[k], v, m.state)
		}
	}
}

// A listed resource the runner cannot list (RBAC gap) is unresolved, not
// watched: an informer that cannot list never syncs.
func TestResolveCustomResources_UnlistableIsUnresolved(t *testing.T) {
	disco := fakeDiscovery(&metav1.APIResourceList{GroupVersion: "cert-manager.io/v1", APIResources: []metav1.APIResource{
		{Name: "certificates", Kind: "Certificate", Namespaced: true},
	}})
	dyn := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{certGVR: "CertificateList"})
	dyn.PrependReactor("list", "certificates", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errForbidden{}
	})
	if got := ResolveCustomResources(context.Background(), disco, dyn, "", []CustomResourceEntry{{Group: "cert-manager.io", Resource: "certificates"}}, nil, nil); len(got) != 0 {
		t.Fatalf("an unlistable resource must not be watched, got %+v", got)
	}
}

type errForbidden struct{}

func (errForbidden) Error() string { return "forbidden" }

func crUpdatesOnly(kind, operation string) bool {
	return operation == "update" && strings.Contains(kind, ".")
}

// End to end through a dynamic informer: an update is encoded under the
// qualified kind with the group/version apiVersion, and the evidence carries
// neither status nor the applied copy nor fields outside diffPaths.
func TestWatcher_CustomResourceUpdateEncoded(t *testing.T) {
	scheme := runtime.NewScheme()
	start := time.Now().Add(-time.Hour)
	existing := cert("web", "1", "a.example.com", start.Add(-time.Hour))
	dyn := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{certGVR: "CertificateList"}, existing)
	sink := &recordingSink{}
	w := New(Config{StartTime: start, Kinds: map[string]bool{"pod": true}}, sink, crUpdatesOnly, nil, nil)
	w.RegisterCustomResources(dyn, []ResolvedCustomResource{{GVR: certGVR, Kind: "Certificate", Namespaced: true, DiffPaths: []string{"spec"}}})
	if len(w.synced) != 1 {
		t.Fatalf("K8S_WATCH_KINDS must not filter custom resources; registered %d", len(w.synced))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	waitFor(t, func() bool { return w.synced[0].synced() })

	upd := cert("web", "2", "b.example.com", start.Add(-time.Hour))
	if _, err := dyn.Resource(certGVR).Namespace("shop").Update(ctx, upd, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(sink.snapshot()) == 1 })
	c := sink.snapshot()[0]
	if c.kind != "Certificate.cert-manager.io" || c.data["apiVersion"] != "cert-manager.io/v1" || c.data["kind"] != "Certificate.cert-manager.io" {
		t.Errorf("encoded kind/apiVersion = %q / %v / %v", c.kind, c.data["kind"], c.data["apiVersion"])
	}
	raw := string(c.raw)
	for _, leak := range []string{"hunter2", "last-applied-configuration", "Ready", "managedFields"} {
		if strings.Contains(raw, leak) {
			t.Errorf("encoded change carries %q", leak)
		}
	}
	if !strings.Contains(raw, "b.example.com") || !strings.Contains(raw, "a.example.com") {
		t.Error("encoded change must carry the old and new spec")
	}
}

// With the watch limited to one namespace, a cluster-scoped custom resource is
// unresolved: a namespaced informer for it would never sync.
func TestResolveCustomResources_ClusterScopedWithNamespaceIsUnresolved(t *testing.T) {
	npGVR := schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}
	disco := fakeDiscovery(&metav1.APIResourceList{GroupVersion: "karpenter.sh/v1", APIResources: []metav1.APIResource{
		{Name: "nodepools", Kind: "NodePool", Namespaced: false},
	}})
	dyn := fakedynamic.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{npGVR: "NodePoolList"})
	entries := []CustomResourceEntry{{Group: "Karpenter.sh", Resource: "NodePools"}}
	if got := ResolveCustomResources(context.Background(), disco, dyn, "apps", entries, nil, nil); len(got) != 0 {
		t.Fatalf("cluster-scoped resource with a namespace-limited watch must not be watched, got %+v", got)
	}
	got := ResolveCustomResources(context.Background(), disco, dyn, "", entries, nil, nil)
	if len(got) != 1 || got[0].Namespaced || got[0].GVR != npGVR {
		t.Fatalf("cluster-wide watch: want the cluster-scoped resource (group and resource lower-cased), got %+v", got)
	}
}
