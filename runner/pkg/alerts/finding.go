package alerts

// Default-Finding builder. The Go agent forwards raw AlertManager
// webhooks and raw kubewatch K8s-event payloads to the collector's
// `POST /v1/k8s/events` endpoint, wrapping each one in a Finding
// envelope so the collector pipeline accepts it without any backend
// code change.
//
// This is **packaging, not enrichment**. We extract the bare minimum
// required by the collector consumer (subject_name / aggregation_key /
// fingerprint / etc.) and attach the raw payload as a `json` evidence
// block for UI visibility.
//
// Enum values mirrored byte-for-byte:
//
//	FindingType:    "issue", "configuration_change", "report", "health_check"
//	FindingSource:  "kubernetes_api_server", "prometheus", "nudgebee", ...
//	Priority:       "DEBUG" | "INFO" | "LOW" | "MEDIUM" | "HIGH"
//	SubjectType:    "pod", "deployment", "node", "job", "daemonset", "statefulset", ...

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Finding mirrors the collector's expected wire shape. All non-`omitempty`
// fields are accessed without `.get()` by the consumer — missing values
// cause a KeyError that drops the message before storage.
type Finding struct {
	ID               string `json:"id"`
	Title            string `json:"title"`
	Description      string `json:"description"`
	Source           string `json:"source"`
	AggregationKey   string `json:"aggregation_key"`
	Failure          bool   `json:"failure"`
	FindingType      string `json:"finding_type"`
	Category         string `json:"category"`
	Priority         string `json:"priority"`
	SubjectType      string `json:"subject_type"`
	SubjectName      string `json:"subject_name"`
	SubjectNamespace string `json:"subject_namespace"`
	SubjectNode      string `json:"subject_node"`
	ServiceKey       string `json:"service_key"`
	Cluster          string `json:"cluster"`
	AccountID        string `json:"account_id"`
	VideoLinks       []any  `json:"video_links"`
	StartsAt         string `json:"starts_at"`
	UpdatedAt        string `json:"updated_at"`
	Fingerprint      string `json:"fingerprint,omitempty"`
	SubjectOwner     string `json:"subject_owner,omitempty"`
	SubjectOwnerKind string `json:"subject_owner_kind,omitempty"`
}

// Evidence is one outer envelope per enrichment block. One of these
// is emitted per `Enrichment`, with `data` being a JSON-stringified
// array of structured-data items.
type Evidence struct {
	IssueID   string `json:"issue_id"`
	FileType  string `json:"file_type"`
	Data      string `json:"data"` // JSON-stringified []structuredItem
	AccountID string `json:"account_id"`
}

// FindingEnvelope is the body POSTed to /v1/k8s/events. Tenant +
// cloud_account_id come from auth context and are added by the collector
// controller.
type FindingEnvelope struct {
	Finding  Finding    `json:"finding"`
	Evidence []Evidence `json:"evidence"`
	Message  string     `json:"message"`
}

// Builder holds per-agent constants so the per-event builders stay clean.
type Builder struct {
	AccountID string // `account_id` — pinned to the agent's NUDGEBEE account UUID
	Cluster   string // `cluster_id` — same as the agent's CLUSTER_NAME env
}

