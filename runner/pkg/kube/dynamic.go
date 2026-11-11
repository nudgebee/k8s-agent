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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sync/semaphore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	// MaxResultBytes caps the JSON GetResourceJSON returns; 0 means no cap.
	MaxResultBytes int
	// unscoped bounds how many lists not scoped to a namespace, name or selector
	// run at once; nil means no bound. Set with SetUnscopedListLimit.
	unscoped *semaphore.Weighted
}

func NewClient(dyn dynamic.Interface, typed kubernetes.Interface) *Client {
	return &Client{Dynamic: dyn, Typed: typed}
}

// SetUnscopedListLimit bounds concurrent cluster-wide lists to n; n <= 0
// removes the bound. Call before the client is shared.
func (c *Client) SetUnscopedListLimit(n int) {
	if n <= 0 {
		c.unscoped = nil
		return
	}
	c.unscoped = semaphore.NewWeighted(int64(n))
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
//
// This holds the whole result as maps, several times its JSON size. The
// get_resource action uses GetResourceJSON instead; this stays for callers that
// need the objects themselves.
func (c *Client) GetResource(ctx context.Context, p GetParams) (any, error) {
	out := make([]any, 0, 16)
	err := c.eachResult(ctx, p, func(obj map[string]any) error {
		out = append(out, obj)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetResourceJSON returns what GetResource returns, with SnakeKeysDeep applied,
// as the JSON array the get_resource action sends — byte for byte what
// json.Marshal(SnakeKeysDeep(GetResource(...))) produced.
//
// Each listed object is converted and encoded on its own, so there is no
// snake_case copy of the whole list, and the size is known while listing —
// which is what lets MaxResultBytes stop a call before it is all encoded. The
// decoded list itself is still held whole: a cluster-wide pod list of 11 MB of
// JSON peaks near 120 MB either way. What bounds a burst of them is the
// unscoped-list limit (acquireList).
//
// The list is one request. Paging it (Limit/Continue) lowered that peak but
// made every list 2-4x slower on a v1.35 cluster, one-page lists included: a
// list with a limit is not served from the API server's cache.
//
// Over MaxResultBytes it stops and returns ErrResultTooLarge rather than a
// partial list: callers that treat what comes back as the complete set would
// read a silently cut list as deletions.
func (c *Client) GetResourceJSON(ctx context.Context, p GetParams) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	err := c.eachResult(ctx, p, func(obj map[string]any) error {
		b, err := json.Marshal(SnakeKeysDeep(obj))
		if err != nil {
			return err
		}
		if c.MaxResultBytes > 0 && buf.Len()+len(b)+2 > c.MaxResultBytes {
			return fmt.Errorf("%w: over %d bytes; scope it with a namespace, name or label_selector",
				ErrResultTooLarge, c.MaxResultBytes)
		}
		if buf.Len() > 1 { // past the opening bracket: not the first item
			buf.WriteByte(',')
		}
		buf.Write(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

// ErrResultTooLarge is returned when a get_resource result would exceed
// Client.MaxResultBytes.
var ErrResultTooLarge = errors.New("kube: result too large")

// eachResult lists every resource type in p and hands each matching object to
// fn.
func (c *Client) eachResult(ctx context.Context, p GetParams, fn func(map[string]any) error) error {
	if c.Dynamic == nil {
		return errors.New("kube: dynamic client not configured")
	}
	if p.Version == "" || p.ResourceType == "" {
		return errors.New("kube: version and resource_type are required")
	}
	release, err := c.acquireList(ctx, p)
	if err != nil {
		return err
	}
	defer release()

	types := splitCSV(p.ResourceType)
	for _, t := range types {
		err := c.eachItem(ctx, schema.GroupVersionResource{
			Group: p.Group, Version: p.Version, Resource: t,
		}, p, fn)
		// Tolerate a type whose list fails when caller asked for multiple types
		// — it contributed nothing; continue. Return error otherwise. An error
		// from fn (a result over the size cap) is never tolerated.
		if err != nil && (len(types) == 1 || errors.Is(err, ErrResultTooLarge) || ctx.Err() != nil) {
			return err
		}
	}
	return nil
}

// acquireList takes a slot for a list that is not scoped to a namespace, name
// or selector. Those are the lists that return a whole cluster's worth of
// objects, and several arriving at once is what pushed the runner's heap from
// ~60 MB to ~1 GB: each one peaks near 10x its JSON size. Ten cluster-wide pod
// lists at once peaked at 1.7 GB unbounded and 220 MB with a limit of 2.
// Scoped lists are small and never wait.
func (c *Client) acquireList(ctx context.Context, p GetParams) (func(), error) {
	if c.unscoped == nil || p.isScoped() {
		return func() {}, nil
	}
	if err := c.unscoped.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("kube: waiting for a cluster-wide list slot: %w", err)
	}
	return func() { c.unscoped.Release(1) }, nil
}

// isScoped reports whether the API server narrows the list: one namespace
// (and not all_namespaces), a name, or a selector.
func (p GetParams) isScoped() bool {
	return (p.Namespace != "" && !p.AllNamespaces) || p.Name != "" || p.LabelSelector != "" || p.FieldSelector != ""
}

// eachItem lists a single GVR and hands each object that passes the
// name/namespace filters to fn. A list error returns before fn is called.
func (c *Client) eachItem(ctx context.Context, gvr schema.GroupVersionResource, p GetParams, fn func(map[string]any) error) error {
	opts := metav1.ListOptions{LabelSelector: p.LabelSelector, FieldSelector: p.FieldSelector}
	// One name is selected server-side. Without this a lookup by name listed the
	// whole namespace — or the cluster, when no namespace came with it — and
	// threw away everything but one object.
	if p.Name != "" {
		opts.FieldSelector = joinSelector(p.FieldSelector, "metadata.name="+p.Name)
	}
	ri := c.resourceInterface(gvr, p)
	list, err := ri.List(ctx, opts)
	// A cluster-scoped resource asked for with a namespace is a 404, not an empty
	// list, so NotFound here means the scope is wrong rather than the data
	// missing. The API server does not check namespace existence on a collection
	// request — `GET /api/v1/namespaces/<missing>/pods` answers 200 with an empty
	// PodList, while `GET /api/v1/namespaces/default/nodes` is the 404 (verified
	// against a live cluster). So this cannot fire for a namespace that is merely
	// absent, and the retry is not a cluster-wide list on a typo. Callers pass a
	// namespace freely (the generic k8s_resource action forwards whatever a
	// playbook wrote), and that used to be harmless because the namespace was
	// dropped — so retry the way it used to be served rather than regress them.
	if err != nil && p.Namespace != "" && apierrors.IsNotFound(err) {
		cluster := p
		cluster.Namespace = ""
		// The retry can list the whole cluster. Take a slot for it unless the
		// request already holds one (acquireList skips only scoped requests):
		// taking a second would deadlock a limit of 1.
		if p.isScoped() {
			release, err := c.acquireList(ctx, cluster)
			if err != nil {
				return err
			}
			defer release()
		}
		ri = c.resourceInterface(gvr, cluster)
		list, err = ri.List(ctx, opts)
	}
	// Aggregated APIs need not support field selectors. Fall back to the name
	// filter below, which is how every lookup by name used to be served.
	if err != nil && p.Name != "" && apierrors.IsBadRequest(err) {
		opts.FieldSelector = p.FieldSelector
		list, err = ri.List(ctx, opts)
	}
	if err != nil {
		return fmt.Errorf("list %s: %w", gvr.Resource, err)
	}
	for i := range list.Items {
		item := &list.Items[i]
		// The server scoped the request when exactly one name/namespace was
		// asked for; these filters cover the rest, and the single case costs one
		// comparison against a value that already matches.
		if len(p.Names) > 0 && !contains(p.Names, item.GetName()) {
			continue
		}
		if len(p.Namespaces) > 0 && item.GetNamespace() != "" && !contains(p.Namespaces, item.GetNamespace()) {
			continue
		}
		obj := item.UnstructuredContent()
		// Secret values never leave the cluster: callers get keys, not values.
		if gvr.Group == "" && gvr.Resource == "secrets" {
			redactSecretData(obj)
		}
		if err := fn(obj); err != nil {
			return err
		}
		// Done with this object: drop the list's reference so what fn already
		// encoded can be collected while the rest is processed.
		list.Items[i] = unstructured.Unstructured{}
	}
	return nil
}

func joinSelector(a, b string) string {
	if a == "" {
		return b
	}
	return a + "," + b
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
