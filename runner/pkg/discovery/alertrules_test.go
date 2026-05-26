package discovery

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

// captureSink records every Post() call so tests can assert on the envelope
// without touching the network.
type captureSink struct {
	*Sink
	posts []*Envelope
}

// newCaptureSink wraps a real Sink whose URL points at a recording
// httptest.Server. We use the real Post path (so gzip / retry logic runs)
// rather than monkey-patching the sink.
func newCaptureSink(t *testing.T) *captureSink {
	t.Helper()
	cs := &captureSink{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var env Envelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			t.Errorf("bad envelope: %v", err)
			w.WriteHeader(500)
			return
		}
		cs.posts = append(cs.posts, &env)
		w.WriteHeader(202)
	}))
	t.Cleanup(srv.Close)
	cs.Sink = &Sink{
		URL:        srv.URL,
		AuthSecret: "test:secret",
		AccountID:  "acc",
		Cluster:    "test",
		HTTP:       srv.Client(),
		Logger:     slog.Default(),
	}
	return cs
}

// promRulesServer returns an httptest server that responds to /api/v1/rules
// with the given JSON body (or a 404 when body is empty).
func promRulesServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/rules" {
			http.NotFound(w, r)
			return
		}
		if body == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const promRulesOK = `{
  "status": "success",
  "data": {
    "groups": [{
      "name": "kubernetes.rules",
      "rules": [{
        "type": "alerting",
        "name": "KubeJobFailed",
        "query": "kube_job_failed > 0",
        "duration": "5m",
        "annotations": {"summary": "Job failed"},
        "labels": {"severity": "warning"}
      }]
    }]
  }
}`

func TestAlertRulesCollector_PromOnly(t *testing.T) {
	cs := newCaptureSink(t)
	promSrv := promRulesServer(t, promRulesOK)
	prom := prometheus.New(promSrv.URL, nil)

	c := &AlertRulesCollector{Prom: prom, Sink: cs.Sink}
	if err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cs.posts) != 1 {
		t.Fatalf("want 1 post, got %d", len(cs.posts))
	}
	env := cs.posts[0]
	if env.Type != TypeAlertRules {
		t.Errorf("type = %s; want alert_rules", env.Type)
	}
	if !env.FullLoad {
		t.Errorf("full_load should be true on alert_rules emits")
	}
	// alert_rules sends Data as an unwrapped dict — collector's
	// handle_alert_rules() expects a Mapping. Other discovery types
	// (services / pods / …) still wrap in []any.
	payload, ok := env.Data.(map[string]any)
	if !ok {
		t.Fatalf("data not a map: %T", env.Data)
	}
	if _, ok := payload["api_based_rules"]; !ok {
		t.Errorf("api_based_rules missing")
	}
	if _, ok := payload["crd_based_rules"]; ok {
		t.Errorf("crd_based_rules should be absent when no kube client")
	}
}

func TestAlertRulesCollector_PromUnavailable(t *testing.T) {
	cs := newCaptureSink(t)
	promSrv := promRulesServer(t, "") // 404s
	prom := prometheus.New(promSrv.URL, nil)

	c := &AlertRulesCollector{Prom: prom, Sink: cs.Sink}
	if err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Both sources absent → no envelope.
	if len(cs.posts) != 0 {
		t.Fatalf("want 0 posts (both sources empty), got %d", len(cs.posts))
	}
}

func TestAlertRulesCollector_RejectsMalformedPromResponse(t *testing.T) {
	cs := newCaptureSink(t)
	// Returns 200 but the shape is not the {status, data:{groups}} envelope.
	promSrv := promRulesServer(t, `{"status":"error","error":"timed out"}`)
	prom := prometheus.New(promSrv.URL, nil)

	c := &AlertRulesCollector{Prom: prom, Sink: cs.Sink}
	if err := c.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(cs.posts) != 0 {
		t.Fatalf("malformed prom response should be skipped, got %d posts", len(cs.posts))
	}
}

func TestAlertRulesCollector_RunEmitsImmediatelyThenOnTick(t *testing.T) {
	cs := newCaptureSink(t)
	promSrv := promRulesServer(t, promRulesOK)
	prom := prometheus.New(promSrv.URL, nil)

	c := &AlertRulesCollector{Prom: prom, Sink: cs.Sink}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// Immediate emit + at least one tick at the configured interval.
	_ = c.Run(ctx, 80*time.Millisecond)

	if len(cs.posts) < 2 {
		t.Errorf("expected at least 2 posts (immediate + tick), got %d", len(cs.posts))
	}
}

func TestUnwrapPromRules(t *testing.T) {
	good, ok := unwrapPromRules(json.RawMessage(promRulesOK))
	if !ok {
		t.Fatal("good payload should unwrap")
	}
	if _, has := good["groups"]; !has {
		t.Errorf("unwrapped result should contain `groups`")
	}

	cases := map[string]string{
		"non-success status": `{"status":"error"}`,
		"missing groups":     `{"status":"success","data":{"other":1}}`,
		"not json":           `not json`,
	}
	for name, raw := range cases {
		_, ok := unwrapPromRules(json.RawMessage(raw))
		if ok {
			t.Errorf("%s should reject", name)
		}
	}
}

func TestCrdCount(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    int
	}{
		{"empty", map[string]any{}, 0},
		{"crd absent", map[string]any{"api_based_rules": "..."}, 0},
		{"wrong shape", map[string]any{"crd_based_rules": "not-a-map"}, 0},
		{
			"populated",
			map[string]any{"crd_based_rules": map[string]any{"items": []any{1, 2, 3}}},
			3,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := crdCount(tc.payload); got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}
