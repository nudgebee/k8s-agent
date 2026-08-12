package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/config"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

// ELASTICSEARCH_ENABLED=false must keep a configured ELASTICSEARCH_URL from
// hijacking logs-provider selection — the agent should fall through to the next
// configured provider (here, Loki).
func TestProbeLogsProvider_ESDisabledFallsThroughToLoki(t *testing.T) {
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mirror the loki gateway: only the API paths are served; /ready 404s.
		if r.URL.Path == "/loki/api/v1/status/buildinfo" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer loki.Close()

	cfg := &config.Config{
		ElasticsearchURL:     "http://es.invalid:9200",
		ElasticsearchEnabled: false,
		LokiURL:              loki.URL,
	}
	provider, url, ok, _ := probeLogsProvider(context.Background(), cfg)
	if provider != "loki" {
		t.Fatalf("provider = %q, want loki", provider)
	}
	if url != loki.URL {
		t.Fatalf("url = %q, want %q", url, loki.URL)
	}
	if !ok {
		t.Fatal("ok = false, want true (buildinfo path should be reachable)")
	}
}

// Regression: a leftover ES URL (e.g. the prod chart's default
// monitoring.svc URL carried into a values file) must NOT hijack logs-provider
// selection when ES isn't explicitly enabled — SigNoz, which the operator did
// configure, must win. Mirrors the customer scenario that motivated defaulting
// ELASTICSEARCH_ENABLED to false.
func TestProbeLogsProvider_StrayESURLDoesNotMaskSignoz(t *testing.T) {
	signoz := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer signoz.Close()

	cfg := &config.Config{
		// Stray/leftover ES URL, ES not enabled (the new default).
		ElasticsearchURL:     "https://elasticsearch-es-internal-http.monitoring.svc:9200",
		ElasticsearchEnabled: false,
		SignozURL:            signoz.URL,
	}
	provider, url, ok, _ := probeLogsProvider(context.Background(), cfg)
	if provider != "signoz" {
		t.Fatalf("provider = %q, want signoz (stray ES URL must not mask SigNoz)", provider)
	}
	if url != signoz.URL || !ok {
		t.Fatalf("url=%q ok=%v; want %q true", url, ok, signoz.URL)
	}
}

// The ES probe must send the configured credentials so the badge reflects
// query reachability against a secured OpenSearch/ES endpoint (which 401s an
// unauthenticated probe).
func TestProbeLogsProvider_ESProbeSendsAuth(t *testing.T) {
	var gotAuth string
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/_cluster/health" && gotAuth != "" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer es.Close()

	cfg := &config.Config{
		ElasticsearchEnabled:  true,
		ElasticsearchURL:      es.URL,
		ElasticsearchUser:     "admin",
		ElasticsearchPassword: "pw",
	}
	provider, _, ok, _ := probeLogsProvider(context.Background(), cfg)
	if provider != "ES" {
		t.Fatalf("provider = %q, want ES", provider)
	}
	if !ok {
		t.Fatal("ok = false, want true (authenticated probe should succeed)")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:pw"))
	if gotAuth != want {
		t.Fatalf("Authorization = %q, want %q", gotAuth, want)
	}
}

// prometheusConnected must report a Chronosphere-style backend as connected:
// it only serves /api/v1/query (not /-/healthy) and requires the bearer token.
// This is the exact case the old /-/healthy probe reported Disconnected.
func TestPrometheusConnected_ChronosphereStyleBackend(t *testing.T) {
	var queried bool
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		// Chronosphere doesn't serve the Prometheus admin/health endpoints.
		if r.URL.Path == "/-/healthy" || r.URL.Path == "/api/v1/status/flags" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/api/v1/query" && gotAuth == "Bearer tok" {
			queried = true
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := prometheus.New(srv.URL, nil)
	c.ExtraHeaders = config.ParseHeaders("Authorization: Bearer tok")
	if !prometheusConnected(context.Background(), c, slog.Default()) {
		t.Error("expected connected=true for query-only backend serving /api/v1/query")
	}
	if !queried {
		t.Error("health check should hit /api/v1/query")
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q; want Bearer tok (auth must be sent)", gotAuth)
	}
}

func TestPrometheusConnected_FailuresReportDisconnected(t *testing.T) {
	// Backend that rejects everything → not connected.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer down.Close()
	if prometheusConnected(context.Background(), prometheus.New(down.URL, nil), slog.Default()) {
		t.Error("expected connected=false when backend returns 500")
	}
	// Nil / unconfigured client → not connected, no panic.
	if prometheusConnected(context.Background(), nil, slog.Default()) {
		t.Error("expected connected=false for nil client")
	}
	if prometheusConnected(context.Background(), prometheus.New("", nil), slog.Default()) {
		t.Error("expected connected=false for empty base URL")
	}
}

// selectedLogsProvider must match probeLogsProvider's precedence
// (pinot → ES → signoz → loki), including that a stray ES URL only wins when
// ES is enabled.
func TestSelectedLogsProvider_Precedence(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{"none", config.Config{}, ""},
		{"loki only", config.Config{LokiURL: "http://loki"}, "loki"},
		{"signoz over loki", config.Config{SignozURL: "http://sz", LokiURL: "http://loki"}, "signoz"},
		{"es disabled → signoz wins", config.Config{ElasticsearchURL: "http://es", ElasticsearchEnabled: false, SignozURL: "http://sz"}, "signoz"},
		{"es enabled beats signoz", config.Config{ElasticsearchURL: "http://es", ElasticsearchEnabled: true, SignozURL: "http://sz"}, "ES"},
		{"pinot wins all", config.Config{PinotURL: "http://pinot", ElasticsearchURL: "http://es", ElasticsearchEnabled: true}, "pinot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := selectedLogsProvider(&tc.cfg)
			if got != tc.want {
				t.Errorf("selectedLogsProvider = %q; want %q", got, tc.want)
			}
		})
	}
}

