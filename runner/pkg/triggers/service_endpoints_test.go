package triggers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeServiceBackends is a canned-answer ServiceBackendsLister for tests.
type fakeServiceBackends struct {
	anyPod      bool
	anyTemplate bool
	podErr      error
	templateErr error
	samples     []PodLabelSample
	samplesErr  error
}

func (f *fakeServiceBackends) AnyPodMatching(_ context.Context, _ string, _ map[string]string) (bool, error) {
	return f.anyPod, f.podErr
}

func (f *fakeServiceBackends) AnyWorkloadTemplateMatching(_ context.Context, _ string, _ map[string]string) (bool, error) {
	return f.anyTemplate, f.templateErr
}

func (f *fakeServiceBackends) ListPodLabels(_ context.Context, _ string, _ int) ([]PodLabelSample, error) {
	return f.samples, f.samplesErr
}

// brokenService is the classic misconfiguration: selector patched to a
// label no pod carries.
func brokenService(t *testing.T) map[string]any {
	t.Helper()
	return asObj(t, `{
		"metadata":{"name":"nginx-test","namespace":"demo-workloads"},
		"spec":{"type":"ClusterIP","selector":{"app":"wrong-label"},
			"ports":[{"port":80}]}
	}`)
}

func TestServiceNoEndpoints_FiresOnSelectorMismatch(t *testing.T) {
	m := serviceNoEndpointsMatcher()
	ec := EnrichContext{ServiceBackends: &fakeServiceBackends{anyPod: false, anyTemplate: false}}
	if !m.PredicateCtx(brokenService(t), nil, ec) {
		t.Fatal("must fire when selector matches no pods and no workload templates")
	}
	if m.FingerprintFn(brokenService(t)) == "" {
		t.Error("fingerprint must be non-empty")
	}
}

func TestServiceNoEndpoints_DropsWhenPodsMatch(t *testing.T) {
	m := serviceNoEndpointsMatcher()
	ec := EnrichContext{ServiceBackends: &fakeServiceBackends{anyPod: true}}
	if m.PredicateCtx(brokenService(t), nil, ec) {
		t.Error("must not fire when the selector matches live pods")
	}
}

func TestServiceNoEndpoints_DropsOnScaleToZero(t *testing.T) {
	// Selector matches a Deployment's pod template but the workload has
	// no pods right now (replicas=0 / KEDA). Scale state, not misconfig.
	m := serviceNoEndpointsMatcher()
	ec := EnrichContext{ServiceBackends: &fakeServiceBackends{anyPod: false, anyTemplate: true}}
	if m.PredicateCtx(brokenService(t), nil, ec) {
		t.Error("must not fire when the selector matches a workload pod template (scale-to-zero)")
	}
}

func TestServiceNoEndpoints_DropsSelectorlessAndExternalName(t *testing.T) {
	m := serviceNoEndpointsMatcher()
	ec := EnrichContext{ServiceBackends: &fakeServiceBackends{}}

	noSelector := asObj(t, `{
		"metadata":{"name":"manual-endpoints","namespace":"prod"},
		"spec":{"type":"ClusterIP","ports":[{"port":80}]}
	}`)
	if m.PredicateCtx(noSelector, nil, ec) {
		t.Error("must not fire for a selector-less Service (manually-managed Endpoints)")
	}

	externalName := asObj(t, `{
		"metadata":{"name":"ext","namespace":"prod"},
		"spec":{"type":"ExternalName","externalName":"db.example.com",
			"selector":{"app":"ignored"}}
	}`)
	if m.PredicateCtx(externalName, nil, ec) {
		t.Error("must not fire for ExternalName (kube-proxy ignores the selector)")
	}
}

func TestServiceNoEndpoints_DropsWithoutListerAndOnErrors(t *testing.T) {
	m := serviceNoEndpointsMatcher()

	if m.PredicateCtx(brokenService(t), nil, EnrichContext{}) {
		t.Error("must not fire when no ServiceBackends lister is wired")
	}

	// Fail open: an API error must never produce a Finding.
	ec := EnrichContext{ServiceBackends: &fakeServiceBackends{podErr: errors.New("apiserver down")}}
	if m.PredicateCtx(brokenService(t), nil, ec) {
		t.Error("must not fire when the pod probe errors")
	}
	ec = EnrichContext{ServiceBackends: &fakeServiceBackends{templateErr: errors.New("apiserver down")}}
	if m.PredicateCtx(brokenService(t), nil, ec) {
		t.Error("must not fire when the workload-template probe errors")
	}
}