// FromMatchedTrigger wraps a kubewatch K8s-event payload into a Finding
// envelope when a trigger matcher fired. Differs from FromKubewatchEvent
// in two important ways:
//
//   - aggregation_key, priority, finding_type come from the matched
//     trigger spec (`report_crash_loop`, `pod_oom_killer_enricher`, etc.)
//     instead of the generic `k8s_event_<kind>_<op>` default.
//   - subject_owner / subject_owner_kind are populated from the matcher's
//     owner-walk so multiple Pod restarts of the same Deployment dedupe
//     to the workload, not to the per-restart Pod name.
//
// `match` carries the trigger result (Spec, Fingerprint, Owner, Subject*).
// `rawData` is the kubewatch inner `data` dict (operation, kind, obj,
// oldObj, …) — preserved verbatim as the evidence payload so the UI /
// LLM can inspect what triggered the Finding.
func (b *Builder) FromMatchedTrigger(matchSpec MatchedTrigger, rawData []byte) (*FindingEnvelope, error) {
	if matchSpec.SubjectName == "" {
		return nil, errors.New("triggers: match has no subject_name")
	}
	now := time.Now().UTC()
	startsAt := utcDBStr(now)
	id := uuid.NewString()
	serviceKey := matchSpec.SubjectNamespace + "/" + matchSpec.SubjectName

	// Owner-aware subject — the UI groups findings by service_key, which
	// is namespace/owner-or-pod. Use the resolved owner when the matcher
	// walked one (most Pod-based matchers do); fall back to the raw
	// subject otherwise (node_not_ready).
	subjectOwner, subjectOwnerKind := matchSpec.Owner.Name, matchSpec.Owner.Kind
	if subjectOwner != "" {
		serviceKey = matchSpec.SubjectNamespace + "/" + subjectOwner
	}

	finding := Finding{
		ID:               id,
		Title:            matchSpec.Title(),
		Description:      matchSpec.Description(),
		Source:           "kubernetes_api_server",
		AggregationKey:   matchSpec.AggregationKey,
		Failure:          matchSpec.FindingType == "issue",
		FindingType:      matchSpec.FindingType,
		Category:         "",
		Priority:         matchSpec.Priority,
		SubjectType:      matchSpec.SubjectKind,
		SubjectName:      matchSpec.SubjectName,
		SubjectNamespace: matchSpec.SubjectNamespace,
		SubjectNode:      matchSpec.SubjectNode,
		ServiceKey:       serviceKey,
		Cluster:          b.Cluster,
		AccountID:        b.AccountID,
		VideoLinks:       []any{},
		StartsAt:         startsAt,
		UpdatedAt:        startsAt,
		Fingerprint:      matchSpec.Fingerprint,
		SubjectOwner:     subjectOwner,
		SubjectOwnerKind: subjectOwnerKind,
	}
	evidence := []Evidence{newJSONEvidence(id, b.AccountID, rawData, matchSpec.ExtraBlocks)}
	return &FindingEnvelope{
		Finding:  finding,
		Evidence: evidence,
		Message:  finding.Title,
	}, nil
}

// MatchedTrigger is the subset of pkg/triggers.Match that the Builder
// needs to construct a Finding. Defining it locally (instead of importing
// pkg/triggers directly) keeps pkg/alerts free of an import cycle and
// preserves the layering: triggers → alerts → finding builder, never
// the other way.
type MatchedTrigger struct {
	AggregationKey   string
	Priority         string
	FindingType      string
	Fingerprint      string
	Owner            OwnerRef
	SubjectName      string
	SubjectNamespace string
	SubjectKind      string
	SubjectNode      string
	// MatcherName is for log + metric labels; not emitted on the Finding.
	MatcherName string

	// ExtraBlocks are additional structured-data evidence blocks
	// produced by the matcher (currently only `babysitter_*` populates
	// this with a `markdown` block carrying the spec diff). Each block
	// is the open map shape used by the legacy evidence emitter.
	ExtraBlocks []map[string]any
}

// OwnerRef mirrors triggers.OwnerRef. Same reasoning re. import cycle.
type OwnerRef struct {
	Name string
	Kind string
}

// Title produces the Finding title from the matcher + subject. Format
// mirrors the legacy Finding builders' output for each aggregation_key
// so the UI's existing per-aggregation_key formatting stays unchanged.
func (m MatchedTrigger) Title() string {
	switch m.AggregationKey {
	case "report_crash_loop":
		return fmt.Sprintf("Pod %s/%s is in CrashLoopBackOff", m.SubjectNamespace, m.SubjectName)
	case "pod_oom_killer_enricher":
		return fmt.Sprintf("Pod %s/%s was OOMKilled", m.SubjectNamespace, m.SubjectName)
	case "image_pull_backoff_reporter":
		return fmt.Sprintf("Pod %s/%s has ImagePullBackOff", m.SubjectNamespace, m.SubjectName)
	case "job_failure":
		return fmt.Sprintf("Job %s/%s failed", m.SubjectNamespace, m.SubjectName)
	case "node_not_ready":
		return fmt.Sprintf("Node %s is NotReady", m.SubjectName)
	default:
		return fmt.Sprintf("%s on %s/%s", m.AggregationKey, m.SubjectNamespace, m.SubjectName)
	}
}

