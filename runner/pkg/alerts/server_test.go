package alerts

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestForwarder_AlertHappyPath verifies /api/alerts builds a Finding
// envelope per alert and POSTs to the collector with the existing auth +
// tenant headers. The body must be the {finding, evidence, message}
// envelope (no `event_type` wrapper) so the collector pipeline accepts it.
func TestForwarder_AlertHappyPath(t *testing.T) {
	got := make(chan *http.Request, 1)
	gotBody := make(chan []byte, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- r
		gotBody <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	f := NewForwarder(backend.URL, "test-secret", "acc-uuid", "cluster-x", slog.Default())
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	rawWebhook := `{"alerts":[{
		"startsAt":"2026-05-07T10:00:00Z",
		"status":"firing",
		"labels":{"alertname":"PodCrashLooping","pod":"web-0","namespace":"prod","severity":"critical"},
		"annotations":{"summary":"Pod web-0 in prod is crashlooping"}
	}]}`
	resp, err := http.Post(srv.URL+"/api/alerts", "application/json", strings.NewReader(rawWebhook))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d; want 202", resp.StatusCode)
	}

	select {
	case r := <-got:
		if r.Header.Get("X-NB-Account-Id") != "acc-uuid" {
			t.Errorf("X-NB-Account-Id = %q", r.Header.Get("X-NB-Account-Id"))
		}
		if r.Header.Get("X-NB-Cluster") != "cluster-x" {
			t.Errorf("X-NB-Cluster = %q", r.Header.Get("X-NB-Cluster"))
		}
		// Bare base64, no "Basic " prefix — matches the legacy sink format.
		if r.Header.Get("Authorization") == "" || strings.HasPrefix(r.Header.Get("Authorization"), "Basic ") {
			t.Errorf("Authorization = %q (expected bare base64, no Basic prefix)", r.Header.Get("Authorization"))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backend never received forwarded alert")
	}

	body := <-gotBody
	var env FindingEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("forwarded body is not a FindingEnvelope: %v\n%s", err, body)
	}
	// Field-by-field assertions against the backend shape.
	if env.Finding.Source != "prometheus" {
		t.Errorf("source = %q; want prometheus", env.Finding.Source)
	}
	if env.Finding.FindingType != "issue" {
		t.Errorf("finding_type = %q; want issue", env.Finding.FindingType)
	}
	if env.Finding.AggregationKey != "PodCrashLooping" {
		t.Errorf("aggregation_key = %q; want PodCrashLooping", env.Finding.AggregationKey)
	}
	if env.Finding.SubjectName != "web-0" || env.Finding.SubjectType != "pod" {
		t.Errorf("subject = (%q,%q); want (web-0,pod)", env.Finding.SubjectName, env.Finding.SubjectType)
	}
	if env.Finding.SubjectNamespace != "prod" {
		t.Errorf("subject_namespace = %q; want prod", env.Finding.SubjectNamespace)
	}
	if env.Finding.Priority != "HIGH" {
		t.Errorf("priority = %q; want HIGH (severity=critical)", env.Finding.Priority)
	}
	if env.Finding.AccountID != "acc-uuid" || env.Finding.Cluster != "cluster-x" {
		t.Errorf("account/cluster = (%q,%q)", env.Finding.AccountID, env.Finding.Cluster)
	}
	if env.Finding.Fingerprint == "" {
		t.Error("fingerprint must be set so consumer can dedupe")
	}
	if len(env.Evidence) != 1 {
		t.Fatalf("evidence count = %d; want 1 (raw payload block)", len(env.Evidence))
	}
	if env.Evidence[0].FileType != "structured_data" {
		t.Errorf("evidence file_type = %q; want structured_data", env.Evidence[0].FileType)
	}
	// Evidence.data is a JSON-stringified list of structured-data blocks.
	// Parse it back and walk to the inner json block to confirm the raw
	// alert is preserved.
	var blocks []map[string]any
	if err := json.Unmarshal([]byte(env.Evidence[0].Data), &blocks); err != nil {
		t.Fatalf("evidence.data not a JSON array: %v", err)
	}
	if len(blocks) != 1 || blocks[0]["type"] != "json" {
		t.Fatalf("unexpected evidence blocks: %v", blocks)
	}
	innerJSON, _ := blocks[0]["data"].(string)
	if !strings.Contains(innerJSON, `"alertname":"PodCrashLooping"`) {
		t.Errorf("raw alert payload missing from evidence inner block: %s", innerJSON)
	}
}