func TestServiceNoEndpoints_FingerprintTracksSelector(t *testing.T) {
	m := serviceNoEndpointsMatcher()
	a := brokenService(t)
	b := asObj(t, `{
		"metadata":{"name":"nginx-test","namespace":"demo-workloads"},
		"spec":{"type":"ClusterIP","selector":{"app":"other-label"}}
	}`)
	if m.FingerprintFn(a) == m.FingerprintFn(b) {
		t.Error("a changed selector must produce a fresh fingerprint (new Finding immediately)")
	}
	if m.FingerprintFn(a) != m.FingerprintFn(brokenService(t)) {
		t.Error("the same broken selector must produce a stable fingerprint (rate-limit dedup)")
	}
}

func TestServiceNoEndpoints_EnrichBlocks(t *testing.T) {
	ec := EnrichContext{ServiceBackends: &fakeServiceBackends{
		samples: []PodLabelSample{
			{Name: "nginx-abc", Labels: map[string]string{"app": "nginx-test"}},
		},
	}}
	blocks := serviceNoEndpointsEnrichBlocks(brokenService(t), nil, ec)
	joined := ""
	var haveTable bool
	for _, b := range blocks {
		if b["type"] == "table" {
			haveTable = true
			continue
		}
		s, _ := b["data"].(string)
		joined += s + "\n"
	}
	if !strings.Contains(joined, "app=wrong-label") {
		t.Errorf("evidence must show the configured selector; got:\n%s", joined)
	}
	if !haveTable {
		t.Error("evidence must include the pod-labels comparison table")
	}

	// One block must carry additional_info.insights with severity Critical
	// — the collector passes it through to the Finding's insight list,
	// which is what the investigate page renders.
	haveInsight := false
	for _, b := range blocks {
		ai, _ := b["additional_info"].(map[string]any)
		if ai == nil {
			continue
		}
		ins, _ := ai["insights"].([]map[string]any)
		for _, i := range ins {
			if i["severity"] == "Critical" {
				haveInsight = true
			}
		}
	}
	if !haveInsight {
		t.Error("evidence must carry a Critical insight via additional_info.insights")
	}

	// Empty namespace → the table degrades to a "no pods at all" note.
	ec = EnrichContext{ServiceBackends: &fakeServiceBackends{}}
	blocks = serviceNoEndpointsEnrichBlocks(brokenService(t), nil, ec)
	found := false
	for _, b := range blocks {
		if s, _ := b["data"].(string); strings.Contains(s, "no pods at all") {
			found = true
		}
	}
	if !found {
		t.Error("evidence must note when the namespace has no pods at all")
	}
}

// TestServiceNoEndpoints_EndToEndThroughEngine drives the matcher through
// Engine.Match with the kubewatch wire shape.
func TestServiceNoEndpoints_EndToEndThroughEngine(t *testing.T) {
	eng := NewEngine(Builtins(), time.Now().Add(-time.Hour)).
		WithServiceBackendsLister(&fakeServiceBackends{anyPod: false, anyTemplate: false})
	matches := eng.Match(IncomingK8sEvent{
		Operation: "update",
		Kind:      "Service",
		Obj:       brokenService(t),
		OldObj:    brokenService(t), // kubewatch aliases obj/oldObj; must still fire
	})
	if len(matches) != 1 {
		t.Fatalf("want exactly 1 match, got %d", len(matches))
	}
	m := matches[0]
	if m.Spec.AggregationKey != "service_no_endpoints" {
		t.Errorf("aggregation key = %q", m.Spec.AggregationKey)
	}
	if m.SubjectName != "nginx-test" || m.SubjectNamespace != "demo-workloads" || m.SubjectKind != "service" {
		t.Errorf("subject = %s/%s kind=%s", m.SubjectNamespace, m.SubjectName, m.SubjectKind)
	}

	// Same event again inside the rate-limit window → deduped.
	if again := eng.Match(IncomingK8sEvent{
		Operation: "update", Kind: "Service",
		Obj: brokenService(t), OldObj: brokenService(t),
	}); len(again) != 0 {
		t.Errorf("repeat within rate-limit window must not re-fire, got %d", len(again))
	}

	// CREATE never fires (helm installs Services before their workloads).
	engCreate := NewEngine(Builtins(), time.Now().Add(-time.Hour)).
		WithServiceBackendsLister(&fakeServiceBackends{anyPod: false, anyTemplate: false})
	if created := engCreate.Match(IncomingK8sEvent{
		Operation: "create", Kind: "Service", Obj: brokenService(t),
	}); len(created) != 0 {
		t.Errorf("must not fire on CREATE, got %d", len(created))
	}
}
