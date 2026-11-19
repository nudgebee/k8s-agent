package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// The action used to send json.Marshal(SnakeKeysDeep(GetResource(...))).
// Callers parse that string, so the streamed encoding has to match it exactly.
func TestGetResourceJSON_MatchesTheMapEncoding(t *testing.T) {
	secret := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": "creds", "namespace": "app",
			"labels": map[string]any{"camelLabel": "x"}},
		"data": map[string]any{"password": "c2VjcmV0"},
	}}
	pod := newPod("web-0", "app")
	pod.Object["spec"] = map[string]any{
		"nodeName":   "node-a",
		"containers": []any{map[string]any{"name": "web", "imagePullPolicy": "Always"}},
	}
	pod.Object["metadata"].(map[string]any)["annotations"] = map[string]any{"a&b": "<x>"}

	cases := []GetParams{
		{Version: "v1", ResourceType: "pods"},
		{Version: "v1", ResourceType: "pods,secrets", Namespace: "app"},
		{Version: "v1", ResourceType: "secrets"},
		{Version: "v1", ResourceType: "pods", Namespace: "empty"},
	}
	for _, p := range cases {
		t.Run(p.ResourceType+"/"+p.Namespace, func(t *testing.T) {
			c := newFakeDynamic(t, pod.DeepCopy(), secret.DeepCopy(), newPod("db-0", "data"))
			old, err := c.GetResource(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(SnakeKeysDeep(old))
			if err != nil {
				t.Fatal(err)
			}
			got, err := newFakeDynamic(t, pod.DeepCopy(), secret.DeepCopy(), newPod("db-0", "data")).GetResourceJSON(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("GetResourceJSON =\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// A cut list would read as deletions to callers that treat it as complete, so
// over the cap the call fails instead.
func TestGetResourceJSON_FailsOverTheCapRatherThanTruncating(t *testing.T) {
	var pods []runtime.Object
	for i := range 20 {
		pods = append(pods, newPod(fmt.Sprintf("pod-%02d", i), "app"))
	}
	c := newFakeDynamic(t, pods...)
	c.MaxResultBytes = 1000

	got, err := c.GetResourceJSON(context.Background(), GetParams{Version: "v1", ResourceType: "pods,services"})
	if !errors.Is(err, ErrResultTooLarge) {
		t.Fatalf("err = %v; want ErrResultTooLarge", err)
	}
	if got != nil {
		t.Errorf("returned %d bytes alongside the error; want none", len(got))
	}
	if !strings.Contains(err.Error(), "label_selector") {
		t.Errorf("error %q does not say how to narrow the request", err)
	}
}

// With several types asked for, one the cluster does not serve (a CRD that is
// not installed, which the API server answers with 404) is left out and the
// rest still come back.
func TestGetResourceJSON_SkipsATypeTheClusterDoesNotServe(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	dyn := dynamicfake.NewSimpleDynamicClient(scheme, newPod("keep", "app"))
	dyn.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "")
	})
	c := NewClient(dyn, nil)

	got, err := c.GetResourceJSON(context.Background(), GetParams{Version: "v1", ResourceType: "configmaps,pods"})
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	if err := json.Unmarshal(got, &items); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, got)
	}
	if len(items) != 1 || items[0]["metadata"].(map[string]any)["name"] != "keep" {
		t.Errorf("items = %s; want only the pod", got)
	}
}

// Any other failure of one of several types fails the request: returning the
// rest as the whole answer is how an RBAC check reading "roles,rolebindings"
// passed with rolebindings forbidden.
func TestGetResourceJSON_FailsWhenOneOfSeveralTypesFails(t *testing.T) {
	for name, listErr := range map[string]error{
		"forbidden":    apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "", errors.New("no list")),
		"server error": apierrors.NewInternalError(errors.New("etcd went away")),
		"timeout":      apierrors.NewTimeoutError("slow", 1),
	} {
		t.Run(name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			dyn := dynamicfake.NewSimpleDynamicClient(scheme, newPod("keep", "app"))
			dyn.PrependReactor("list", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, listErr
			})
			c := NewClient(dyn, nil)

			got, err := c.GetResourceJSON(context.Background(), GetParams{Version: "v1", ResourceType: "pods,configmaps"})
			if err == nil {
				t.Fatalf("got %s and no error; want the configmaps failure reported", got)
			}
			if !strings.Contains(err.Error(), "configmaps") {
				t.Errorf("err = %v; want it to name the type that failed", err)
			}
		})
	}
}

