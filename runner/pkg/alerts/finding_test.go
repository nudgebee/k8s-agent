package alerts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuilder_Alert_SubjectFallbackOrder(t *testing.T) {
	b := &Builder{AccountID: "acc", Cluster: "c"}
	cases := []struct {
		name    string
		labels  map[string]string
		subject string
		stype   string
	}{
		// Workload-level labels win over pod when both are present. A
		// `pod` label alongside a workload label is the signature of an
		// exporter-emitted metric (kube-state-metrics, node-exporter, etc.)
		// or an aggregated-by-workload rule — in both cases the workload
		// is the subject and `pod` is the scraper / one instance.
		{"deployment wins over scraper pod", map[string]string{"alertname": "KubeDeploymentReplicasMismatch", "pod": "victoria-kube-state-metrics-xxx", "deployment": "cert-manager-cainjector"}, "cert-manager-cainjector", "deployment"},
		{"statefulset wins over scraper pod", map[string]string{"alertname": "KubeStatefulSetReplicasMismatch", "pod": "ksm-xxx", "statefulset": "clickhouse"}, "clickhouse", "statefulset"},
		{"daemonset wins over scraper pod", map[string]string{"alertname": "X", "pod": "ksm-xxx", "daemonset": "fluent-bit"}, "fluent-bit", "daemonset"},
		// Pod is the subject when no workload label is present (typical
		// of single-pod metrics: CrashLoopBackOff, OOMKilled).
		{"pod used when no workload label", map[string]string{"alertname": "KubePodCrashLooping", "pod": "p1"}, "p1", "pod"},
		// Existing fallback chain still works.
		{"deployment used when no pod", map[string]string{"alertname": "X", "deployment": "d1"}, "d1", "deployment"},
		{"daemonset", map[string]string{"alertname": "X", "daemonset": "ds"}, "ds", "daemonset"},
		{"node only", map[string]string{"alertname": "X", "node": "n1"}, "n1", "node"},
		{"hpa", map[string]string{"alertname": "X", "hpa": "h"}, "h", "horizontalpodautoscaler"},
		// kube-state-metrics emits the full `horizontalpodautoscaler` label,
		// alongside job=kube-state-metrics — the HPA is the subject.
		{"hpa canonical label wins over scrape job", map[string]string{"alertname": "KubeHpaMaxedOut", "job": "kube-state-metrics", "horizontalpodautoscaler": "frontend", "namespace": "shop"}, "frontend", "horizontalpodautoscaler"},
		// PVC alerts (KubePersistentVolumeFillingUp/Errors) carry the
		// persistentvolumeclaim label alongside job=kubelet (the scrape
		// target). The PVC is the subject, never the kubelet scraper.
		{"pvc wins over scrape job", map[string]string{"alertname": "KubePersistentVolumeFillingUp", "job": "kubelet", "namespace": "hive", "persistentvolumeclaim": "dfs-hive-metastore-hdfs-datanode-0"}, "dfs-hive-metastore-hdfs-datanode-0", "persistentvolumeclaim"},
		// Real k8s Job alerts (KubeJobFailed) come from kube-state-metrics:
		// job_name is the Job, job=kube-state-metrics is the scraper.
		{"job_name is the subject, not the scraper job", map[string]string{"alertname": "KubeJobFailed", "job": "kube-state-metrics", "job_name": "backup-1234", "namespace": "ops"}, "backup-1234", "job"},
		// A bare exporter `job` is never a subject — falls through to the
		// alertname placeholder with an empty subject type.
		{"scrape exporter job is not a subject", map[string]string{"alertname": "KubeletTooManyPods", "job": "kubelet"}, "KubeletTooManyPods", ""},
		// Self-targeting control-plane alerts: the job IS the component and
		// no more-specific label exists, so it is a usable last resort.
		{"control-plane job used as last resort", map[string]string{"alertname": "KubeSchedulerDown", "job": "kube-scheduler"}, "kube-scheduler", "job"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			webhook := map[string]any{"alerts": []map[string]any{{"labels": tc.labels}}}
			raw, _ := json.Marshal(webhook)
			out, _, err := b.FromAlertManager(raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != 1 {
				t.Fatalf("got %d envelopes; want 1", len(out))
			}
			if out[0].Finding.SubjectName != tc.subject {
				t.Errorf("subject_name = %q; want %q", out[0].Finding.SubjectName, tc.subject)
			}
			if out[0].Finding.SubjectType != tc.stype {
				t.Errorf("subject_type = %q; want %q", out[0].Finding.SubjectType, tc.stype)
			}
		})
	}
}

