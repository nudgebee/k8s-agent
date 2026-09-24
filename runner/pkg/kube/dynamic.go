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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	Group        string
	Version      string
	ResourceType string // singular resource name; for plural lists, may be comma-separated
	// Namespace and Name hold the single-value case, which is what scopes the
	// request server-side. Namespaces/Names hold everything the caller asked for
	// and are filtered post-list when there is more than one.
	Namespace     string
	Namespaces    []string
	Name          string
	Names         []string
	LabelSelector string
	FieldSelector string
	AllNamespaces bool
}

func ParseGetParams(p map[string]any) GetParams {
	namespaces := strListParam(p, "namespace")
	names := strListParam(p, "name")
	gp := GetParams{
		Group:        strParam(p, "group"),
		Version:      strParam(p, "version"),
		ResourceType: strParam(p, "resource_type"),
		Namespaces:   namespaces,
		Names:        names,
		// api-server sends these as JSON arrays (`"namespace": ["demo"]`), the UI
		// and the legacy kind-based contract as bare strings. Only the string form
		// was ever read — `m[k].(string)` fails on a []any and yields "" — so every
		// api-server call listed the whole cluster and leaned on client-side
		// filtering. That is how a Job in `nudgebee` came back captioned with a
		// `demo`-namespace pod's logs (98% of job_failure findings on dev,
		// 99.6% on prod, measured 2026-09-23).
		Namespace:     single(namespaces),
		Name:          single(names),
		LabelSelector: strParam(p, "label_selector"),
		FieldSelector: strParam(p, "field_selector"),
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
	opts := metav1.ListOptions{LabelSelector: p.LabelSelector, FieldSelector: p.FieldSelector}
	list, err := c.resourceInterface(gvr, p).List(ctx, opts)
	// A cluster-scoped resource asked for with a namespace is a 404, not an empty
	// list: listing a namespace that does not exist returns no items, so NotFound
	// here means the scope is wrong rather than the data missing. Callers pass a
	// namespace freely (the generic k8s_resource action forwards whatever a
	// playbook wrote), and that used to be harmless because the namespace was
	// dropped — so retry the way it used to be served rather than regress them.
	if err != nil && p.Namespace != "" && apierrors.IsNotFound(err) {
		cluster := p
		cluster.Namespace = ""
		list, err = c.resourceInterface(gvr, cluster).List(ctx, opts)
	}
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", gvr.Resource, err)
	}
	out := make([]any, 0, len(list.Items))
	for _, item := range list.Items {
		// The server scoped the request when exactly one name/namespace was asked
		// for; these filters cover the rest, and the single case costs one
		// comparison against a value that already matches.
		if len(p.Names) > 0 && !contains(p.Names, item.GetName()) {
			continue
		}
		if len(p.Namespaces) > 0 && item.GetNamespace() != "" && !contains(p.Namespaces, item.GetNamespace()) {
			continue
		}
		out = append(out, item.UnstructuredContent())
	}
	return out, nil
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
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

// strListParam reads a param that callers write either as a string or as a list
// of strings. JSON decoding gives []any, and a Go caller inside the agent may
// pass []string; both mean the same thing. Empty and blank entries are dropped so
// `"namespace": [""]` does not scope a request to a namespace called "".
func strListParam(m map[string]any, k string) []string {
	if m == nil {
		return nil
	}
	var raw []any
	switch v := m[k].(type) {
	case nil:
		return nil
	case string:
		raw = []any{v}
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		raw = v
	default:
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// single returns the only value, or "" when the caller asked for none or for
// several — several cannot scope one request, so listOne filters those post-list.
func single(values []string) string {
	if len(values) == 1 {
		return values[0]
	}
	return ""
}

func boolParam(m map[string]any, k string) bool {
	if m == nil {
		return false
	}
	b, _ := m[k].(bool)
	return b
}
