// Package config loads agent configuration from environment variables.
//
// Env vars (chart-compatibility names):
//
//	NUDGEBEE_AUTH_SECRET_KEY   - shared secret for relay Basic-Auth and HMAC
//	WEBSOCKET_RELAY_ADDRESS    - relay /register URL (ws:// or wss://)
//	NUDGEBEE_ENDPOINT          - backend HTTP base URL (for discovery/alert forward)
//	ACCOUNT_ID                 - tenant account id (sent in greeting; informational)
//	CLUSTER_NAME               - cluster name (informational)
package config

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AuthSecretKey   string
	RelayURL        string
	BackendEndpoint string
	AccountID       string
	ClusterName     string

	// Optional datasource URLs. Empty = subsystem disabled.
	PrometheusURL     string
	PrometheusHeaders string // raw "Header: value" string, parsed into http.Header
	LokiURL           string
	LokiHeaders       string

	// Elasticsearch
	ElasticsearchURL      string
	ElasticsearchUser     string
	ElasticsearchPassword string
	ElasticsearchAPIKey   string

	// Signoz
	SignozURL    string
	SignozAPIKey string

	// Jaeger
	JaegerURL string

	// Chronosphere
	ChronosphereURL    string
	ChronosphereAPIKey string

	// Pinot
	PinotURL       string
	PinotAuthToken string // optional Bearer token
	PinotUsername  string // optional Basic-Auth
	PinotPassword  string

	// HTTP proxy targets — semicolon-delimited "name=url" pairs.
	// Example: "grafana=http://grafana:3000;datadog=https://api.datadoghq.com"
	// Use "*=ignored" to opt into explicit-URL targets (security risk; off by default).
	HTTPProxyTargets string

	// Loki rules HTTP API URL (separate from LOKI_URL which is for queries).
	LokiRulesURL string

	// GCP — enables gke_logs + gke_traces. Auth via ADC / Workload Identity.
	GCPEnabled   bool
	GCPProjectID string

	// HTTP server (alerts intake + healthz). Default :5000.
	HTTPListenAddr string

	// Discovery: enabled when DiscoveryEnabled=true. Resync interval is
	// DISCOVERY_RESYNC (default 30m); KUBECONFIG env is honoured by client-go.
	DiscoveryEnabled bool
	DiscoveryResync  time.Duration
	// AlertRulesInterval is how often the agent pushes Prometheus alert
	// rules (api/v1/rules + PrometheusRule CRDs) to the collector's
	// /v1/k8s/discovery endpoint. Gated by DiscoveryEnabled (same toggle
	// that controls the rest of the discovery push loop).
	AlertRulesInterval time.Duration

	// Scalability knobs (see runner/docs/SCALABILITY_AUDIT.md).
	//
	// DiscoverySnapshotBatching turns the full-load snapshot into N-item
	// batches (DiscoveryBatchSize each) using the envelope's batch fields.
	// OFF by default: it changes the /v1/k8s/discovery wire behavior and the
	// collector must support batch reassembly + deferred deletion-reconcile
	// (accumulate by batch_id, reconcile only on is_last_batch) before it is
	// safe to enable.
	DiscoverySnapshotBatching bool
	DiscoveryBatchSize        int
	// IncrementalBatchSize coalesces up to N queued informer events into one
	// incremental envelope. Default 1 (no coalescing — wire-identical to the
	// historical one-item-per-event path). Raising it sends a multi-item
	// `data` list and requires the collector's incremental handler to iterate
	// `data` rather than read data[0].
	IncrementalBatchSize int
	// IncrementalBatchWindow optionally lets the coalescer wait briefly to
	// accumulate more events. Default 0 (drain the current backlog only).
	IncrementalBatchWindow time.Duration
	// EmitTombstones makes the incremental path emit a `deleted:true` tombstone
	// on resource deletion instead of waiting for the next full snapshot. OFF
	// by default: requires collector support for incremental deletes.
	EmitTombstones bool

	// ForwardPoolSize bounds concurrent event-forward goroutines in pkg/alerts
	// (kubewatch + AlertManager intake). Excess is shed (HTTP 202 already
	// returned). Default 64.
	ForwardPoolSize int
	// RelayHandlerPoolSize bounds concurrent inbound WS handler goroutines in
	// pkg/relay as a soft outer guard against goroutine pile-up. Default 32.
	RelayHandlerPoolSize int

	// Kube primitives (group B): enabled when KubeEnabled=true. Independent
	// of discovery so an operator can run primitives-only without paying
	// for the full informer cache.
	KubeEnabled bool

	// Scanners (group F): enabled when ScannersEnabled=true. Needs a
	// namespace + service account that Trivy/Popeye/KRR/etc. can run as.
	ScannersEnabled       bool
	ScannerNamespace      string
	ScannerServiceAccount string

	// PodExecEnabled (group D): pod_bash_enricher / pod_script_run_enricher.
	// MUST be paired with RSA partial-keys auth in production.
	PodExecEnabled bool

	// MutateEnabled (group D): delete_pod / cordon / uncordon / rollout_restart.
	// MUST be paired with RSA partial-keys auth in production.
	MutateEnabled   bool
	AlertManagerURL string

	// RSA private key for the partial-keys auth path. PEM-encoded file path.
	// When unset, partial-keys auth is disabled — mutate/podexec actions
	// fall back to HMAC signature only.
	RSAPrivateKeyPath string

	// ClickHouse — backs the `query_data` action. The chart sets these from
	// the in-cluster ClickHouse Service the agent ships alongside.
	ClickHouseEnabled  bool
	ClickHouseHost     string
	ClickHousePort     int
	ClickHouseUser     string
	ClickHousePassword string
	ClickHouseDB       string
	ClickHouseSSL      bool
}

