package config

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestFromEnv_RequiredFieldsErrorOnMissing(t *testing.T) {
	t.Setenv("WEBSOCKET_RELAY_ADDRESS", "")
	t.Setenv("NUDGEBEE_AUTH_SECRET_KEY", "x")
	if _, err := FromEnv(); err == nil {
		t.Error("expected error for missing WEBSOCKET_RELAY_ADDRESS")
	}

	t.Setenv("WEBSOCKET_RELAY_ADDRESS", "ws://relay")
	t.Setenv("NUDGEBEE_AUTH_SECRET_KEY", "")
	if _, err := FromEnv(); err == nil {
		t.Error("expected error for missing NUDGEBEE_AUTH_SECRET_KEY")
	}
}

func TestFromEnv_ReadsAllFields(t *testing.T) {
	t.Setenv("WEBSOCKET_RELAY_ADDRESS", "ws://relay")
	t.Setenv("NUDGEBEE_AUTH_SECRET_KEY", "secret")
	t.Setenv("NUDGEBEE_ENDPOINT", "https://api.example.com")
	t.Setenv("ACCOUNT_ID", "acc-1")
	t.Setenv("CLUSTER_NAME", "prod-cluster")
	t.Setenv("PROMETHEUS_URL", "http://prom:9090")
	t.Setenv("PROMETHEUS_HEADERS", "X-Scope-OrgID: t1")
	t.Setenv("LOKI_URL", "http://loki:3100")
	t.Setenv("LOKI_EXTRA_HEADER", "X-Scope-OrgID: t1")
	t.Setenv("HTTP_LISTEN_ADDR", ":7000")
	t.Setenv("DISCOVERY_ENABLED", "true")
	t.Setenv("DISCOVERY_RESYNC", "10m")

	c, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	want := &Config{
		AuthSecretKey:     "secret",
		RelayURL:          "ws://relay",
		BackendEndpoint:   "https://api.example.com",
		AccountID:         "acc-1",
		ClusterName:       "prod-cluster",
		PrometheusURL:     "http://prom:9090",
		PrometheusHeaders: "X-Scope-OrgID: t1",
		LokiURL:           "http://loki:3100",
		LokiHeaders:       "X-Scope-OrgID: t1",
		HTTPListenAddr:    ":7000",
		// ES gates logs provider; defaults on when ELASTICSEARCH_ENABLED unset.
		ElasticsearchEnabled: true,
		// K8s subsystems default-on (drop-in compatible with the legacy runner);
		// operators opt out per-subsystem via DISCOVERY_ENABLED=false etc.
		DiscoveryEnabled:   true,
		DiscoveryResync:    10 * time.Minute,
		AlertRulesInterval: 30 * time.Minute, // default when ALERT_RULES_INTERVAL unset
		// Scalability knobs — defaults applied when their env vars are unset.
		DiscoveryBatchSize:   1000,
		IncrementalBatchSize: 1,
		ForwardPoolSize:      64,
		RelayHandlerPoolSize: 32,
		KubeEnabled:          true,
		PodExecEnabled:       true,
		ScannerNamespace:     "nudgebee-agent", // applied as default even when SCANNER_NAMESPACE unset
		// ClickHouse defaults: enabled, port 8123, db "default" — so the
		// chart's existing CH config (CLICKHOUSE_HOST in runner-secret)
		// just works.
		ClickHouseEnabled: true,
		ClickHousePort:    8123,
		ClickHouseDB:      "default",
	}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("Config\n got:  %+v\n want: %+v", c, want)
	}
}

