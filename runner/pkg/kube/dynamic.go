// Package kube implements the K8s read primitives the agent exposes over
// the relay (Group B in the deprecation plan). Backend composers (the new
// api-server enrichers) call these to assemble higher-level findings.
//
// Actions:
//   - get_resource          : fetch one resource as JSON, or a list of them
//   - get_resource_yaml     : same, but YAML
//   - list_resource_names   : just names + namespaces
//   - kubectl_command_executor : generic kubectl runner (see exec.go)
//
// Implementation choice: dynamic client (no compile-time type knowledge of
// every K8s resource). The action params name the GVR explicitly:
//
//	{group: "rbac.authorization.k8s.io", version: "v1",
//	 resource_type: "roles,rolebindings", all_namespaces: true}
package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// Client wraps the dynamic client + a typed clientset for the operations
// (logs, exec) that don't fit the dynamic API.
type Client struct {
	Dynamic dynamic.Interface
	Typed   kubernetes.Interface
}

func NewClient(dyn dynamic.Interface, typed kubernetes.Interface) *Client {
	return &Client{Dynamic: dyn, Typed: typed}
}

// GetParams parses the action_params shape used by the get_resource action
// . resource_type may be a
// comma-separated list ("roles,rolebindings"); all_namespaces toggles cluster
// scope; namespace+name select a single resource.
type GetParams struct {
	Group         string
	Version       string
	ResourceType  string // singular resource name; for plural lists, may be comma-separated
	Namespace     string
	Name          string
	AllNamespaces bool
}

func ParseGetParams(p map[string]any) GetParams {
	gp := GetParams{
		Group:         strParam(p, "group"),
		Version:       strParam(p, "version"),
		ResourceType:  strParam(p, "resource_type"),
		Namespace:     strParam(p, "namespace"),
		Name:          strParam(p, "name"),
		AllNamespaces: boolParam(p, "all_namespaces"),
	}
	// Backward-compat with the legacy `kind`-based contract (and the UI built
	// on it): callers that address a resource by `kind` ("Deployment") instead
	// of an explicit GVR used to work, but requiring resource_type made
	// {name, namespace, kind} fail with "version and resource_type are
	// required". When resource_type is absent, resolve the kind to its
	// canonical GVR (see kind_resolver.go). An explicit GVR always wins:
	// resource_type set ⇒ kind ignored; an explicitly-provided group/version
	// is preserved over the table's canonical one.
	if gp.ResourceType == "" {
		if gvr, ok := resolveKind(strParam(p, "kind")); ok {
			gp.ResourceType = gvr.Resource
			if gp.Group == "" {
				gp.Group = gvr.Group
			}
			if gp.Version == "" {
				gp.Version = gvr.Version
			}
		}
	}
	return gp
}

// GetResource fetches resources matching p and returns them as a FLAT array
// of unstructured objects.
//
// Shape contract: always `[]any` of `map[string]any`, never the K8s List
// wrapper `{kind, apiVersion, items, metadata}`. UI callers like
// KubernetesPV.jsx:58 do `data.map(item => ...)` directly on the result;
// returning the wrapper makes every list-typed call render an empty table.
//
// When `name` is set, lists then filters by name — so name-based lookup
// returns `[obj]`, not the bare object.
//
// When `resource_type` is comma-separated, the per-type results are
// concatenated into one flat array, not a `{kind: [...]}` map.
func (c *Client) GetResource(ctx context.Context, p GetParams) (any, error) {
	if c.Dynamic == nil {
		return nil, errors.New("kube: dynamic client not configured")
	}
	if p.Version == "" || p.ResourceType == "" {
		return nil, errors.New("kube: version and resource_type are required")
	}

	types := splitCSV(p.ResourceType)
	out := make([]any, 0, 16)
	for _, t := range types {
		items, err := c.listOne(ctx, schema.GroupVersionResource{
			Group: p.Group, Version: p.Version, Resource: t,
		}, p)
		if err != nil {
			// Tolerate per-type errors when caller asked for multiple types
			// — log and continue. Return error otherwise.
			if len(types) == 1 {
				return nil, err
			}
			continue
		}
		out = append(out, items...)
	}
	return out, nil
}

// listOne lists a single GVR and returns its items as []any. If p.Name is set,
// the items are filtered by name post-list.
func (c *Client) listOne(ctx context.Context, gvr schema.GroupVersionResource, p GetParams) ([]any, error) {
	list, err := c.resourceInterface(gvr, p).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", gvr.Resource, err)
	}
	out := make([]any, 0, len(list.Items))
	for _, item := range list.Items {
		if p.Name != "" && item.GetName() != p.Name {
			continue
		}
		out = append(out, item.UnstructuredContent())
	}
	return out, nil
}

func (c *Client) resourceInterface(gvr schema.GroupVersionResource, p GetParams) dynamic.ResourceInterface {
	if p.Namespace != "" && !p.AllNamespaces {
		return c.Dynamic.Resource(gvr).Namespace(p.Namespace)
	}
	if p.AllNamespaces || p.Namespace == "" {
		return c.Dynamic.Resource(gvr)
	}
	return c.Dynamic.Resource(gvr).Namespace(p.Namespace)
}

// GetResourceYAML returns the same data as GetResource, marshaled as YAML.
func (c *Client) GetResourceYAML(ctx context.Context, p GetParams) ([]byte, error) {
	got, err := c.GetResource(ctx, p)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(got)
}

// ListResourceNames returns just the names (and namespaces, if applicable)
// of the resources matching p. Useful for quick existence checks.
type NamedResource struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

func (c *Client) ListResourceNames(ctx context.Context, p GetParams) ([]NamedResource, error) {
	if c.Dynamic == nil {
		return nil, errors.New("kube: dynamic client not configured")
	}
	if p.Version == "" || p.ResourceType == "" {
		return nil, errors.New("kube: version and resource_type are required")
	}

	gvr := schema.GroupVersionResource{Group: p.Group, Version: p.Version, Resource: p.ResourceType}
	list, err := c.resourceInterface(gvr, p).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]NamedResource, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, NamedResource{Name: item.GetName(), Namespace: item.GetNamespace()})
	}
	return out, nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func strParam(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

func boolParam(m map[string]any, k string) bool {
	if m == nil {
		return false
	}
	b, _ := m[k].(bool)
	return b
}