func TestBuilder_Alert_MissingSubjectGetsPlaceholder(t *testing.T) {
	b := &Builder{AccountID: "acc", Cluster: "c"}
	// 3 alerts: first has pod, second has no subject label, third has node.
	// Subject-less alerts must NOT be dropped (matches robusta) — they get
	// a placeholder subject so they still surface.
	raw := []byte(`{"alerts":[
		{"labels":{"alertname":"A","pod":"p"}},
		{"labels":{"alertname":"B","severity":"critical"}},
		{"labels":{"alertname":"C","node":"n1"}}
	]}`)
	out, dropped, err := b.FromAlertManager(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("envelopes = %d; want 3 (subject-less alert is kept)", len(out))
	}
	if dropped != 0 {
		t.Errorf("dropped = %d; want 0", dropped)
	}
	// middle alert: alertname fallback, empty (TYPE_NONE) subject type.
	if got := out[1].Finding.SubjectName; got != "B" {
		t.Errorf("subject_name = %q; want %q", got, "B")
	}
	if got := out[1].Finding.SubjectType; got != "" {
		t.Errorf("subject_type = %q; want empty", got)
	}
}

func TestBuilder_Alert_PriorityFromSeverity(t *testing.T) {
	cases := map[string]string{
		"critical": "HIGH",
		"warning":  "MEDIUM",
		"info":     "INFO",
		"":         "INFO",
		"unknown":  "INFO",
		"low":      "LOW",
		"debug":    "DEBUG",
	}
	b := &Builder{AccountID: "acc", Cluster: "c"}
	for sev, want := range cases {
		t.Run("severity="+sev, func(t *testing.T) {
			labels := map[string]string{"alertname": "X", "pod": "p", "severity": sev}
			raw, _ := json.Marshal(map[string]any{"alerts": []map[string]any{{"labels": labels}}})
			out, _, err := b.FromAlertManager(raw)
			if err != nil || len(out) == 0 {
				t.Fatalf("err=%v len=%d", err, len(out))
			}
			if out[0].Finding.Priority != want {
				t.Errorf("priority = %q; want %q", out[0].Finding.Priority, want)
			}
		})
	}
}

func TestBuilder_Alert_StableFingerprint(t *testing.T) {
	b := &Builder{AccountID: "acc", Cluster: "c"}
	raw := []byte(`{"alerts":[{
		"startsAt":"2026-05-07T10:00:00Z",
		"labels":{"alertname":"X","pod":"p","namespace":"ns","severity":"critical"}
	}]}`)
	a, _, _ := b.FromAlertManager(raw)
	c, _, _ := b.FromAlertManager(raw)
	if a[0].Finding.Fingerprint == "" {
		t.Fatal("fingerprint must be set")
	}
	if a[0].Finding.Fingerprint != c[0].Finding.Fingerprint {
		t.Errorf("fingerprint not stable across builds: %q vs %q",
			a[0].Finding.Fingerprint, c[0].Finding.Fingerprint)
	}
}

func TestBuilder_Alert_PreservesUpstreamFingerprint(t *testing.T) {
	b := &Builder{AccountID: "acc", Cluster: "c"}
	// AlertManager sometimes carries its own fingerprint — preserve it.
	// The backend uses fingerprint to dedupe, and AlertManager's is
	// the more stable one.
	raw := []byte(`{"alerts":[{
		"fingerprint":"upstream-fp-1234",
		"labels":{"alertname":"X","pod":"p","namespace":"ns"}
	}]}`)
	out, _, _ := b.FromAlertManager(raw)
	if out[0].Finding.Fingerprint != "upstream-fp-1234" {
		t.Errorf("upstream fingerprint not preserved: %q", out[0].Finding.Fingerprint)
	}
}