// A ClickHouse that answers /ping is healthy and has no reason to report.
func TestProbeClickhouse_HealthyReportsNoReason(t *testing.T) {
	t.Setenv("TRACES_ENABLED", "")
	ch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ch.Close()

	host, port, _ := net.SplitHostPort(strings.TrimPrefix(ch.URL, "http://"))
	ok, reason := probeClickhouse(context.Background(), ch.Client(), host, port)
	if !ok {
		t.Errorf("probeClickhouse ok = false; want true")
	}
	if reason != "" {
		t.Errorf("reason = %q; want empty for a healthy ClickHouse", reason)
	}
}

// An unreachable ClickHouse must explain itself — this is the string the UI
// renders under the Traces "Disconnected" pill.
func TestProbeClickhouse_UnreachableReportsReason(t *testing.T) {
	t.Setenv("TRACES_ENABLED", "")
	// Bind and immediately close, so the port is dead but well-formed.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(dead.URL, "http://"))
	dead.Close()

	ok, reason := probeClickhouse(context.Background(), &http.Client{Timeout: 2 * time.Second}, host, port)
	if ok {
		t.Errorf("probeClickhouse ok = true; want false for a dead ClickHouse")
	}
	if reason == "" {
		t.Fatal("reason = empty; want a failure explanation")
	}
	if !strings.Contains(reason, "ClickHouse ping failed") {
		t.Errorf("reason = %q; want it to name the failing probe", reason)
	}
}

// Traces off by operator choice is not a failure, so there's nothing to explain.
func TestProbeClickhouse_ExplicitlyDisabledReportsNoReason(t *testing.T) {
	t.Setenv("TRACES_ENABLED", "false")
	ok, reason := probeClickhouse(context.Background(), http.DefaultClient, "ch.example", "8123")
	if ok {
		t.Errorf("probeClickhouse ok = true; want false under TRACES_ENABLED=false")
	}
	if reason != "" {
		t.Errorf("reason = %q; want empty — disabled on purpose isn't a failure", reason)
	}
}

// No host means traces were never wired up; say so rather than going silent.
func TestProbeClickhouse_UnconfiguredExplainsItself(t *testing.T) {
	t.Setenv("TRACES_ENABLED", "")
	ok, reason := probeClickhouse(context.Background(), http.DefaultClient, "", "8123")
	if ok {
		t.Errorf("probeClickhouse ok = true; want false with no CLICKHOUSE_HOST")
	}
	if !strings.Contains(reason, "CLICKHOUSE_HOST") {
		t.Errorf("reason = %q; want it to name the missing env var", reason)
	}
}

// The reason string ships to the backend and renders in the UI, so a URL-form
// CLICKHOUSE_HOST must not leak its credentials into it.
func TestRedactUserinfo(t *testing.T) {
	cases := []struct{ in, want string }{
		{"clickhouse.svc:8123", "clickhouse.svc:8123"},
		{"https://otel.last9.io:443", "https://otel.last9.io:443"},
		{"https://admin:hunter2@otel.last9.io:443", "https://otel.last9.io:443"},
		{"admin:hunter2@clickhouse.svc:8123", "clickhouse.svc:8123"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := redactUserinfo(tc.in); got != tc.want {
			t.Errorf("redactUserinfo(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
