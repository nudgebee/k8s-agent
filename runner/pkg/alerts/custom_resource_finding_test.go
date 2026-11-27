package alerts

import (
	"encoding/json"
	"testing"
)

// A custom-resource change is sent with no service key and no owner, so the
// backend never ties a Certificate "web" to a Deployment "web".
func TestFromMatchedTrigger_UnattributedCustomResource(t *testing.T) {
	b := &Builder{AccountID: "acc", Cluster: "c"}
	env, err := b.FromMatchedTrigger(MatchedTrigger{
		AggregationKey: "ConfigurationChange/KubernetesResource/Change", Priority: "INFO",
		FindingType: "configuration_change", SubjectName: "web", SubjectNamespace: "shop",
		SubjectKind: "certificate.cert-manager.io", SubjectDisplayKind: "Certificate",
		Owner: OwnerRef{Name: "web", Kind: "deployment"}, Unattributed: true,
	}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	f := env.Finding
	if f.ServiceKey != "" || f.SubjectOwner != "" || f.SubjectOwnerKind != "" {
		t.Errorf("want no service key or owner, got %q / %q / %q", f.ServiceKey, f.SubjectOwner, f.SubjectOwnerKind)
	}
	if f.SubjectType != "certificate.cert-manager.io" {
		t.Errorf("subject_type = %q", f.SubjectType)
	}
	if f.Title != "Certificate shop/web was changed" {
		t.Errorf("title = %q", f.Title)
	}
	if want := "The Certificate's configuration was changed (deploy or spec update); the spec diff is attached in evidence."; f.Description != want {
		t.Errorf("description = %q", f.Description)
	}
}

// Built-in kinds keep their service key.
func TestFromMatchedTrigger_AttributedKeepsServiceKey(t *testing.T) {
	b := &Builder{AccountID: "acc", Cluster: "c"}
	env, err := b.FromMatchedTrigger(MatchedTrigger{
		AggregationKey: "ConfigurationChange/KubernetesResource/Change", SubjectName: "web",
		SubjectNamespace: "shop", SubjectKind: "deployment",
	}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if env.Finding.ServiceKey != "shop/web" || env.Finding.Title != "Deployment shop/web was changed" {
		t.Errorf("got %q / %q", env.Finding.ServiceKey, env.Finding.Title)
	}
}
