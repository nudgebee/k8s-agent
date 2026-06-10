package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nudgebee/nudgebee-agent/pkg/config"
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

// The ES probe must send the configured credentials and honour
// ELASTICSEARCH_SSL_VERIFY=false, so the badge reflects query reachability
// against a secured, self-signed OpenSearch/ES endpoint.
func TestProbeLogsProvider_ESAuthAndInsecureTLS(t *testing.T) {
	var gotAuth string
	es := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path == "/_cluster/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer es.Close()

	cfg := &config.Config{
		ElasticsearchEnabled:   true,
		ElasticsearchURL:       es.URL, // https with a self-signed cert
		ElasticsearchUser:      "admin",
		ElasticsearchPassword:  "pw",
		ElasticsearchSSLVerify: false,
	}
	provider, _, ok, _ := probeLogsProvider(context.Background(), cfg)
	if provider != "ES" {
		t.Fatalf("provider = %q, want ES", provider)
	}
	if !ok {
		t.Fatal("ok = false, want true (insecure TLS + basic auth should succeed)")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:pw"))
	if gotAuth != want {
		t.Fatalf("Authorization = %q, want %q", gotAuth, want)
	}
}

// With strict TLS verification (the default) the probe cannot complete the
// handshake against a self-signed endpoint, so the badge reports unhealthy.
func TestProbeLogsProvider_ESStrictTLSFailsOnSelfSigned(t *testing.T) {
	es := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer es.Close()

	cfg := &config.Config{
		ElasticsearchEnabled:   true,
		ElasticsearchURL:       es.URL,
		ElasticsearchSSLVerify: true,
	}
	provider, _, ok, _ := probeLogsProvider(context.Background(), cfg)
	if provider != "ES" {
		t.Fatalf("provider = %q, want ES", provider)
	}
	if ok {
		t.Fatal("ok = true, want false (strict verify must reject self-signed cert)")
	}
}
