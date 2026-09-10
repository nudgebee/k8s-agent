package alerts

import (
	"encoding/json"
	"strings"
	"testing"
)

func changeTrigger(kind, ns, name string) MatchedTrigger {
	return MatchedTrigger{
		AggregationKey:   "ConfigurationChange/KubernetesResource/Change",
		Priority:         "INFO",
		FindingType:      "configuration_change",
		Fingerprint:      "fp",
		SubjectName:      name,
		SubjectNamespace: ns,
		SubjectKind:      kind,
		MatcherName:      "babysitter_" + kind,
	}
}

// The title is what the change history lists and what an investigating
// model reads first. It used to be the raw aggregation key, which named
// neither the object nor its kind — and with a Deployment and the
// ConfigMap it mounts both changing, the kind is what tells them apart.
func TestChangeFindingTitleNamesTheObject(t *testing.T) {
	for _, tc := range []struct {
		kind, ns, name string
		want           string
	}{
		{"configmap", "sessions", "sessions-config", "ConfigMap sessions/sessions-config was changed"},
		{"deployment", "sessions", "sessions-api", "Deployment sessions/sessions-api was changed"},
		{"rollout", "prod", "checkout", "Rollout prod/checkout was changed"},
		// Cluster-scoped subjects must not render a leading slash.
		{"ingress", "", "public", "Ingress public was changed"},
	} {
		if got := changeTrigger(tc.kind, tc.ns, tc.name).Title(); got != tc.want {
			t.Errorf("Title() = %q; want %q", got, tc.want)
		}
	}
}

func TestChangeFindingDescriptionExplainsLatentConfigChanges(t *testing.T) {
	got := changeTrigger("configmap", "sessions", "sessions-config").Description()
	// The restart caveat is the operationally load-bearing part: an
	// envFrom value is only re-read when the consuming pods restart, so
	// the symptom can surface hours later and look unrelated.
	if !strings.Contains(got, "restart") {
		t.Errorf("ConfigMap description must state that consumers only pick the value up on restart; got %q", got)
	}

	workload := changeTrigger("deployment", "prod", "api").Description()
	if strings.Contains(workload, "ConfigMap") {
		t.Errorf("workload description must not claim to be a ConfigMap change; got %q", workload)
	}
}

// The `json` block carries the verbatim watch payload — obj AND oldObj.
// A ConfigMap holds up to 1 MiB, so an uncapped change to one ships
// ~2 MiB per event on top of the diff block that already summarises it.
func TestRawPayloadIsCappedAndStaysValidJSON(t *testing.T) {
	big := json.RawMessage(`{"obj":{"data":{"f":"` + strings.Repeat("x", maxRawPayloadBytes) + `"}}}`)
	capped := cappedRawPayload(big)
	if len(capped) >= len(big) {
		t.Fatalf("payload not capped: %d bytes", len(capped))
	}
	// The collector json-decodes this block. A truncated prefix would be
	// a parse error and the whole evidence array would be dropped.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(capped), &decoded); err != nil {
		t.Fatalf("capped payload is not valid JSON: %v (%s)", err, capped)
	}
	if decoded["truncated"] != true {
		t.Errorf("truncation must be stated, not silent: %s", capped)
	}
}

func TestRawPayloadUnderCapIsUntouched(t *testing.T) {
	raw := json.RawMessage(`{"obj":{"metadata":{"name":"web"}}}`)
	if got := cappedRawPayload(raw); got != string(raw) {
		t.Errorf("payload under the cap must pass through verbatim; got %s", got)
	}
}

// End-to-end shape of a ConfigMap change Finding, as the collector reads it.
func TestFromMatchedTrigger_ConfigMapChange(t *testing.T) {
	b := &Builder{AccountID: "acc", Cluster: "c"}
	m := changeTrigger("configmap", "sessions", "sessions-config")
	m.ExtraBlocks = []map[string]any{{
		"type": "diff",
		"data": map[string]any{"updated_paths": []any{"data.CACHE_TTL_SECONDS"}},
	}}

	env, err := b.FromMatchedTrigger(m, json.RawMessage(`{"operation":"update","kind":"ConfigMap"}`))
	if err != nil {
		t.Fatal(err)
	}
	if env.Finding.SubjectType != "configmap" {
		t.Errorf("subject_type = %q; want configmap", env.Finding.SubjectType)
	}
	if env.Finding.FindingType != "configuration_change" {
		t.Errorf("finding_type = %q; want configuration_change", env.Finding.FindingType)
	}
	if env.Finding.Failure {
		t.Error("a configuration change is not a failure")
	}
	// service_key is the ConfigMap's own namespace/name. Attribution to
	// the workloads that consume it happens server-side off the knowledge
	// graph's USES_CONFIG edges — the agent does not guess it.
	if env.Finding.ServiceKey != "sessions/sessions-config" {
		t.Errorf("service_key = %q; want sessions/sessions-config", env.Finding.ServiceKey)
	}

	var blocks []map[string]any
	if err := json.Unmarshal([]byte(env.Evidence[0].Data), &blocks); err != nil {
		t.Fatalf("evidence.data not a JSON array: %v", err)
	}
	var foundDiff bool
	for _, block := range blocks {
		if block["type"] == "diff" {
			foundDiff = true
		}
	}
	if !foundDiff {
		t.Errorf("diff block missing from evidence: %v", blocks)
	}
}
