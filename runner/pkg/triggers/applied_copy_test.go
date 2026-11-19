package triggers

import (
	"strings"
	"testing"
)

// A Deployment applied with kubectl carries a full copy of itself, inline env
// values included. The change diff renders the whole old and new object, so
// before this the copy was shipped twice with every recorded change.
func deploymentApplied(replicas, password string) string {
	return `{
		"metadata":{"name":"api","namespace":"prod","annotations":{
			"kubectl.kubernetes.io/last-applied-configuration":"{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"env\":[{\"name\":\"DB_PASSWORD\",\"value\":\"` + password + `\"}]}]}}}}",
			"objectset.rio.cattle.io/applied":"H4sIAAAAAAAA-rancher-copy",
			"deployment.kubernetes.io/revision":"4"}},
		"spec":{"replicas":` + replicas + `}
	}`
}

func TestChangeDiff_NeverRendersAppliedCopyAnnotations(t *testing.T) {
	old := mustObj(t, deploymentApplied("2", "old-pass"))
	updated := mustObj(t, deploymentApplied("5", "new-pass"))

	m := babysitterChangeMatcher("Deployment")
	if !m.Predicate(updated, old) {
		t.Fatal("the replicas change must still fire")
	}
	blocks := m.EnrichBlocks(updated, old, EnrichContext{})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 evidence block; got %d", len(blocks))
	}
	data, _ := blocks[0]["data"].(map[string]any)
	for _, side := range []string{"old", "new"} {
		yaml, _ := data[side].(string)
		for _, leaked := range []string{"last-applied-configuration", "objectset.rio.cattle.io/applied", "old-pass", "new-pass", "rancher-copy"} {
			if strings.Contains(yaml, leaked) {
				t.Errorf("%s YAML carries %q:\n%s", side, leaked, yaml)
			}
		}
		if !strings.Contains(yaml, "deployment.kubernetes.io/revision") {
			t.Errorf("%s YAML lost a real annotation:\n%s", side, yaml)
		}
	}
	if data["resource_name"] != "deployment/prod/api.yaml" {
		t.Errorf("resource_name = %v; want deployment/prod/api.yaml", data["resource_name"])
	}

	// The engine hands the same object to every matcher; it must not change.
	meta, _ := updated["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	if _, present := ann["kubectl.kubernetes.io/last-applied-configuration"]; !present {
		t.Error("enrichment mutated the caller's object")
	}
	if _, present := ann["objectset.rio.cattle.io/applied"]; !present {
		t.Error("enrichment mutated the caller's object")
	}
}

// Ingress annotations are monitored as changes. A Rancher apply rewrites its
// copy of the object; that alone is bookkeeping, and recording it would put
// the copy into the change's recorded values.
func TestIngressChange_RancherAppliedCopyIsNotAChange(t *testing.T) {
	mk := func(copy string) map[string]any {
		return mustObj(t, `{
			"metadata":{"name":"app","namespace":"prod","annotations":{
				"objectset.rio.cattle.io/applied":"`+copy+`"}},
			"spec":{"rules":[]}
		}`)
	}
	if babysitterChangeMatcher("Ingress").Predicate(mk("H4sI-new"), mk("H4sI-old")) {
		t.Error("a Rancher applied-copy-only change is bookkeeping, not a change")
	}
}
