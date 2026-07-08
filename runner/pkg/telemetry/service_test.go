package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestService_PostsActivityStatsWithProbedStatus stands up fake datasources
// and verifies the service POSTs the expected wire shape to /v1/k8s/telemetry —
// keys the collector reads plus the URLs the UI
// shows. Confirms:
//   - AlertManager /-/healthy in-package probe flips its Connection bool
//   - caller-provided PrometheusConnected / LogsProviderStatus / NodeAgentCount round-trip
//   - Authorization header is bare base64 (no "Basic " prefix)
//   - URL set + probe down → Connection=false but URL still emitted
//   - traceProvider mirrors get_trace_provider() default ("otel_clickhouse")
func TestService_PostsActivityStatsWithProbedStatus(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/-/healthy" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
	}))
	defer prom.Close()
	// AlertManager URL set but server returns 500 → Connection should be false
	// while AlertmanagerUrl still appears in the payload (so the UI can show
	// "URL configured but unhealthy").
	am := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer am.Close()

	var got struct {
		body []byte
		auth string
	}
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.body, _ = io.ReadAll(r.Body)
		got.auth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer collector.Close()

	s := &Service{
		Endpoint:     collector.URL,
		AuthSecret:   "tok",
		AccountID:    "a929c7a3",
		ClusterName:  "dev",
		AgentVersion: "v0",
		Period:       time.Minute,
		Logger:       slog.Default(),
		Datasources: func() Datasources {
			return Datasources{
				PrometheusURL:       prom.URL,
				PrometheusConnected: true, // caller-computed (authenticated query)
				AlertManagerURL:     am.URL,
				LogsProvider:        "loki",
				LogsProviderURL:     "http://loki.svc:3100",
				LogsProviderStatus:  true,
				NodeAgentCount:      3,
			}
		},
		LightActions: func() []string { return []string{"prometheus_enricher"} },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.postOnce(ctx); err != nil {
		t.Fatal(err)
	}

	var posted ClusterStatus
	if err := json.Unmarshal(got.body, &posted); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, got.body)
	}
	if !posted.ActivityStat.PrometheusConnection {
		t.Error("prometheusConnection should be true")
	}
	if !posted.ActivityStat.LogsConnection {
		t.Error("logsConnection should be true (caller provided LogsProviderStatus=true)")
	}
	if posted.ActivityStat.AlertManagerConnection {
		t.Error("alertManagerConnection should be false (probe 500)")
	}
	if posted.ActivityStat.AlertManagerURL != am.URL {
		t.Errorf("alertmanagerUrl = %q; want %q (URL emitted even when probe fails)", posted.ActivityStat.AlertManagerURL, am.URL)
	}
	if posted.ActivityStat.LogsConnectionProvider != "loki" {
		t.Errorf("logsConnectionProvider = %q; want loki", posted.ActivityStat.LogsConnectionProvider)
	}
	if posted.ActivityStat.NodeAgentCount != 3 || !posted.ActivityStat.NodeAgentConnection {
		t.Errorf("nodeAgent: count=%d connection=%v; want 3, true", posted.ActivityStat.NodeAgentCount, posted.ActivityStat.NodeAgentConnection)
	}
	// Default trace provider when nothing else is configured —
	// falls through to "otel_clickhouse".
	if posted.ActivityStat.TraceProvider != "otel_clickhouse" {
		t.Errorf("traceProvider = %q; want otel_clickhouse", posted.ActivityStat.TraceProvider)
	}
	if !equalStrings(posted.LightActions, []string{"prometheus_enricher"}) {
		t.Errorf("light_actions = %v", posted.LightActions)
	}

	// Auth must be bare base64 (no "Basic " prefix) — collector decodes the
	// whole header verbatim. Same shape as discovery sink.
	if got.auth == "" || strings.HasPrefix(got.auth, "Basic ") {
		t.Errorf("Authorization = %q (expected bare base64)", got.auth)
	}
}

// TestService_PostsClusterStatsProviderFields verifies the six new
// ClusterStats fields populated from Provider end up on the wire under their
// snake_case JSON keys. Dropping omitempty matters here — even empty
// strings must be emitted under the right keys, matching Pydantic v1's
// `Optional[str] = ""` shape.
func TestService_PostsClusterStatsProviderFields(t *testing.T) {
	var captured []byte
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer collector.Close()

	s := &Service{
		Endpoint:     collector.URL,
		AuthSecret:   "tok",
		ClusterName:  "prod-cluster",
		AgentVersion: "v0",
		Period:       time.Minute,
		Logger:       slog.Default(),
		Provider: ProviderInfo{
			Provider:      "EKS",
			AccountNumber: "123456789012",
			Region:        "us-east-1",
			Zone:          "us-east-1a",
		},
		Datasources:  func() Datasources { return Datasources{} },
		LightActions: func() []string { return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.postOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// Typed assertion — round-trip through ClusterStats.
	var posted ClusterStatus
	if err := json.Unmarshal(captured, &posted); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, captured)
	}
	want := ClusterStats{
		Provider:              "EKS",
		ClusterName:           "prod-cluster",
		ProviderAccountNumber: "123456789012",
		ProviderRegion:        "us-east-1",
		ProviderZone:          "us-east-1a",
		// ProjectID / ResourceGroup intentionally empty — must still serialize.
	}
	if posted.Stats != want {
		t.Errorf("stats = %+v\n  want = %+v", posted.Stats, want)
	}

	// Raw JSON-key assertion — guards against omitempty regressions and against
	// rename of any of the six contract field names.
	var raw struct {
		Stats map[string]any `json:"stats"`
	}
	if err := json.Unmarshal(captured, &raw); err != nil {
		t.Fatalf("raw unmarshal: %v", err)
	}
	for _, k := range []string{
		"provider",
		"cluster_name",
		"provider_account_number",
		"provider_region",
		"provider_zone",
		"provider_project_id",
		"provider_resource_group",
	} {
		if _, ok := raw.Stats[k]; !ok {
			t.Errorf("stats missing key %q (omitempty regression?). keys present: %v", k, raw.Stats)
		}
	}
}