// Description is a 1-line context blurb. Keeps Finding rows informative
// even before stage 2.2 ships the enricher chain that adds rich evidence.
func (m MatchedTrigger) Description() string {
	if m.MatcherName == "" {
		return "Trigger matched on agent (raw event in evidence)"
	}
	return "Trigger '" + m.MatcherName + "' matched on agent (raw event in evidence)"
}

// FromAlertManager wraps a raw AlertManager webhook into a Finding
// envelope. The webhook may carry many alerts under `alerts[]`; we emit
// one envelope per alert so the existing UI dedupes / aggregates per
// alertname the same way it does today.
//
// Returns (envelopes, dropped_count). Alerts with no resolvable
// kubernetes subject are NOT dropped — they get a placeholder subject
// (see alertToFinding) so they still surface, matching robusta. So
// dropped_count only counts alerts that failed to build (e.g. the raw
// alert could not be marshalled); those are skipped.
func (b *Builder) FromAlertManager(rawWebhook []byte) ([]FindingEnvelope, int, error) {
	var webhook struct {
		Alerts []alertManagerAlert `json:"alerts"`
	}
	if err := json.Unmarshal(rawWebhook, &webhook); err != nil {
		return nil, 0, fmt.Errorf("alertmanager: parse webhook: %w", err)
	}
	out := make([]FindingEnvelope, 0, len(webhook.Alerts))
	dropped := 0
	for _, a := range webhook.Alerts {
		env, err := b.alertToFinding(a)
		if err != nil {
			dropped++
			continue
		}
		out = append(out, env)
	}
	return out, dropped, nil
}

// FromKubewatchEvent wraps a single kubewatch event payload (the inner
// `data` dict from `{type: ..., data: {operation, kind, obj, ...}}` —
// the outer wrapper has already been stripped by the caller).
//
// Returns nil + error when the payload doesn't carry a usable
// metadata.name; consumer would drop it.
func (b *Builder) FromKubewatchEvent(rawData []byte) (*FindingEnvelope, error) {
	var k struct {
		Operation string         `json:"operation"`
		Kind      string         `json:"kind"`
		Obj       map[string]any `json:"obj"`
	}
	if err := json.Unmarshal(rawData, &k); err != nil {
		return nil, fmt.Errorf("kubewatch: parse data: %w", err)
	}
	meta, _ := k.Obj["metadata"].(map[string]any)
	subjectName, _ := meta["name"].(string)
	if subjectName == "" {
		return nil, errors.New("kubewatch: payload missing metadata.name")
	}
	subjectNamespace, _ := meta["namespace"].(string)

	subjectNode := ""
	if spec, ok := k.Obj["spec"].(map[string]any); ok {
		subjectNode, _ = spec["nodeName"].(string)
	}

	subjectType := strings.ToLower(k.Kind)
	if subjectType == "" {
		subjectType = "pod" // safest default; kubewatch always sends Kind in practice
	}
	operation := strings.ToLower(k.Operation)
	if operation == "" {
		operation = "update"
	}
	aggregationKey := fmt.Sprintf("k8s_event_%s_%s", subjectType, operation)
	now := time.Now().UTC()
	startsAt := utcDBStr(now)
	id := uuid.NewString()
	serviceKey := subjectNamespace + "/" + subjectName

	rawJSON, err := json.Marshal(k.Obj)
	if err != nil {
		return nil, fmt.Errorf("kubewatch: marshal raw obj: %w", err)
	}

	finding := Finding{
		ID:               id,
		Title:            fmt.Sprintf("%s %s/%s %s", k.Kind, subjectNamespace, subjectName, operation),
		Description:      fmt.Sprintf("kubewatch %s event from agent", operation),
		Source:           "kubernetes_api_server",
		AggregationKey:   aggregationKey,
		Failure:          false,
		FindingType:      "configuration_change",
		Category:         "",
		Priority:         "INFO",
		SubjectType:      subjectType,
		SubjectName:      subjectName,
		SubjectNamespace: subjectNamespace,
		SubjectNode:      subjectNode,
		ServiceKey:       serviceKey,
		Cluster:          b.Cluster,
		AccountID:        b.AccountID,
		VideoLinks:       []any{},
		StartsAt:         startsAt,
		UpdatedAt:        startsAt,
		Fingerprint:      fingerprint(aggregationKey, serviceKey, startsAt),
	}
	evidence := []Evidence{newJSONEvidence(id, b.AccountID, rawJSON, nil)}
	return &FindingEnvelope{
		Finding:  finding,
		Evidence: evidence,
		Message:  finding.Title,
	}, nil
}