// TestForwarder_AlertWithoutSubjectIsForwarded — alerts that don't carry a
// pod/deployment/node/etc. label (cluster-level / control-plane / custom
// application alerts) must still be forwarded, with a placeholder subject,
// matching robusta's behaviour. They are NOT dropped.
func TestForwarder_AlertWithoutSubjectIsForwarded(t *testing.T) {
	var hits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	f := NewForwarder(backend.URL, "tok", "acc-1", "cluster-x", slog.Default())
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	// Only an alertname — no pod/deployment/node label.
	rawWebhook := `{"alerts":[{"labels":{"alertname":"GenericFire","severity":"warning"},"annotations":{}}]}`
	resp, err := http.Post(srv.URL+"/api/alerts", "application/json", strings.NewReader(rawWebhook))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	time.Sleep(200 * time.Millisecond)
	if hits != 1 {
		t.Errorf("backend received %d forwards; want 1 (subject-less alert is forwarded)", hits)
	}
}

// TestForwarder_K8sEventDroppedWithoutEngine — the forwarder's default
// (no Engine wired) is to drop every kubewatch event. Mirrors the
// "no playbook → no Finding" effective behaviour. Without this gate, a
// healthy Pod create event would have produced a no-op Finding and
// flooded the UI.
func TestForwarder_K8sEventDroppedWithoutEngine(t *testing.T) {
	var hits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	f := NewForwarder(backend.URL, "", "acc", "cluster", slog.Default())
	// f.Engine is nil — no matchers wired.
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/handle", "application/json", strings.NewReader(
		`{"data":{"operation":"create","kind":"Pod","obj":{"metadata":{"name":"p","namespace":"ns"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d; want 202 (still ack kubewatch even on drop)", resp.StatusCode)
	}
	time.Sleep(150 * time.Millisecond)
	if hits != 0 {
		t.Errorf("backend received %d forwards; want 0", hits)
	}
	if got := f.K8sUnmatched(); got != 1 {
		t.Errorf("K8sUnmatched() = %d; want 1", got)
	}
}

// TestForwarder_K8sEventFiresOneFindingPerMatch — when the engine
// produces matches, the forwarder emits one Finding per match with the
// matched aggregation_key. The same kubewatch event firing two matchers
// produces two Findings (both with matching subject info but distinct
// aggregation_key + fingerprint).
func TestForwarder_K8sEventFiresOneFindingPerMatch(t *testing.T) {
	gotBody := make(chan []byte, 4)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	f := NewForwarder(backend.URL, "tok", "acc-uuid", "cluster-x", slog.Default())
	f.Engine = &fakeEngine{matches: []MatchedTrigger{
		{
			AggregationKey:   "report_crash_loop",
			Priority:         "HIGH",
			FindingType:      "issue",
			Fingerprint:      "fp-crash",
			SubjectName:      "web-0",
			SubjectNamespace: "prod",
			SubjectKind:      "pod",
			MatcherName:      "pod_crash_loop",
		},
		{
			AggregationKey:   "image_pull_backoff_reporter",
			Priority:         "MEDIUM",
			FindingType:      "issue",
			Fingerprint:      "fp-pull",
			SubjectName:      "web-0",
			SubjectNamespace: "prod",
			SubjectKind:      "pod",
			MatcherName:      "image_pull_backoff",
		},
	}}
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	if _, err := http.Post(srv.URL+"/api/handle", "application/json", strings.NewReader(
		`{"data":{"operation":"update","kind":"Pod","obj":{"metadata":{"name":"web-0","namespace":"prod"}}}}`)); err != nil {
		t.Fatal(err)
	}

	gotKeys := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(gotKeys) < 2 {
		select {
		case body := <-gotBody:
			var env FindingEnvelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("body parse failed: %v\n%s", err, body)
			}
			gotKeys[env.Finding.AggregationKey] = true
		case <-deadline:
			t.Fatalf("expected 2 Findings; got %d: %v", len(gotKeys), gotKeys)
		}
	}
	if !gotKeys["report_crash_loop"] || !gotKeys["image_pull_backoff_reporter"] {
		t.Errorf("missing expected aggregation_keys; got %v", gotKeys)
	}
}

// fakeEngine returns a fixed match list for any input. Test-only.
type fakeEngine struct{ matches []MatchedTrigger }

func (e *fakeEngine) MatchK8sEvent(_, _ string, _, _ map[string]any) []MatchedTrigger {
	return e.matches
}

// (TestForwarder_K8sEventHappyPath was removed in stage 2.1 — replaced by
// TestForwarder_K8sEventDroppedWithoutEngine + the per-match test above.
// The old "every event becomes a Finding" behaviour is intentionally gone.)

// TestForwarder_K8sEventClusterSnapshotDropped covers the kubewatch-specific
// invariant: cluster_snapshot is dropped at the agent because pkg/discovery
// already handles informer-driven full sync. Forwarding it would duplicate
// every workload payload every hour.
func TestForwarder_K8sEventClusterSnapshotDropped(t *testing.T) {
	var hits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	f := NewForwarder(backend.URL, "", "", "", slog.Default())
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/handle", "application/json",
		strings.NewReader(`{"type":"cluster_snapshot","clusterSnapshot":{"workloads":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d; want 202 (still ack kubewatch even on drop)", resp.StatusCode)
	}
	time.Sleep(200 * time.Millisecond)
	if hits != 0 {
		t.Errorf("backend received %d forwards; cluster_snapshot must be dropped", hits)
	}
}

// TestForwarder_K8sEventBadPayloadDropped — malformed JSON or payload
// missing both `type` and `data` is dropped (no forward, no panic).
func TestForwarder_K8sEventBadPayloadDropped(t *testing.T) {
	var hits int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	f := NewForwarder(backend.URL, "", "", "", slog.Default())
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	if _, err := http.Post(srv.URL+"/api/handle", "application/json",
		strings.NewReader(`not json`)); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Post(srv.URL+"/api/handle", "application/json",
		strings.NewReader(`{}`)); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond)
	if hits != 0 {
		t.Errorf("backend received %d forwards on malformed input; should be 0", hits)
	}
}