// TestProbe_TraceProviderSelection covers the get_trace_provider /
// get_trace_status / get_trace_url precedence.
func TestProbe_TraceProviderSelection(t *testing.T) {
	cases := []struct {
		name     string
		ds       Datasources
		provider string
		enabled  bool
		url      string
	}{
		{
			name:     "TRACE_TABLE wins",
			ds:       Datasources{TraceTable: "bq.dataset.traces", JaegerEnabled: true, JaegerQueryURL: "http://jaeger"},
			provider: "bigquery",
			enabled:  true,
			url:      "bq.dataset.traces",
		},
		{
			name:     "chronosphere via explicit URL",
			ds:       Datasources{ChronosphereTracesEnabled: true, ChronosphereTracesURL: "https://traces.example.io"},
			provider: "chronosphere",
			enabled:  true,
			// get_trace_url falls through to "" when neither TRACE_TABLE nor jaeger
			url: "",
		},
		{
			name:     "chronosphere inferred from prometheus URL",
			ds:       Datasources{ChronosphereTracesEnabled: true, PrometheusURL: "https://abc.chronosphere.io/api/prom"},
			provider: "chronosphere",
			enabled:  true,
		},
		{
			name:     "jaeger when enabled+url",
			ds:       Datasources{JaegerEnabled: true, JaegerQueryURL: "http://jaeger.svc:16686"},
			provider: "jaeger",
			enabled:  true,
			url:      "http://jaeger.svc:16686",
		},
		{
			name:     "default otel_clickhouse with clickhouse down",
			ds:       Datasources{ClickHouseStatus: false},
			provider: "otel_clickhouse",
			enabled:  false,
		},
		{
			name:     "default otel_clickhouse with clickhouse up",
			ds:       Datasources{ClickHouseStatus: true},
			provider: "otel_clickhouse",
			enabled:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := traceProvider(tc.ds); got != tc.provider {
				t.Errorf("traceProvider = %q; want %q", got, tc.provider)
			}
			if got := traceStatus(tc.ds); got != tc.enabled {
				t.Errorf("traceStatus = %v; want %v", got, tc.enabled)
			}
			if got := traceURL(tc.ds); got != tc.url {
				t.Errorf("traceURL = %q; want %q", got, tc.url)
			}
		})
	}
}

// TestProbe_PrometheusConnectionFromCaller verifies PrometheusConnection is
// driven by the caller-computed PrometheusConnected flag, NOT an in-package
// /-/healthy probe. This is what lets query-only backends (Chronosphere/Thanos/
// Mimir/AMP) — which don't serve /-/healthy and require auth — report Connected.
func TestProbe_PrometheusConnectionFromCaller(t *testing.T) {
	// A server that 404s everything, standing in for Chronosphere (no /-/healthy).
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(404)
	}))
	defer backend.Close()
	s := &Service{HTTP: &http.Client{Timeout: time.Second}, Logger: slog.Default()}

	// Connected=true flows through even though /-/healthy would 404.
	if got := s.probe(context.Background(), Datasources{PrometheusURL: backend.URL, PrometheusConnected: true}); !got.PrometheusConnection {
		t.Error("PrometheusConnection should be true when caller reports connected")
	}
	// Connected=false → Disconnected, even with a URL set.
	if got := s.probe(context.Background(), Datasources{PrometheusURL: backend.URL, PrometheusConnected: false}); got.PrometheusConnection {
		t.Error("PrometheusConnection should be false when caller reports not connected")
	}
}

// TestService_RunStopsOnContextCancel covers the lifecycle path — Run()
// posts immediately, then exits cleanly when ctx is canceled.
func TestService_RunStopsOnContextCancel(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := &Service{
		Endpoint:     srv.URL,
		AuthSecret:   "x",
		AgentVersion: "v0",
		Period:       time.Hour, // doesn't fire in test window
		Logger:       slog.Default(),
		Datasources:  func() Datasources { return Datasources{} },
		LightActions: func() []string { return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := posts.Load(); got < 1 {
		t.Errorf("expected at least 1 post; got %d", got)
	}
}

// TestService_NoEndpointIsNoop covers the disabled path — no env, agent
// keeps running, no panic.
func TestService_NoEndpointIsNoop(t *testing.T) {
	s := &Service{Logger: slog.Default()}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		t.Errorf("Run with empty endpoint should be noop: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