// alertToFinding converts one PrometheusAlert to a Finding envelope.
func (b *Builder) alertToFinding(a alertManagerAlert) (FindingEnvelope, error) {
	subjectName, subjectType := alertSubject(a.Labels)
	if subjectName == "" {
		// No kubernetes object resolvable from the alert labels (cluster-
		// level / control-plane / custom application alerts like Watchdog,
		// KubeSchedulerDown, HighP95Latency). Don't drop — fall back to the
		// alertname as the subject so the alert still surfaces and each alert
		// type keeps a distinct service_key in the UI (a static placeholder
		// would group unrelated alerts together). subjectType stays empty.
		subjectName = pickLabel(a.Labels, "alertname")
		if subjectName == "" {
			subjectName = "UnnamedAlert"
		}
	}
	subjectNamespace := pickLabel(a.Labels, "namespace", "exported_namespace")
	subjectNode := pickLabel(a.Labels, "node", "instance")
	alertname := pickLabel(a.Labels, "alertname")
	if alertname == "" {
		alertname = "UnnamedAlert"
	}
	startsAt := a.StartsAt
	if startsAt == "" {
		startsAt = utcDBStr(time.Now().UTC())
	}
	serviceKey := subjectNamespace + "/" + subjectName
	id := uuid.NewString()

	rawJSON, err := json.Marshal(a)
	if err != nil {
		return FindingEnvelope{}, fmt.Errorf("alertmanager: marshal alert: %w", err)
	}

	finding := Finding{
		ID:               id,
		Title:            firstNonEmpty(a.Annotations["summary"], a.Annotations["description"], alertname),
		Description:      a.Annotations["description"],
		Source:           "prometheus",
		AggregationKey:   alertname,
		Failure:          false,
		FindingType:      "issue",
		Category:         "",
		Priority:         severityToPriority(a.Labels["severity"]),
		SubjectType:      subjectType,
		SubjectName:      subjectName,
		SubjectNamespace: subjectNamespace,
		SubjectNode:      subjectNode,
		ServiceKey:       serviceKey,
		Cluster:          b.Cluster,
		AccountID:        b.AccountID,
		VideoLinks:       []any{},
		StartsAt:         startsAt,
		UpdatedAt:        startsAt,
		Fingerprint:      coalesce(a.Fingerprint, fingerprint(alertname, serviceKey, startsAt)),
	}
	evidence := []Evidence{newJSONEvidence(id, b.AccountID, rawJSON, nil)}
	return FindingEnvelope{
		Finding:  finding,
		Evidence: evidence,
		Message:  finding.Title,
	}, nil
}