func TestGetResource_SelectsOneNameServerSide(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	dyn := dynamicfake.NewSimpleDynamicClient(scheme, newPod("web-0", "app"), newPod("web-1", "app"))
	c := NewClient(dyn, nil)

	got, err := c.GetResourceJSON(context.Background(), ParseGetParams(map[string]any{
		"version": "v1", "resource_type": "pods", "namespace": "app", "name": []any{"web-1"},
		"field_selector": "status.phase=Running",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"web-1"`) || strings.Contains(string(got), `"web-0"`) {
		t.Errorf("result %s; want web-1 only", got)
	}
	la := dyn.Actions()[0].(k8stesting.ListAction)
	if sel := la.GetListRestrictions().Fields.String(); sel != "metadata.name=web-1,status.phase=Running" {
		t.Errorf("field selector %q; want the name added to the caller's", sel)
	}
}

// An API that rejects the name field selector still answers, filtered here the
// way every lookup by name used to be.
func TestGetResource_NameFallsBackWhenFieldSelectorIsRejected(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	dyn := dynamicfake.NewSimpleDynamicClient(scheme, newPod("web-0", "app"), newPod("web-1", "app"))
	dyn.PrependReactor("list", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !a.(k8stesting.ListAction).GetListRestrictions().Fields.Empty() {
			return true, nil, apierrors.NewBadRequest(`field label not supported: metadata.name`)
		}
		return false, nil, nil
	})
	c := NewClient(dyn, nil)

	got, err := c.GetResourceJSON(context.Background(), ParseGetParams(map[string]any{
		"version": "v1", "resource_type": "pods", "namespace": "app", "name": "web-1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"web-1"`) || strings.Contains(string(got), `"web-0"`) {
		t.Errorf("result %s; want web-1 only", got)
	}
}

// gatedDynamic blocks cluster-wide lists on `gate` and counts how many are in
// flight. It wraps the fake rather than using a reactor: the fake holds one
// lock while any reactor runs, which would block the namespaced list too.
type gatedDynamic struct {
	dynamic.Interface
	gate           chan struct{}
	mu             sync.Mutex
	inFlight, peak int
}

func (d *gatedDynamic) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return gatedResource{NamespaceableResourceInterface: d.Interface.Resource(gvr), d: d}
}

type gatedResource struct {
	dynamic.NamespaceableResourceInterface
	d *gatedDynamic
}

func (r gatedResource) List(ctx context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	r.d.mu.Lock()
	r.d.inFlight++
	r.d.peak = max(r.d.peak, r.d.inFlight)
	r.d.mu.Unlock()
	<-r.d.gate
	r.d.mu.Lock()
	r.d.inFlight--
	r.d.mu.Unlock()
	return r.NamespaceableResourceInterface.List(ctx, opts)
}

// Cluster-wide lists queue behind the limit; scoped ones do not wait for them.
func TestGetResource_BoundsConcurrentClusterWideLists(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	dyn := &gatedDynamic{Interface: dynamicfake.NewSimpleDynamicClient(scheme, newPod("web-0", "app")), gate: make(chan struct{})}
	c := NewClient(dyn, nil)
	c.SetUnscopedListLimit(1)

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.GetResourceJSON(context.Background(), GetParams{Version: "v1", ResourceType: "pods"}); err != nil {
				t.Error(err)
			}
		}()
	}

	// A namespaced list goes straight through the gated resource's Namespace(),
	// which returns the fake's own interface, so only the limit could hold it.
	done := make(chan error, 1)
	go func() {
		_, err := c.GetResourceJSON(context.Background(), GetParams{Version: "v1", ResourceType: "pods", Namespace: "app"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a namespaced list waited behind cluster-wide ones")
	}

	// Let the cluster-wide lists through one at a time.
	go func() {
		for range 3 {
			dyn.gate <- struct{}{}
		}
	}()
	wg.Wait()
	if dyn.peak != 1 {
		t.Errorf("peak concurrent cluster-wide lists = %d; want 1", dyn.peak)
	}
}

// A caller that gives up while queued gets an error, not a hang.
func TestGetResource_QueuedListHonoursCancellation(t *testing.T) {
	c := newFakeDynamic(t, newPod("web-0", "app"))
	c.SetUnscopedListLimit(1)
	hold, err := c.acquireList(context.Background(), GetParams{Version: "v1", ResourceType: "pods"})
	if err != nil {
		t.Fatal(err)
	}
	defer hold()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.GetResourceJSON(ctx, GetParams{Version: "v1", ResourceType: "pods"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v; want a deadline error", err)
	}
}

// clusterScopedFake answers a namespaced list of nodes with 404, as the API
// server does for a cluster-scoped kind, and counts the lists that went out.
func clusterScopedFake(t *testing.T) (*Client, *int) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{{Version: "v1", Resource: "nodes"}: "NodeList"})
	lists := 0
	dyn.PrependReactor("list", "nodes", func(a k8stesting.Action) (bool, runtime.Object, error) {
		lists++
		if a.GetNamespace() != "" {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, "")
		}
		return false, nil, nil
	})
	return NewClient(dyn, nil), &lists
}

// A cluster-scoped kind asked for with a namespace falls back to a
// cluster-wide list, so that fallback takes a slot like any other.
func TestGetResource_ClusterScopedFallbackTakesASlot(t *testing.T) {
	c, lists := clusterScopedFake(t)
	c.SetUnscopedListLimit(1)
	hold, err := c.acquireList(context.Background(), GetParams{Version: "v1", ResourceType: "pods"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = c.GetResourceJSON(ctx, GetParams{Version: "v1", ResourceType: "nodes", Namespace: "default"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want the fallback to wait for the held slot", err)
	}
	if *lists != 1 {
		t.Errorf("lists = %d; want only the namespaced attempt while the slot is held", *lists)
	}

	hold()
	if _, err := c.GetResourceJSON(context.Background(), GetParams{Version: "v1", ResourceType: "nodes", Namespace: "default"}); err != nil {
		t.Fatalf("after the slot frees: %v", err)
	}
}

// A request that already holds the slot (all_namespaces with a namespace)
// must not wait for a second one in the fallback: with a limit of 1 that
// would never return. A type that is not installed answers 404 to every list,
// so it reaches the fallback while holding the slot.
func TestGetResource_FallbackDoesNotTakeASecondSlot(t *testing.T) {
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{{Group: "example.com", Version: "v1", Resource: "widgets"}: "WidgetList"})
	lists := 0
	dyn.PrependReactor("list", "widgets", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists++
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "example.com", Resource: "widgets"}, "")
	})
	c := NewClient(dyn, nil)
	c.SetUnscopedListLimit(1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := GetParams{Group: "example.com", Version: "v1", ResourceType: "widgets", Namespace: "default", AllNamespaces: true}
	_, err := c.GetResourceJSON(ctx, p)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("the fallback waited for a second slot")
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("err = %v; want the 404", err)
	}
	if lists != 2 {
		t.Errorf("lists = %d; want the first attempt and the fallback", lists)
	}
}