// TestForwarder_K8sEventsAlias verifies /api/k8s-events aliases /api/handle.
// Same trigger-engine semantics — both URLs run inputs through the engine.
func TestForwarder_K8sEventsAlias(t *testing.T) {
	gotBody := make(chan []byte, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	f := NewForwarder(backend.URL, "", "acc", "cluster", slog.Default())
	f.Engine = &fakeEngine{matches: []MatchedTrigger{{
		AggregationKey:   "test_alias",
		Priority:         "INFO",
		FindingType:      "issue",
		Fingerprint:      "fp",
		SubjectName:      "d",
		SubjectNamespace: "ns",
		SubjectKind:      "deployment",
		MatcherName:      "test",
	}}}
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	if _, err := http.Post(srv.URL+"/api/k8s-events", "application/json",
		strings.NewReader(`{"data":{"operation":"update","kind":"Deployment","obj":{"metadata":{"name":"d","namespace":"ns"}}}}`)); err != nil {
		t.Fatal(err)
	}

	select {
	case body := <-gotBody:
		var env FindingEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("alias body parse failed: %v\n%s", err, body)
		}
		if env.Finding.AggregationKey != "test_alias" {
			t.Errorf("alias did not route through engine: aggregation_key=%q", env.Finding.AggregationKey)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alias /api/k8s-events did not forward")
	}
}

func TestForwarder_RejectsNonPost(t *testing.T) {
	f := NewForwarder("http://nowhere", "", "", "", slog.Default())
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	for _, path := range []string{"/api/alerts", "/api/handle", "/api/k8s-events"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s GET status = %d; want 405", path, resp.StatusCode)
		}
	}
}

// TestForwarder_StillReturns202OnBackendFailure — AlertManager retries are
// noisy; we always 202 the webhook handshake even when the backend is down.
func TestForwarder_StillReturns202OnBackendFailure(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()

	f := NewForwarder(backend.URL, "", "acc", "cluster", slog.Default())
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/alerts", "application/json",
		bytes.NewReader([]byte(`{"alerts":[{"labels":{"alertname":"X","pod":"p","namespace":"ns"}}]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d; want 202 (always ack AlertManager to suppress retries)", resp.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.Dropped() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("dropped counter = %d; want 1", f.Dropped())
}

func TestForwarder_HealthzReturns200(t *testing.T) {
	f := NewForwarder("http://nowhere", "", "", "", slog.Default())
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
}

// TestForwarder_ForwardShedWhenPoolSaturated verifies the bounded forward pool
// sheds (and counts) intake when saturated, instead of spawning unbounded
// goroutines. With pool size 1 and a forward held in-flight, a second intake
// is dropped and ForwardShed() increments.
func TestForwarder_ForwardShedWhenPoolSaturated(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release // hold the forward goroutine (and its pool slot) open
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	defer close(release)

	f := NewForwarder(backend.URL, "", "acc", "cluster", slog.Default())
	f.SetForwardPoolSize(1)
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	body := `{"alerts":[{"startsAt":"2026-05-07T10:00:00Z","status":"firing","labels":{"alertname":"X","pod":"p","namespace":"ns","severity":"critical"},"annotations":{"summary":"s"}}]}`

	post := func() {
		resp, err := http.Post(srv.URL+"/api/alerts", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}

	post() // acquires the only slot, then blocks in the backend
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first forward never reached the backend")
	}

	post() // pool saturated → shed
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.ForwardShed() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.ForwardShed(); got != 1 {
		t.Errorf("ForwardShed() = %d; want 1", got)
	}
}
