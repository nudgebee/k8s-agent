package triggers

import (
	"strings"
	"testing"
)

// The shape this matcher was blind to: nginx behaviour lives in
// annotations, so tightening a body-size ceiling until uploads start
// returning 413 leaves the Ingress spec byte-identical.
func ingressWithBodySize(size string) string {
	return `{
		"metadata":{
			"name":"llm-gateway","namespace":"nudgebee","resourceVersion":"100",
			"annotations":{
				"nginx.ingress.kubernetes.io/proxy-body-size":"` + size + `",
				"nginx.ingress.kubernetes.io/proxy-read-timeout":"600",
				"cert-manager.io/issuer":"letsencrypt"
			}
		},
		"spec":{"rules":[{"host":"gw.example.com","http":{"paths":[{"path":"/","backend":{"service":{"name":"llm-gateway","port":{"number":80}}}}]}}]}
	}`
}

func TestIngressChange_FiresOnAnnotationChange(t *testing.T) {
	old := mustObj(t, ingressWithBodySize("50m"))
	updated := mustObj(t, ingressWithBodySize("1m"))

	m := babysitterChangeMatcher("Ingress")
	if !m.Predicate(updated, old) {
		t.Fatal("an annotation change must fire: it is how nginx behaviour is configured")
	}

	diffs := ComputeSpecDiff(updated, old, IngressDiffOptions())
	if len(diffs) != 1 {
		t.Fatalf("expected exactly the changed annotation; got %d: %+v", len(diffs), diffs)
	}
	if !strings.HasSuffix(diffs[0].Path, "proxy-body-size") {
		t.Errorf("path = %q; want the proxy-body-size annotation", diffs[0].Path)
	}
	if diffs[0].Before != "50m" || diffs[0].After != "1m" {
		t.Errorf("before/after = %v/%v; want 50m/1m", diffs[0].Before, diffs[0].After)
	}
}

// Guard test: this is exactly what the spec-only filter missed, and the
// reason the Ingress kind needed its own options.
func TestIngressChange_SpecOnlyOptionsMissAnnotationChanges(t *testing.T) {
	old := mustObj(t, ingressWithBodySize("50m"))
	updated := mustObj(t, ingressWithBodySize("1m"))
	if diffs := ComputeSpecDiff(updated, old, DefaultSpecDiffOptions()); len(diffs) != 0 {
		t.Fatalf("guard test is wrong: spec-rooted options produced %+v", diffs)
	}
}

func TestIngressChange_StillFiresOnRoutingChanges(t *testing.T) {
	old := mustObj(t, `{
		"metadata":{"name":"app","namespace":"prod"},
		"spec":{"rules":[{"http":{"paths":[{"path":"/api","backend":{"service":{"name":"api","port":{"number":80}}}}]}}]}
	}`)
	updated := mustObj(t, `{
		"metadata":{"name":"app","namespace":"prod"},
		"spec":{"rules":[{"http":{"paths":[{"path":"/api","backend":{"service":{"name":"api-v2","port":{"number":80}}}}]}}]}
	}`)
	if !babysitterChangeMatcher("Ingress").Predicate(updated, old) {
		t.Error("a backend service swap must still fire")
	}
}

// Every `kubectl apply` rewrites last-applied-configuration with a copy
// of the whole object. Monitoring annotations without excluding it would
// report every apply twice — once as the real change, once as the
// annotation carrying the same change.
func TestIngressChange_LastAppliedAnnotationIsNotAChange(t *testing.T) {
	old := mustObj(t, `{
		"metadata":{"name":"app","namespace":"prod","annotations":{
			"kubectl.kubernetes.io/last-applied-configuration":"{\"spec\":{\"rules\":[]}}"}},
		"spec":{"rules":[]}
	}`)
	updated := mustObj(t, `{
		"metadata":{"name":"app","namespace":"prod","annotations":{
			"kubectl.kubernetes.io/last-applied-configuration":"{\"spec\":{\"rules\":[{\"host\":\"x\"}]}}"}},
		"spec":{"rules":[]}
	}`)
	if babysitterChangeMatcher("Ingress").Predicate(updated, old) {
		t.Error("a last-applied-only change is kubectl bookkeeping, not a change")
	}
}