func TestFromEnv_DefaultsWhenOptionalMissing(t *testing.T) {
	t.Setenv("WEBSOCKET_RELAY_ADDRESS", "ws://relay")
	t.Setenv("NUDGEBEE_AUTH_SECRET_KEY", "secret")
	t.Setenv("HTTP_LISTEN_ADDR", "")
	t.Setenv("DISCOVERY_RESYNC", "")
	t.Setenv("DISCOVERY_ENABLED", "")
	t.Setenv("KUBE_ENABLED", "")
	t.Setenv("PODEXEC_ENABLED", "")
	t.Setenv("SCANNERS_ENABLED", "")
	t.Setenv("MUTATE_ENABLED", "")
	t.Setenv("GCP_ENABLED", "")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPListenAddr != ":5000" {
		t.Errorf("HTTPListenAddr default = %q; want :5000", c.HTTPListenAddr)
	}
	if c.DiscoveryResync != 30*time.Minute {
		t.Errorf("DiscoveryResync default = %v; want 30m", c.DiscoveryResync)
	}
	// K8s subsystems default ON for drop-in compatibility with the legacy runner.
	if !c.DiscoveryEnabled {
		t.Error("DiscoveryEnabled default should be true (drop-in compat)")
	}
	if !c.KubeEnabled {
		t.Error("KubeEnabled default should be true (drop-in compat)")
	}
	if !c.PodExecEnabled {
		t.Error("PodExecEnabled default should be true (drop-in compat)")
	}
	// These need extra config (RSA key / scanner SA / GCP ADC) so they
	// stay opt-in.
	if c.ScannersEnabled {
		t.Error("ScannersEnabled default should be false (needs SCANNER_SERVICE_ACCOUNT)")
	}
	if c.MutateEnabled {
		t.Error("MutateEnabled default should be false (needs RSA_PRIVATE_KEY_PATH)")
	}
	if c.GCPEnabled {
		t.Error("GCPEnabled default should be false (needs GCP_PROJECT_ID + ADC)")
	}
}

func TestFromEnv_ElasticsearchEnabled(t *testing.T) {
	t.Setenv("WEBSOCKET_RELAY_ADDRESS", "ws://relay")
	t.Setenv("NUDGEBEE_AUTH_SECRET_KEY", "secret")

	// Default-on so a bare ELASTICSEARCH_URL keeps working.
	t.Setenv("ELASTICSEARCH_ENABLED", "")
	if c, err := FromEnv(); err != nil {
		t.Fatal(err)
	} else if !c.ElasticsearchEnabled {
		t.Error("ElasticsearchEnabled should default to true when unset")
	}

	// Explicit false lets the URL stay configured (for actions) without
	// selecting ES as the logs provider.
	t.Setenv("ELASTICSEARCH_ENABLED", "false")
	if c, err := FromEnv(); err != nil {
		t.Fatal(err)
	} else if c.ElasticsearchEnabled {
		t.Error("ElasticsearchEnabled should be false when ELASTICSEARCH_ENABLED=false")
	}
}

func TestEnvBool_FallbackBehavior(t *testing.T) {
	t.Setenv("X_TEST", "")
	if !envBool("X_TEST", true) {
		t.Error("empty value should fall back to true")
	}
	if envBool("X_TEST", false) {
		t.Error("empty value should fall back to false")
	}

	t.Setenv("X_TEST", "true")
	if !envBool("X_TEST", false) {
		t.Error(`"true" should override fallback`)
	}
	t.Setenv("X_TEST", "false")
	if envBool("X_TEST", true) {
		t.Error(`"false" should override fallback`)
	}
	t.Setenv("X_TEST", "1") // unrecognised — fallback applies
	if !envBool("X_TEST", true) {
		t.Error(`unrecognised value should fall back to true`)
	}
	if envBool("X_TEST", false) {
		t.Error(`unrecognised value should fall back to false`)
	}
}

func TestParseDuration_FallbackOnInvalid(t *testing.T) {
	if got := parseDuration("not-a-duration", 7*time.Second); got != 7*time.Second {
		t.Errorf("parseDuration invalid = %v; want fallback", got)
	}
	if got := parseDuration("", 7*time.Second); got != 7*time.Second {
		t.Errorf("parseDuration empty = %v; want fallback", got)
	}
	if got := parseDuration("90s", time.Second); got != 90*time.Second {
		t.Errorf("parseDuration 90s = %v; want 90s", got)
	}
}

func TestParseHeaders(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want http.Header
	}{
		{"empty", "", http.Header{}},
		{"one", "X-Scope-OrgID: tenant-1", http.Header{"X-Scope-Orgid": []string{"tenant-1"}}},
		{
			"multi",
			"X-Scope-OrgID: tenant-1, Authorization: Bearer abc",
			http.Header{
				"X-Scope-Orgid": []string{"tenant-1"},
				"Authorization": []string{"Bearer abc"},
			},
		},
		{"trims_whitespace", "  X-A : v ", http.Header{"X-A": []string{"v"}}},
		{"skips_invalid", "no-colon-here, X-Y: ok", http.Header{"X-Y": []string{"ok"}}},
		{"value_can_contain_colons", "Authorization: Bearer x:y:z",
			http.Header{"Authorization": []string{"Bearer x:y:z"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseHeaders(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ParseHeaders(%q)\n got:  %v\n want: %v", c.in, got, c.want)
			}
		})
	}
}
