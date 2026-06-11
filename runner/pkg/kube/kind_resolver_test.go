package kube

import "testing"

func TestResolveKind(t *testing.T) {
	cases := []struct {
		in       string
		ok       bool
		group    string
		version  string
		resource string
	}{
		{"Deployment", true, "apps", "v1", "deployments"},
		{"deployment", true, "apps", "v1", "deployments"}, // case-insensitive
		{" Deployment ", true, "apps", "v1", "deployments"},
		{"Pod", true, "", "v1", "pods"},
		{"ConfigMap", true, "", "v1", "configmaps"},
		{"NetworkPolicy", true, "networking.k8s.io", "v1", "networkpolicies"},
		{"Rollout", true, "argoproj.io", "v1alpha1", "rollouts"},
		{"", false, "", "", ""},
		{"NoSuchKind", false, "", "", ""},
	}
	for _, c := range cases {
		gvr, ok := resolveKind(c.in)
		if ok != c.ok {
			t.Errorf("resolveKind(%q) ok = %v; want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if gvr.Group != c.group || gvr.Version != c.version || gvr.Resource != c.resource {
			t.Errorf("resolveKind(%q) = %s/%s/%s; want %s/%s/%s",
				c.in, gvr.Group, gvr.Version, gvr.Resource, c.group, c.version, c.resource)
		}
	}
}

// The exact failing call from the field: {name, namespace, kind:"Deployment"}
// must resolve to the apps/v1/deployments GVR instead of erroring.
func TestParseGetParams_KindResolvesToGVR(t *testing.T) {
	got := ParseGetParams(map[string]any{
		"name":      "services-server",
		"namespace": "nudgebee",
		"kind":      "Deployment",
	})
	want := GetParams{
		Group: "apps", Version: "v1", ResourceType: "deployments",
		Namespace: "nudgebee", Name: "services-server",
	}
	if got != want {
		t.Errorf("ParseGetParams kind path\n got:  %+v\n want: %+v", got, want)
	}
}

// An explicit resource_type must win — kind is ignored when GVR is given.
func TestParseGetParams_ExplicitResourceTypeWins(t *testing.T) {
	got := ParseGetParams(map[string]any{
		"group":         "apps",
		"version":       "v1",
		"resource_type": "statefulsets",
		"kind":          "Deployment", // should be ignored
		"name":          "db",
	})
	if got.ResourceType != "statefulsets" {
		t.Errorf("explicit resource_type should win, got %q", got.ResourceType)
	}
}

// A caller-provided version is preserved over the table's canonical one when
// resolving by kind (only resource_type is unconditionally filled).
func TestParseGetParams_KindPreservesExplicitVersion(t *testing.T) {
	got := ParseGetParams(map[string]any{
		"kind":    "HorizontalPodAutoscaler",
		"version": "v1", // table canonical is v2; caller override must stick
		"name":    "hpa",
	})
	if got.Version != "v1" {
		t.Errorf("explicit version should be preserved, got %q", got.Version)
	}
	if got.ResourceType != "horizontalpodautoscalers" {
		t.Errorf("resource_type should be filled from kind, got %q", got.ResourceType)
	}
}

// An unknown kind with no explicit GVR leaves resource_type empty so the
// downstream "version and resource_type are required" error still fires.
func TestParseGetParams_UnknownKindLeavesEmpty(t *testing.T) {
	got := ParseGetParams(map[string]any{
		"kind": "SomeRandomCRD",
		"name": "x",
	})
	if got.ResourceType != "" {
		t.Errorf("unknown kind should not resolve, got resource_type %q", got.ResourceType)
	}
}