func TestIngressChange_MetadataChurnIsNotAChange(t *testing.T) {
	old := mustObj(t, `{
		"metadata":{"name":"app","namespace":"prod","resourceVersion":"100","generation":1,
			"managedFields":[{"manager":"old"}],"annotations":{"cert-manager.io/issuer":"letsencrypt"}},
		"spec":{"rules":[]},"status":{"loadBalancer":{"ingress":[{"ip":"10.0.0.1"}]}}
	}`)
	updated := mustObj(t, `{
		"metadata":{"name":"app","namespace":"prod","resourceVersion":"101","generation":2,
			"managedFields":[{"manager":"new"}],"annotations":{"cert-manager.io/issuer":"letsencrypt"}},
		"spec":{"rules":[]},"status":{"loadBalancer":{"ingress":[{"ip":"10.0.0.2"}]}}
	}`)
	if babysitterChangeMatcher("Ingress").Predicate(updated, old) {
		t.Error("resourceVersion / managedFields / status churn must not fire")
	}
}

func TestIngressChange_EnrichBlockRendersTheAnnotationDiff(t *testing.T) {
	old := mustObj(t, ingressWithBodySize("50m"))
	updated := mustObj(t, ingressWithBodySize("1m"))

	blocks := babysitterChangeMatcher("Ingress").EnrichBlocks(updated, old, EnrichContext{})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 evidence block; got %d", len(blocks))
	}
	data, _ := blocks[0]["data"].(map[string]any)
	if data == nil {
		t.Fatalf("block.data missing: %v", blocks[0])
	}
	if data["resource_name"] != "ingress/nudgebee/llm-gateway.yaml" {
		t.Errorf("resource_name = %v; want ingress/nudgebee/llm-gateway.yaml", data["resource_name"])
	}
	oldYAML, _ := data["old"].(string)
	newYAML, _ := data["new"].(string)
	if !strings.Contains(oldYAML, "50m") || !strings.Contains(newYAML, "1m") {
		t.Errorf("both annotation values must be rendered.\nold: %s\nnew: %s", oldYAML, newYAML)
	}
}

func TestIngressChange_LastAppliedStrippedFromRenderedYAML(t *testing.T) {
	mk := func(size string) map[string]any {
		return mustObj(t, `{
			"metadata":{"name":"app","namespace":"prod","annotations":{
				"nginx.ingress.kubernetes.io/proxy-body-size":"`+size+`",
				"kubectl.kubernetes.io/last-applied-configuration":"{\"metadata\":{\"name\":\"app\"}}"}},
			"spec":{"rules":[]}
		}`)
	}
	old, updated := mk("50m"), mk("1m")
	blocks := babysitterChangeMatcher("Ingress").EnrichBlocks(updated, old, EnrichContext{})
	data, _ := blocks[0]["data"].(map[string]any)
	newYAML, _ := data["new"].(string)
	if strings.Contains(newYAML, "last-applied-configuration") {
		t.Errorf("last-applied duplicates the object into the diff of that object: %s", newYAML)
	}
	if !strings.Contains(newYAML, "proxy-body-size") {
		t.Errorf("real annotations must survive: %s", newYAML)
	}
	// The caller's map must not be mutated — the engine hands the same
	// object to every matcher it evaluates.
	meta, _ := updated["metadata"].(map[string]any)
	ann, _ := meta["annotations"].(map[string]any)
	if _, present := ann["kubectl.kubernetes.io/last-applied-configuration"]; !present {
		t.Error("enrichment mutated the caller's object")
	}
}

// Only Ingress gets annotation monitoring. A Deployment's annotations
// carry rollout bookkeeping (restartedAt, deployment.kubernetes.io/revision)
// that would fire on every rollout — the spec change already reports that.
func TestIngressChange_OtherKindsKeepSpecOnlyOptions(t *testing.T) {
	mk := func(revision string) map[string]any {
		return mustObj(t, `{
			"metadata":{"name":"api","namespace":"prod","annotations":{"deployment.kubernetes.io/revision":"`+revision+`"}},
			"spec":{"replicas":2}
		}`)
	}
	if babysitterChangeMatcher("Deployment").Predicate(mk("2"), mk("1")) {
		t.Error("a Deployment annotation-only change must not fire")
	}
}