// alertManagerAlert mirrors PrometheusAlert.
type alertManagerAlert struct {
	StartsAt     string            `json:"startsAt"`
	EndsAt       string            `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
}

// alertSubject picks the most specific kubernetes object name from the
// alert labels, returning (name, type).
//
// Order: workload-level labels first, then pod. A `pod` label is only
// unambiguously the subject when no workload-level label is present
// (typical of single-pod metrics like CrashLoopBackOff, OOMKilled). When
// the metric series carries both — as with kube-state-metrics exporters
// (labels.pod is the scraper, labels.deployment is the resource being
// measured) and aggregated-by-workload rules (`sum by (deployment) ...`) —
// the workload label is the subject and `pod` would mis-attribute the
// alert to the emitter.
func alertSubject(labels map[string]string) (string, string) {
	for _, candidate := range []struct{ key, kind string }{
		{"deployment", "deployment"},
		{"statefulset", "statefulset"},
		{"daemonset", "daemonset"},
		{"job", "job"},
		{"hpa", "horizontalpodautoscaler"},
		{"persistentvolumeclaim", "persistentvolumeclaim"},
		{"workload", "deployment"}, // some alerts emit a generic workload label
		{"node", "node"},
		{"pod", "pod"}, // last — only used when no workload-level label is present
	} {
		if v := labels[candidate.key]; v != "" {
			return v, candidate.kind
		}
	}
	if v := labels["kubernetes_kind"]; v != "" {
		// kubernetes_kind tells us the type but not the name — the labels
		// above are the only source of name, so this is reachable only if
		// upstream forgot to also set the per-kind label. Fall through.
		return "", strings.ToLower(v)
	}
	return "", ""
}

// severityToPriority maps Prometheus alert `severity` labels to the
// FindingSeverity values the backend expects.
func severityToPriority(severity string) string {
	switch strings.ToLower(severity) {
	case "critical":
		return "HIGH"
	case "warning":
		return "MEDIUM"
	case "info", "":
		return "INFO"
	case "low":
		return "LOW"
	case "debug":
		return "DEBUG"
	default:
		return "INFO"
	}
}

// newJSONEvidence wraps the raw payload as a single `json` block inside
// the `structured_data` envelope. The collector reads `data` as a
// JSON-stringified array of these blocks.
//
// `extras` holds matcher-supplied evidence blocks (e.g. babysitter's
// markdown diff) appended after the raw payload. Empty/nil extras leave
// the evidence shape identical to the pre-stage-2.1 single-block form,
// so existing UI parsers (KubernetesPVC.jsx walks blocks[0]) keep
// reading the raw JSON from index 0.
func newJSONEvidence(findingID, accountID string, raw json.RawMessage, extras []map[string]any) Evidence {
	blocks := make([]map[string]any, 0, 1+len(extras))
	blocks = append(blocks, map[string]any{
		"type":            "json",
		"data":            string(raw),
		"additional_info": map[string]any{},
	})
	for _, extra := range extras {
		if extra == nil {
			continue
		}
		blocks = append(blocks, extra)
	}
	encoded, _ := json.Marshal(blocks) // []map[string]any never errors
	return Evidence{
		IssueID:   findingID,
		FileType:  "structured_data",
		Data:      string(encoded),
		AccountID: accountID,
	}
}

// fingerprint stable-hashes the (aggregation_key, service_key, starts_at)
// tuple. The backend dedupes by fingerprint, so two alerts
// for the same workload at the same starts_at collapse to one Finding.
func fingerprint(aggregationKey, serviceKey, startsAt string) string {
	h := sha256.Sum256([]byte(aggregationKey + ":" + serviceKey + ":" + startsAt))
	return hex.EncodeToString(h[:])
}

// utcDBStr produces an RFC3339 UTC timestamp (e.g. "2026-05-07T13:00:57Z").
// The backend's `trigger_investigation` handler decodes `starts_at` /
// `ends_at` via mapstructure into `time.Time`, with a fallback that
// appends "Z" when the string has no timezone indicator. The legacy
// `datetime_to_db_str` emits a space-separated
// "%Y-%m-%dT%H:%M:%S.%f%z" that hits this path with a Postgres-format
// string ("2026-05-07 13:00:57"); the backend appends "Z" to
// "2026-05-07 13:00:57Z", then mapstructure rejects the space —
// every Finding produced that way fails the downstream
// trigger_investigation call (HTTPError 400). RFC3339 is the only
// format that round-trips cleanly through both the backend's
// HasTimezoneIndicator gate and its time.Time parser.
func utcDBStr(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

func pickLabel(labels map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
