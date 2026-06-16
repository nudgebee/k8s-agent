package mutate

import (
	"context"
	"testing"

	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestScaleWorkload_Deployment_ScaleToZero(t *testing.T) {
	existing := seedExisting("apps/v1", "Deployment", "web", "shop", "100")
	dyn := fake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(k8sfake.NewClientset(), "", nil)
	m.SetDynamic(dyn)

	got, err := m.ScaleWorkload(context.Background(), "Deployment", "shop", "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	r := got.(map[string]any)
	if r["success"] != true {
		t.Errorf("success = %v", r["success"])
	}
	updated := r["updated"].(map[string]any)
	spec := updated["spec"].(map[string]any)
	if got := asInt64(spec["replicas"]); got != 0 {
		t.Errorf("replicas = %v; want 0", got)
	}
}

// asInt64 normalises the replicas value, which the dynamic fake's Patch echoes
// as int64 while the JSON wire path would surface it as float64.
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	default:
		return -1
	}
}

func TestHandleReplicaRightsizing_StringReplicaCount(t *testing.T) {
	// event-resolution path sends replica_count as a string.
	existing := seedExisting("apps/v1", "StatefulSet", "db", "data", "5")
	dyn := fake.NewSimpleDynamicClient(workloadScheme(), existing)
	m := New(k8sfake.NewClientset(), "", nil)
	m.SetDynamic(dyn)
	hs := Handlers(m)

	got, err := hs["replica_rightsizing"](context.Background(), map[string]any{
		"kind":          "StatefulSet",
		"namespace":     "data",
		"name":          "db",
		"replica_count": "3",
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := got.(map[string]any)["updated"].(map[string]any)["spec"].(map[string]any)
	if got := asInt64(spec["replicas"]); got != 3 {
		t.Errorf("replicas = %v; want 3", got)
	}
}

func TestHandleReplicaRightsizing_UnsupportedKind(t *testing.T) {
	m := New(k8sfake.NewClientset(), "", nil)
	m.SetDynamic(fake.NewSimpleDynamicClient(workloadScheme()))
	hs := Handlers(m)
	_, err := hs["replica_rightsizing"](context.Background(), map[string]any{
		"kind": "DaemonSet", "namespace": "n", "name": "x", "replica_count": float64(2),
	})
	if err == nil {
		t.Fatal("expected error for DaemonSet (not scalable)")
	}
}

func TestHandleReplicaRightsizing_MissingReplicaCount(t *testing.T) {
	m := New(k8sfake.NewClientset(), "", nil)
	m.SetDynamic(fake.NewSimpleDynamicClient(workloadScheme()))
	hs := Handlers(m)
	_, err := hs["replica_rightsizing"](context.Background(), map[string]any{
		"kind": "Deployment", "namespace": "n", "name": "x",
	})
	if err == nil {
		t.Fatal("expected error for missing replica_count")
	}
}

func TestScaleWorkload_RequiresDynamic(t *testing.T) {
	m := New(k8sfake.NewClientset(), "", nil) // no SetDynamic
	if _, ok := Handlers(m)["replica_rightsizing"]; ok {
		t.Error("replica_rightsizing should not register without a dynamic client")
	}
}

func TestToInt64(t *testing.T) {
	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{float64(0), 0, true},
		{float64(3), 3, true},
		{"3", 3, true},
		{" 5 ", 5, true},
		{"", 0, false},
		{"abc", 0, false},
		{nil, 0, false},
		{int(7), 7, true},
	}
	for _, c := range cases {
		got, ok := toInt64(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("toInt64(%#v) = (%d,%v); want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
