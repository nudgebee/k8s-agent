package triggers

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func certificate(t *testing.T, dnsName, rv, status string) map[string]any {
	t.Helper()
	return mustObj(t, `{
		"apiVersion":"cert-manager.io/v1","kind":"Certificate",
		"metadata":{"name":"web","namespace":"shop","resourceVersion":"`+rv+`",
			"ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"web","uid":"u1"}]},
		"spec":{"secretName":"web-tls","dnsNames":["`+dnsName+`"]},
		"status":{"conditions":[{"type":"Ready","status":"`+status+`"}]}
	}`)
}

func customEngine(specs ...MatcherSpec) *Engine {
	return NewEngine(specs, time.Now().Add(-time.Hour))
}

func TestCustomResourceChange_FiresOnSpecChange(t *testing.T) {
	spec := CustomResourceChangeMatcher("Certificate", "cert-manager.io", nil)
	e := customEngine(spec)
	got := e.Match(IncomingK8sEvent{
		Operation: "update", Kind: "Certificate.cert-manager.io",
		OldObj: certificate(t, "web.example.com", "1", "True"),
		Obj:    certificate(t, "shop.example.com", "2", "True"),
	})
	if len(got) != 1 {
		t.Fatalf("want 1 match, got %d", len(got))
	}
	m := got[0]
	if m.SubjectKind != "certificate.cert-manager.io" {
		t.Errorf("subject kind must be group-qualified, got %q", m.SubjectKind)
	}
	if m.SubjectDisplayKind != "Certificate" || !m.Unattributed {
		t.Errorf("want display kind Certificate and unattributed, got %q / %v", m.SubjectDisplayKind, m.Unattributed)
	}
	// The Certificate's owner reference names a Deployment; an unattributed
	// change must not carry it, or the service key would tie it to that
	// workload.
	if m.Owner != (OwnerRef{}) {
		t.Errorf("unattributed match must have no owner, got %+v", m.Owner)
	}
	var diff EvidenceBlock
	for _, b := range m.ExtraBlocks {
		if b["type"] == "diff" {
			diff = b
		}
	}
	if diff == nil {
		t.Fatal("no diff block")
	}
	if rn := diff["data"].(map[string]any)["resource_name"]; rn != "certificate.cert-manager.io/shop/web.yaml" {
		t.Errorf("resource_name must be group-qualified, got %v", rn)
	}
}

func TestCustomResourceChange_IgnoresStatusOnlyUpdate(t *testing.T) {
	e := customEngine(CustomResourceChangeMatcher("Certificate", "cert-manager.io", nil))
	got := e.Match(IncomingK8sEvent{
		Operation: "update", Kind: "Certificate.cert-manager.io",
		OldObj: certificate(t, "web.example.com", "1", "False"),
		Obj:    certificate(t, "web.example.com", "2", "True"),
	})
	if len(got) != 0 {
		t.Fatalf("a status-only update (renewal) must not fire, got %d", len(got))
	}
}

func TestCustomResourceChange_DiffPathsLimitWhatFires(t *testing.T) {
	e := customEngine(CustomResourceChangeMatcher("Thing", "example.com", []string{"config"}))
	old := mustObj(t, `{"metadata":{"name":"a","namespace":"n"},"spec":{"x":1},"config":{"y":1}}`)
	specOnly := mustObj(t, `{"metadata":{"name":"a","namespace":"n"},"spec":{"x":2},"config":{"y":1}}`)
	if got := e.Match(IncomingK8sEvent{Operation: "update", Kind: "Thing.example.com", OldObj: old, Obj: specOnly}); len(got) != 0 {
		t.Fatalf("a change outside diffPaths must not fire, got %d", len(got))
	}
	cfg := mustObj(t, `{"metadata":{"name":"a","namespace":"n"},"spec":{"x":1},"config":{"y":2}}`)
	if got := e.Match(IncomingK8sEvent{Operation: "update", Kind: "Thing.example.com", OldObj: old, Obj: cfg}); len(got) != 1 {
		t.Fatalf("a change in diffPaths must fire, got %d", len(got))
	}
}

