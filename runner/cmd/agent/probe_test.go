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