// FromEnv reads config from env vars and returns it. Missing required fields
// produce an error; missing optional fields are left blank.
func FromEnv() (*Config, error) {
	c := &Config{
		AuthSecretKey:         os.Getenv("NUDGEBEE_AUTH_SECRET_KEY"),
		RelayURL:              os.Getenv("WEBSOCKET_RELAY_ADDRESS"),
		BackendEndpoint:       os.Getenv("NUDGEBEE_ENDPOINT"),
		AccountID:             os.Getenv("ACCOUNT_ID"),
		ClusterName:           os.Getenv("CLUSTER_NAME"),
		PrometheusURL:         os.Getenv("PROMETHEUS_URL"),
		PrometheusHeaders:     os.Getenv("PROMETHEUS_HEADERS"), // matches runner.yaml secret
		LokiURL:               os.Getenv("LOKI_URL"),
		LokiHeaders:           os.Getenv("LOKI_EXTRA_HEADER"), // matches runner.yaml secret
		ElasticsearchURL:      os.Getenv("ELASTICSEARCH_URL"),
		ElasticsearchUser:     os.Getenv("ELASTICSEARCH_USERNAME"),
		ElasticsearchPassword: os.Getenv("ELASTICSEARCH_PASSWORD"),
		ElasticsearchAPIKey:   os.Getenv("ELASTICSEARCH_APIKEY"),
		SignozURL:             os.Getenv("SIGNOZ_URL"),
		SignozAPIKey:          os.Getenv("SIGNOZ_API_KEY"),
		JaegerURL:             os.Getenv("JAEGER_URL"),
		ChronosphereURL:       os.Getenv("CHRONOSPHERE_URL"),
		ChronosphereAPIKey:    os.Getenv("CHRONOSPHERE_API_KEY"),
		PinotURL:              os.Getenv("PINOT_URL"),
		PinotAuthToken:        os.Getenv("PINOT_AUTH_TOKEN"),
		PinotUsername:         os.Getenv("PINOT_USERNAME"),
		PinotPassword:         os.Getenv("PINOT_PASSWORD"),
		HTTPProxyTargets:      os.Getenv("HTTP_PROXY_TARGETS"),
		LokiRulesURL:          os.Getenv("LOKI_RULES_URL"),
		HTTPListenAddr:        cmp(os.Getenv("HTTP_LISTEN_ADDR"), ":5000"),
		// K8s subsystems default-on so the agent is drop-in compatible with
		// the legacy runner Deployment — no env additions needed for cutover.
		// Operators can opt out per-subsystem via DISCOVERY_ENABLED=false etc.
		DiscoveryEnabled:   envBool("DISCOVERY_ENABLED", true),
		DiscoveryResync:    parseDuration(os.Getenv("DISCOVERY_RESYNC"), 30*time.Minute),
		AlertRulesInterval: parseDuration(os.Getenv("ALERT_RULES_INTERVAL"), 30*time.Minute),
		// Scalability knobs — wire-changing features OFF by default (need
		// collector support); see runner/docs/SCALABILITY_AUDIT.md.
		DiscoverySnapshotBatching: envBool("DISCOVERY_SNAPSHOT_BATCHING", false),
		DiscoveryBatchSize:        envInt("DISCOVERY_BATCH_SIZE", 1000),
		IncrementalBatchSize:      envInt("DISCOVERY_INCREMENTAL_BATCH_SIZE", 1),
		IncrementalBatchWindow:    parseDuration(os.Getenv("DISCOVERY_INCREMENTAL_BATCH_WINDOW"), 0),
		EmitTombstones:            envBool("DISCOVERY_EMIT_TOMBSTONES", false),
		ForwardPoolSize:           envInt("FORWARD_POOL_SIZE", 64),
		RelayHandlerPoolSize:      envInt("RELAY_HANDLER_POOL_SIZE", 32),
		KubeEnabled:               envBool("KUBE_ENABLED", true),
		PodExecEnabled:            envBool("PODEXEC_ENABLED", true),
		// Off by default — these need extra config (RSA key, scanner SA, GCP ADC):
		ScannersEnabled:       envBool("SCANNERS_ENABLED", false),
		ScannerNamespace:      cmp(os.Getenv("SCANNER_NAMESPACE"), "nudgebee-agent"),
		ScannerServiceAccount: os.Getenv("SCANNER_SERVICE_ACCOUNT"),
		MutateEnabled:         envBool("MUTATE_ENABLED", false),
		AlertManagerURL:       os.Getenv("ALERTMANAGER_URL"),
		RSAPrivateKeyPath:     os.Getenv("RSA_PRIVATE_KEY_PATH"),
		GCPEnabled:            envBool("GCP_ENABLED", false),
		GCPProjectID:          os.Getenv("GCP_PROJECT_ID"),
		ClickHouseEnabled:     envBool("CLICKHOUSE_ENABLED", true),
		ClickHouseHost:        os.Getenv("CLICKHOUSE_HOST"),
		ClickHousePort:        envInt("CLICKHOUSE_PORT", 8123),
		ClickHouseUser:        os.Getenv("CLICKHOUSE_USER"),
		ClickHousePassword:    os.Getenv("CLICKHOUSE_PASSWORD"),
		ClickHouseDB:          cmp(os.Getenv("CLICKHOUSE_DB"), "default"),
		ClickHouseSSL:         envBool("CLICKHOUSE_SSL_ENABLED", false),
	}
	if c.RelayURL == "" {
		return nil, errors.New("WEBSOCKET_RELAY_ADDRESS not set")
	}
	if c.AuthSecretKey == "" {
		return nil, errors.New("NUDGEBEE_AUTH_SECRET_KEY not set")
	}
	return c, nil
}

