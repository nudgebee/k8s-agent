package kube

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// api-server sends namespace and name as JSON arrays. Reading only the string
// form dropped both, so every one of its get_resource calls listed the whole
// cluster and left the filtering to the caller — which for job_failure findings
// meant the first failing pod anywhere was captioned as the Job's own output
// (98% of them on dev, 99.6% on prod, measured 2026-09-23).
func TestParseGetParams_ListShapedNamespaceAndName(t *testing.T) {
	got := ParseGetParams(map[string]any{
		"version": "v1", "resource_type": "pods",
		"namespace":      []any{"benchmark-scenarios"},
		"name":           []any{},
		"label_selector": "job-name=nightly-report-29836288",
	})

	if got.Namespace != "benchmark-scenarios" {
		t.Errorf("Namespace = %q; want benchmark-scenarios — an unscoped list is cluster-wide", got.Namespace)
	}
	if got.Name != "" {
		t.Errorf("Name = %q; want empty", got.Name)
	}
	if got.LabelSelector != "job-name=nightly-report-29836288" {
		t.Errorf("LabelSelector = %q; want it forwarded to the server", got.LabelSelector)
	}
}

func TestParseGetParams_ParamShapes(t *testing.T) {
	cases := []struct {
		name    string
		in      any
		wantOne string
		wantAll []string
	}{
		{"bare string", "demo", "demo", []string{"demo"}},
		{"single-element list", []any{"demo"}, "demo", []string{"demo"}},
		{"go string slice", []string{"demo"}, "demo", []string{"demo"}},
		{"empty list", []any{}, "", []string{}},
		{"blank entry", []any{""}, "", []string{}},
		// Several namespaces cannot scope one request; listOne filters those after.
		{"multiple", []any{"a", "b"}, "", []string{"a", "b"}},
		{"absent", nil, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseGetParams(map[string]any{"version": "v1", "resource_type": "pods", "namespace": tc.in})
			if got.Namespace != tc.wantOne {
				t.Errorf("Namespace = %q; want %q", got.Namespace, tc.wantOne)
			}
			if !reflect.DeepEqual(got.Namespaces, tc.wantAll) {
				t.Errorf("Namespaces = %#v; want %#v", got.Namespaces, tc.wantAll)
			}
		})
	}
}

// Parsing the scope is only half of it — it has to reach the API server, or the
// agent is still listing the cluster and filtering afterwards.
func TestGetResource_ScopesListToNamespaceAndSelector(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	dyn := dynamicfake.NewSimpleDynamicClient(scheme,
		newPod("nightly-report-29836288-q74hb", "benchmark-scenarios"),
		newPod("memory-hog-7b565497b8-hbq8q", "demo"),
	)
	c := NewClient(dyn, nil)

	if _, err := c.GetResource(context.Background(), ParseGetParams(map[string]any{
		"version": "v1", "resource_type": "pods",
		"namespace":      []any{"benchmark-scenarios"},
		"label_selector": "job-name=nightly-report-29836288",
	})); err != nil {
		t.Fatal(err)
	}

	var listed bool
	for _, a := range dyn.Actions() {
		la, ok := a.(k8stesting.ListAction)
		if !ok {
			continue
		}
		listed = true
		if ns := la.GetNamespace(); ns != "benchmark-scenarios" {
			t.Errorf("listed namespace %q; want benchmark-scenarios", ns)
		}
		if sel := la.GetListRestrictions().Labels.String(); sel != "job-name=nightly-report-29836288" {
			t.Errorf("label selector %q; want it sent to the server", sel)
		}
	}
	if !listed {
		t.Fatal("no list action recorded")
	}
}

// More namespaces than one request can be scoped to: the filter still holds.
func TestGetResource_FiltersMultipleNamespacesClientSide(t *testing.T) {
	c := newFakeDynamic(t, newPod("a", "ns1"), newPod("b", "ns2"), newPod("c", "ns3"))

	got, err := c.GetResource(context.Background(), ParseGetParams(map[string]any{
		"version": "v1", "resource_type": "pods", "namespace": []any{"ns1", "ns3"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if arr := got.([]any); len(arr) != 2 {
		t.Fatalf("len = %d; want 2 (ns1 + ns3, not ns2)", len(arr))
	}
}