// A Knative Service is not a core Service: the core service_no_endpoints and
// the custom matcher must each see only their own kind.
func TestCustomResourceChange_KindCollisionWithBuiltin(t *testing.T) {
	if !kindMatches("Service", "Service") || kindMatches("Service", QualifiedKind("Service", "serving.knative.dev")) {
		t.Fatal("a group-qualified kind must not match the built-in kind of the same name")
	}
	specs := append(Builtins(), CustomResourceChangeMatcher("Service", "serving.knative.dev", nil))
	e := customEngine(specs...)
	old := mustObj(t, `{"metadata":{"name":"web","namespace":"n"},"spec":{"template":{"spec":{"containers":[{"image":"a:1"}]}}}}`)
	upd := mustObj(t, `{"metadata":{"name":"web","namespace":"n"},"spec":{"template":{"spec":{"containers":[{"image":"a:2"}]}}}}`)
	got := e.Match(IncomingK8sEvent{Operation: "update", Kind: "Service.serving.knative.dev", OldObj: old, Obj: upd})
	if len(got) != 1 || got[0].Spec.Name != "custom_change_service_serving.knative.dev" {
		names := []string{}
		for _, m := range got {
			names = append(names, m.Spec.Name)
		}
		t.Fatalf("want only the custom matcher, got %v", names)
	}
}

func TestCustomResourceChange_FingerprintDiffersFromBuiltinOfSameName(t *testing.T) {
	obj := mustObj(t, `{"metadata":{"name":"web","namespace":"n"}}`)
	custom := CustomResourceChangeMatcher("Deployment", "example.com", nil).FingerprintFn(obj)
	builtin := babysitterChangeMatcher("Deployment").FingerprintFn(obj)
	if custom == builtin {
		t.Fatal("a custom Deployment kind must not share the built-in Deployment's fingerprint")
	}
}

// A controller rewriting its spec on a loop: changes 1-10 in a window are
// reported, the 10th says the mute started, and nothing more for an hour.
func TestCustomResourceChange_ChurnMutesAtTenthWithNote(t *testing.T) {
	spec := CustomResourceChangeMatcher("Thing", "example.com", nil)
	spec.RateLimit = 0 // isolate churn from the 30s rate limit
	e := customEngine(spec)
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	e.crChurn.now = func() time.Time { return now }

	fire := func(i int) []Match {
		old := mustObj(t, `{"metadata":{"name":"a","namespace":"n"},"spec":{"v":0}}`)
		upd := mustObj(t, `{"metadata":{"name":"a","namespace":"n"},"spec":{"v":`+strconv.Itoa(i+1)+`}}`)
		return e.Match(IncomingK8sEvent{Operation: "update", Kind: "Thing.example.com", OldObj: old, Obj: upd})
	}
	for i := 1; i <= CustomResourceChurnThreshold; i++ {
		got := fire(i)
		if len(got) != 1 {
			t.Fatalf("change %d must be reported, got %d", i, len(got))
		}
		note := muteNote(got[0])
		if i < CustomResourceChurnThreshold && note != "" {
			t.Fatalf("change %d must not carry the mute note", i)
		}
		if i == CustomResourceChurnThreshold {
			want := now.Add(CustomResourceChurnCooldown).UTC().Format(time.RFC3339)
			if !strings.Contains(note, want) {
				t.Fatalf("the 10th change must say the mute ends at %s, got %q", want, note)
			}
		}
		now = now.Add(30 * time.Second)
	}
	if got := fire(11); len(got) != 0 {
		t.Fatal("changes after the 10th must be muted")
	}
	now = now.Add(CustomResourceChurnCooldown)
	if got := fire(12); len(got) != 1 {
		t.Fatal("after the cooldown the next change must be reported again")
	}
}

// ConfigMaps keep their own suppressor and timing.
func TestCustomResourceChange_ConfigMapChurnUnchanged(t *testing.T) {
	e := customEngine()
	if e.churn.muteAtThreshold || e.churn.threshold != DefaultChurnThreshold || e.churn.cooldown != DefaultChurnCooldown {
		t.Fatal("the ConfigMap suppressor must keep its defaults and mute after the threshold, not at it")
	}
	if !e.crChurn.muteAtThreshold || e.crChurn.threshold != CustomResourceChurnThreshold {
		t.Fatal("custom resources must use their own suppressor")
	}
}

func TestChurnSuppressor_MuteAtThreshold(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	c := newTestChurn(3, 10*time.Minute, time.Hour, &now).MuteAtThreshold()
	for i := 1; i <= 2; i++ {
		if d := c.AllowDetail("k"); !d.Allowed || d.Classified {
			t.Fatalf("fire %d: want allowed, not classified, got %+v", i, d)
		}
	}
	d := c.AllowDetail("k")
	if !d.Allowed || !d.Classified || !d.MutedUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("the threshold fire must be allowed and start the mute, got %+v", d)
	}
	if d := c.AllowDetail("k"); d.Allowed {
		t.Fatal("the fire after the threshold must be muted")
	}
}

func muteNote(m Match) string {
	for _, b := range m.ExtraBlocks {
		if b["type"] == "markdown" {
			if s, _ := b["data"].(string); strings.Contains(s, "not reported until") {
				return s
			}
		}
	}
	return ""
}
