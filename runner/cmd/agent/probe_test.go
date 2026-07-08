package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

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