func TestBuilder_Kubewatch_HappyPath(t *testing.T) {
	b := &Builder{AccountID: "acc", Cluster: "c"}
	raw := []byte(`{
		"operation":"update",
		"kind":"Deployment",
		"obj":{"metadata":{"name":"web","namespace":"prod"}}
	}`)
	env, err := b.FromKubewatchEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if env.Finding.SubjectType != "deployment" {
		t.Errorf("subject_type = %q; want deployment (lowercased Kind)", env.Finding.SubjectType)
	}
	if env.Finding.AggregationKey != "k8s_event_deployment_update" {
		t.Errorf("aggregation_key = %q", env.Finding.AggregationKey)
	}
	if env.Finding.Source != "kubernetes_api_server" {
		t.Errorf("source = %q; want kubernetes_api_server", env.Finding.Source)
	}
	if env.Finding.FindingType != "configuration_change" {
		t.Errorf("finding_type = %q; want configuration_change", env.Finding.FindingType)
	}
	// Evidence.data is a JSON-stringified array of blocks; the inner
	// "json" block's `data` field is itself a JSON string.
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(env.Evidence[0].Data), &blocks); err != nil {
		t.Fatalf("evidence.data not a JSON array: %v", err)
	}
	innerJSON, _ := blocks[0]["data"].(string)
	if !strings.Contains(innerJSON, `"name":"web"`) {
		t.Errorf("raw obj missing from evidence: %s", innerJSON)
	}
}

func TestBuilder_Kubewatch_RejectsMissingName(t *testing.T) {
	b := &Builder{AccountID: "acc", Cluster: "c"}
	raw := []byte(`{"kind":"Pod","operation":"create","obj":{"metadata":{}}}`)
	if _, err := b.FromKubewatchEvent(raw); err == nil {
		t.Error("expected error for payload without metadata.name")
	}
}

// TestBuilder_StartsAtIsRFC3339 pins the timestamp format. api-server's
// trigger_investigation handler decodes starts_at via mapstructure into
// time.Time, and its HasTimezoneIndicator gate appends "Z" if no offset
// is found — but only RFC3339 (with the T separator) round-trips cleanly.
// A Postgres-format string ("2026-05-07 13:00:57") becomes
// "2026-05-07 13:00:57Z" after the gate and then fails mapstructure.
// This test catches regressions back to the buggy space-separated layout.
func TestBuilder_StartsAtIsRFC3339(t *testing.T) {
	b := &Builder{AccountID: "acc", Cluster: "c"}

	// Kubewatch path generates the timestamp internally — RFC3339 is required.
	env, err := b.FromKubewatchEvent([]byte(
		`{"operation":"create","kind":"Pod","obj":{"metadata":{"name":"p","namespace":"ns"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, env.Finding.StartsAt); err != nil {
		t.Errorf("kubewatch starts_at = %q; not RFC3339: %v", env.Finding.StartsAt, err)
	}

	// Alerts that arrive with no startsAt fall through to the same code path.
	out, _, err := b.FromAlertManager([]byte(
		`{"alerts":[{"labels":{"alertname":"X","pod":"p"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339, out[0].Finding.StartsAt); err != nil {
		t.Errorf("alert (no upstream startsAt) starts_at = %q; not RFC3339: %v",
			out[0].Finding.StartsAt, err)
	}
}

// TestEvidence_DataIsJSONStringifiedArray asserts the wire-shape detail
// the collector relies on: Evidence.data
// is a JSON-stringified array of structured-data items, NOT a raw object.
func TestEvidence_DataIsJSONStringifiedArray(t *testing.T) {
	b := &Builder{AccountID: "acc", Cluster: "c"}
	raw := []byte(`{"alerts":[{"labels":{"alertname":"X","pod":"p"}}]}`)
	out, _, _ := b.FromAlertManager(raw)
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(out[0].Evidence[0].Data), &blocks); err != nil {
		t.Fatalf("Evidence.Data is not a JSON-stringified array: %v\n%s",
			err, out[0].Evidence[0].Data)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "json" {
		t.Errorf("unexpected blocks shape: %v", blocks)
	}
}