// cmp returns a if non-empty, otherwise fallback.
func cmp(a, fallback string) string {
	if a != "" {
		return a
	}
	return fallback
}

// envBool reads a "true"/"false" env var with a fallback when unset. Any
// value that's neither "true" nor "false" is treated as fallback (so a
// stray empty string or "1" doesn't silently flip a setting).
func envBool(name string, fallback bool) bool {
	switch os.Getenv(name) {
	case "true":
		return true
	case "false":
		return false
	default:
		return fallback
	}
}

// envInt reads an int env var with a fallback when unset or invalid.
func envInt(name string, fallback int) int {
	s := os.Getenv(name)
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

func parseDuration(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

// ParseTargets parses a "name=url;name=url" string into a map. Used for
// HTTP_PROXY_TARGETS env var.
func ParseTargets(s string) map[string]string {
	out := map[string]string{}
	if s == "" {
		return out
	}
	for _, pair := range strings.Split(s, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		i := strings.IndexByte(pair, '=')
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(pair[:i])
		v := strings.TrimSpace(pair[i+1:])
		if k != "" {
			out[k] = v
		}
	}
	return out
}

// ParseHeaders splits a comma-separated "Header: value" string into an
// http.Header. Returns an empty Header for empty input. Same shape used
// by the GRAFANA_EXTRA_HEADER / LOKI_EXTRA_HEADER pattern (a single
// "Header: value" or comma-separated list).
func ParseHeaders(s string) http.Header {
	h := http.Header{}
	if s == "" {
		return h
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.IndexByte(part, ':')
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(part[:i])
		v := strings.TrimSpace(part[i+1:])
		if k != "" {
			h.Add(k, v)
		}
	}
	return h
}
