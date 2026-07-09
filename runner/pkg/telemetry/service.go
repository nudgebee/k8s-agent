// Package telemetry posts a periodic ClusterStatus snapshot to the backend so
// the UI can show "Prometheus / AlertManager / Loki / OpenCost / Traces /
// NodeAgent connected".
//
// The collector reads a handful of fields directly out of `activity_stats`
// : prometheusUrl, prometheusConnection, logsConnection,
// logsConnectionProvider, traceProvider, tracesEnabled) and stores the rest
// of the dict as `connection_status` JSON in the `agent` table — that's what
// the UI reads to render the per-datasource Connected/Disconnected pills.
//
// Inputs that need K8s/Prometheus access (NodeAgentCount via PromQL,
// LogsProviderStatus via Loki/Signoz health, OpencostStatus probe URL) are
// computed by the caller and passed in via Datasources, so this package stays
// dependency-free and unit-testable.
package telemetry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ActivityStats is the activity-stats payload. Pydantic on the legacy side
// emits camelCase wire keys for all fields except the snake_case ones
// (`schedule_jobs`, `log_provider_config`); we match exactly so the
// collector + UI parsers see what they expect.
//
// `Optional[T]` fields are emitted as null when unset — Go's `omitempty`
// mostly produces the same output (skipping vs null is a difference the
// collector tolerates because every read uses `.get()` with a default).
// We keep `omitempty` on the strings/maps and explicit emit on
// numerics/bools so a `false`/`0` is always present.
type ActivityStats struct {
	AlertManagerConnection  bool     `json:"alertManagerConnection"`
	PrometheusConnection    bool     `json:"prometheusConnection"`
	PrometheusRetentionTime string   `json:"prometheusRetentionTime,omitempty"`
	TracesEnabled           bool     `json:"tracesEnabled"`
	LogsConnectionProvider  string   `json:"logsConnectionProvider,omitempty"`
	LogsConnection          bool     `json:"logsConnection"`
	NodeAgentConnection     bool     `json:"nodeAgentConnection"`
	NodeAgentCount          int      `json:"nodeAgentCount"`
	OpencostConnection      bool     `json:"opencostConnection"`
	GrafanaEnabled          bool     `json:"grafanaEnabled"`
	Errors                  []string `json:"errors"`
	// Per-feature health-check failure reasons, populated only when the
	// corresponding *Connection probe failed (e.g. "HTTP 401: token is
	// expired"). Empty/omitted when healthy or the datasource is unconfigured.
	// The UI renders these next to the "Disconnected" status.
	PrometheusConnectionError   string            `json:"prometheusConnectionError,omitempty"`
	AlertManagerConnectionError string            `json:"alertManagerConnectionError,omitempty"`
	LogsConnectionError         string            `json:"logsConnectionError,omitempty"`
	OpencostConnectionError     string            `json:"opencostConnectionError,omitempty"`
	InstallationNamespace       string            `json:"installationNamespace,omitempty"`
	LogProviderConfig           map[string]any    `json:"log_provider_config,omitempty"`
	LogProviderURL              string            `json:"logProviderUrl,omitempty"`
	PrometheusURL               string            `json:"prometheusUrl,omitempty"`
	PrometheusAdditionalLabels  map[string]string `json:"prometheusAdditionalLabels,omitempty"`
	AlertManagerURL             string            `json:"alertmanagerUrl,omitempty"`
	OpencostURL                 string            `json:"opencostUrl,omitempty"`
	TracesURL                   string            `json:"tracesUrl,omitempty"`
	AutoScalerVersion           string            `json:"autoScalerVersion,omitempty"`
	AutoScalerEnabled           bool              `json:"autoScalerEnabled"`
	AutoScalerNamespace         string            `json:"autoScalerNamespace,omitempty"`
	AutoScalerType              string            `json:"autoScalerType,omitempty"`
	AgentURL                    string            `json:"agentUrl,omitempty"`
	AgentWSEnabled              bool              `json:"agentWSEnabled"`
	HealthCheckDuration         float64           `json:"healthCheckDuration,omitempty"`
	TraceProvider               string            `json:"traceProvider,omitempty"`
	TraceProviderConfig         map[string]any    `json:"traceProviderConfig,omitempty"`
}

// ClusterStatus is the wire payload posted to /v1/k8s/telemetry.
type ClusterStatus struct {
	AccountID    string        `json:"account_id,omitempty"`
	ClusterID    string        `json:"cluster_id,omitempty"`
	Version      string        `json:"version"`
	LastAlertAt  string        `json:"last_alert_at,omitempty"`
	LightActions []string      `json:"light_actions"`
	Stats        ClusterStats  `json:"stats"`
	ActivityStat ActivityStats `json:"activity_stats"`
	Playbooks    []any         `json:"playbooks"`
	SchedJobs    []any         `json:"schedule_jobs,omitempty"`
}

// ClusterStats is the cluster-stats payload. `the backend` reads
// `provider` and `k8s_version`; the six provider_* fields + `cluster_name`
// are consumed by the cloud_account_attrs auto-populate path.
//
// The provider_* + cluster_name fields intentionally omit `omitempty` — Python
// declares them as `Optional[str] = ""` (Pydantic v1 default), which always
// emits the empty string. The backend's UPSERT skips empty values either way,
// but matching the Python wire shape exactly keeps cross-language diffs tight.
type ClusterStats struct {
	Provider              string `json:"provider"`
	K8sVersion            string `json:"k8s_version,omitempty"`
	ClusterName           string `json:"cluster_name"`
	ProviderAccountNumber string `json:"provider_account_number"`
	ProviderRegion        string `json:"provider_region"`
	ProviderZone          string `json:"provider_zone"`
	ProviderProjectID     string `json:"provider_project_id"`
	ProviderResourceGroup string `json:"provider_resource_group"`
}

// Datasources is the snapshot the caller assembles before each tick. Fields
// map 1:1 to the PrometheusHealthStatus inputs + the trace-helper env
// vars so probe() can mirror the legacy logic without reaching into env
// or running its own queries.
type Datasources struct {
	// URLs the agent has discovered (env or autodiscovery).
	PrometheusURL   string
	AlertManagerURL string
	LokiURL         string // (informational; logs provider URL is LogsProviderURL below)
	OpencostURL     string

	// Logs provider — caller picks based on env (ELASTICSEARCH_ENABLED →
	// SIGNOZ_ENABLED → loki) and probes health.
	LogsProvider       string // "ES" | "signoz" | "loki" | ""
	LogsProviderURL    string
	LogsProviderStatus bool
	// LogsProviderError is the health-probe failure reason for the logs
	// provider, set by the caller when LogsProviderStatus is false (e.g.
	// "HTTP 401: token is expired"). Empty when healthy or unconfigured.
	LogsProviderError string
	LogProviderConfig map[string]any

	// PrometheusConnected is the caller-computed connectivity result: an
	// authenticated `vector(1)` query via the prometheus client (which carries
	// PROMETHEUS_HEADERS). Computed by the caller — like NodeAgentCount — so it
	// reflects "can we actually query metrics?" rather than "does GET /-/healthy
	// return 2xx?". Query-only backends (Chronosphere, Thanos Query, Grafana
	// Mimir, Amazon Managed Prometheus) don't serve the Prometheus /-/healthy
	// admin endpoint and require auth, so the old unauthenticated /-/healthy
	// probe reported them Disconnected even when queries worked.
	PrometheusConnected bool
	// PrometheusConnectedError is the failure reason set by the caller when
	// PrometheusConnected is false (query error / non-success status). Empty
	// when connected or unconfigured. Surfaced next to "Disconnected" in the UI.
	PrometheusConnectedError string

	// Prometheus retention from `flags.retentionTime` (utils.get_prometheus_flags).
	PrometheusRetentionTime    string
	PrometheusAdditionalLabels map[string]string

	// Trace inputs — exactly the ones the get_trace_* helpers consult.
	TraceTable                string // TRACE_TABLE
	JaegerEnabled             bool   // JAEGER_ENABLED
	JaegerQueryURL            string // JAEGER_QUERY_URL
	ChronosphereTracesEnabled bool   // CHRONOSPHERE_TRACES_ENABLED
	ChronosphereTracesURL     string // CHRONOSPHERE_TRACES_URL
	// ClickHouseStatus is the `clickhouse_status` flag — used only as the
	// fallback for tracesEnabled when no other provider matches.
	ClickHouseStatus bool

	// Node-agent: count of `up{job=~"...nudgebee(-.*)?-node-agent"}` from
	// Prometheus, computed by the caller.
	NodeAgentCount int

	// Auto-scaler info from KarpenterDiscovery / AutoScalerDiscovery /
	// AutoScalerForGKEDiscovery.
	AutoScalerEnabled   bool
	AutoScalerType      string
	AutoScalerVersion   string
	AutoScalerNamespace string

	// Grafana/Opencost connection — caller may probe these out-of-band; if
	// caller leaves them at the default we fall back to in-package HTTP
	// probes against the URL fields above.
	GrafanaEnabled bool

	// AgentURL is published to the UI as the cluster's "agent" address so
	// pop-out actions know where to call. Defaults to AGENT_HTTP_URL env.
	AgentURL string
}

// Service is the periodic poster.
type Service struct {
	Endpoint     string // e.g. https://collector.dev.nudgebee.pollux.in
	AuthSecret   string
	AccountID    string
	ClusterName  string
	AgentVersion string
	Namespace    string
	Period       time.Duration
	HTTP         *http.Client
	Logger       *slog.Logger

	// Mutable input the agent updates as it learns about its environment.
	Datasources func() Datasources
	// LightActions is the same set the dispatcher uses for light-action auth;
	// the UI shows it as the agent's action surface.
	LightActions func() []string

	// Provider is the cluster-provider snapshot, computed once at agent
	// startup (DetectProvider) and passed in by main.go. Stable for the
	// process lifetime; we don't re-detect per tick. Zero-value is fine —
	// empty fields get skipped by the backend's UPSERT.
	Provider ProviderInfo

	// K8sVersion is the kubernetes server version (`v1.33.10-gke.1234`
	// etc.) fetched once at startup via Discovery().ServerVersion(). The
	// backend stores it on `agent.k8s_version`; the UI's Agent Health
	// card renders it as "K8s(Provider/Version)". Empty string is
	// tolerated (UI shows `GKE /`).
	K8sVersion string
}

// Run blocks until ctx is done. Posts immediately on start so the UI flips
// Connected within one tick of agent boot, then every Period after.
func (s *Service) Run(ctx context.Context) error {
	if s.Endpoint == "" {
		s.Logger.Info("telemetry disabled — backend endpoint empty")
		<-ctx.Done()
		return nil
	}
	if s.HTTP == nil {
		s.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if s.Period <= 0 {
		s.Period = 60 * time.Second
	}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := s.postOnce(ctx); err != nil {
				s.Logger.Warn("telemetry post failed", "err", err)
			}
			t.Reset(s.Period)
		}
	}
}

func (s *Service) postOnce(ctx context.Context) error {
	if s.HTTP == nil {
		// Defensive — Run() also initializes this, but tests call postOnce
		// directly, and either path produces the same client.
		s.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	start := time.Now()
	ds := Datasources{}
	if s.Datasources != nil {
		ds = s.Datasources()
	}
	stats := s.probe(ctx, ds)
	stats.HealthCheckDuration = time.Since(start).Seconds()
	stats.InstallationNamespace = s.Namespace
	stats.AgentWSEnabled = true
	stats.Errors = []string{} // explicit empty slice — Pydantic deserializer rejects null

	light := []string{}
	if s.LightActions != nil {
		light = s.LightActions()
	}
	body := ClusterStatus{
		ClusterID:    s.ClusterName,
		Version:      s.AgentVersion,
		LightActions: light,
		Stats: ClusterStats{
			Provider:              s.Provider.Provider,
			K8sVersion:            s.K8sVersion,
			ClusterName:           s.ClusterName,
			ProviderAccountNumber: s.Provider.AccountNumber,
			ProviderRegion:        s.Provider.Region,
			ProviderZone:          s.Provider.Zone,
			ProviderProjectID:     s.Provider.ProjectID,
			ProviderResourceGroup: s.Provider.ResourceGroup,
		},
		ActivityStat: stats,
		Playbooks:    []any{},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint+"/v1/k8s/telemetry", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.AuthSecret != "" {
		// Bare base64, no "Basic " prefix — same auth shape the collector
		// expects on /v1/k8s/discovery.
		req.Header.Set("Authorization", base64.StdEncoding.EncodeToString([]byte(s.AuthSecret)))
	}
	if s.AccountID != "" {
		req.Header.Set("X-NB-Account-Id", s.AccountID)
	}
	if s.ClusterName != "" {
		req.Header.Set("X-NB-Cluster", s.ClusterName)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	s.Logger.Debug("telemetry posted",
		"prom", stats.PrometheusConnection,
		"logs", stats.LogsConnection,
		"alertmanager", stats.AlertManagerConnection,
		"opencost", stats.OpencostConnection,
		"node_agent", stats.NodeAgentCount,
		"trace_provider", stats.TraceProvider,
	)
	return nil
}

// probe assembles ActivityStats from Datasources by HTTP-probing the simple
// healthy endpoints and applying the get_trace_* helpers verbatim.
//
// Probes implemented in-package:
//
//	AlertManager /-/healthy  (silence_utils.get_alertmanager_silences_connection)
//	OpenCost    /healthz
//
// Inputs the caller pre-computes (see Datasources):
//
//	PrometheusConnected   — authenticated `vector(1)` query via prometheus client
//	NodeAgentCount        — Prometheus query, mirrors lines 246-264
//	LogsProviderStatus    — loki/es/signoz health, mirrors lines 204-242
//	GrafanaEnabled        — grafana_client.health(), mirrors lines 284-293
//	ClickHouseStatus      — db.health(), mirrors lines 297-305
//	AutoScaler*           — Karpenter/CA/GKE detection, mirrors lines 309-350
func (s *Service) probe(ctx context.Context, ds Datasources) ActivityStats {
	out := ActivityStats{
		PrometheusURL:              ds.PrometheusURL,
		PrometheusRetentionTime:    ds.PrometheusRetentionTime,
		PrometheusAdditionalLabels: ds.PrometheusAdditionalLabels,
		AlertManagerURL:            ds.AlertManagerURL,
		LogProviderURL:             ds.LogsProviderURL,
		LogProviderConfig:          ds.LogProviderConfig,
		LogsConnectionProvider:     ds.LogsProvider,
		LogsConnection:             ds.LogsProviderStatus,
		LogsConnectionError:        ds.LogsProviderError,
		NodeAgentCount:             ds.NodeAgentCount,
		NodeAgentConnection:        ds.NodeAgentCount > 0, // line 254: count > 0
		OpencostURL:                ds.OpencostURL,
		GrafanaEnabled:             ds.GrafanaEnabled,
		AutoScalerEnabled:          ds.AutoScalerEnabled,
		AutoScalerType:             ds.AutoScalerType,
		AutoScalerVersion:          ds.AutoScalerVersion,
		AutoScalerNamespace:        ds.AutoScalerNamespace,
		AgentURL:                   ds.AgentURL,
	}

	// Prometheus: connectivity is computed by the caller via an authenticated
	// query (see Datasources.PrometheusConnected) so query-only backends that
	// don't serve /-/healthy (Chronosphere/Thanos/Mimir/AMP) report correctly.
	out.PrometheusConnection = ds.PrometheusConnected
	out.PrometheusConnectionError = ds.PrometheusConnectedError
	// AlertManager: HTTP /-/healthy — the endpoint kube-prometheus-stack ships,
	// no auth needed for in-cluster AlertManager.
	if ds.AlertManagerURL != "" {
		out.AlertManagerConnection, out.AlertManagerConnectionError = httpHealth(ctx, s.HTTP, ds.AlertManagerURL+"/-/healthy")
	}
	// OpenCost: only probe if a URL is set.
	// where missing URL → opencost=False, no request.
	if ds.OpencostURL != "" {
		out.OpencostConnection, out.OpencostConnectionError = httpHealth(ctx, s.HTTP, ds.OpencostURL+"/healthz")
	}

	// Trace status / provider / URL — verbatim port of the backend.
	out.TracesEnabled = traceStatus(ds)
	out.TraceProvider = traceProvider(ds)
	out.TracesURL = traceURL(ds)
	// traceProviderConfig: the legacy code queries ClickHouse for the
	// otel_traces materialized-column flag. The agent doesn't run a
	// local ClickHouse anymore; the backend computes this. Emit an
	// empty dict so the field shape matches.
	out.TraceProviderConfig = map[string]any{"hasMaterializedColumn": false}

	return out
}

// traceStatus mirrors get_trace_status.
func traceStatus(ds Datasources) bool {
	if ds.TraceTable != "" {
		return true
	}
	if isChronosphereEnabled(ds) {
		return true
	}
	if isJaegerEnabled(ds) {
		return true
	}
	return ds.ClickHouseStatus
}

// traceProvider mirrors get_trace_provider. Note
// the default is "otel_clickhouse" even when ClickHouse isn't healthy — UI
// uses (provider, status) to decide what to render.
func traceProvider(ds Datasources) string {
	if ds.TraceTable != "" {
		return "bigquery"
	}
	if isChronosphereEnabled(ds) {
		return "chronosphere"
	}
	if isJaegerEnabled(ds) {
		return "jaeger"
	}
	return "otel_clickhouse"
}

// traceURL mirrors get_trace_url. Note the first
// argument `url_from_prometheus` is what the legacy passes as
// `clickhouse_url` — we don't run a local ClickHouse, so it's always "".
func traceURL(ds Datasources) string {
	if ds.TraceTable != "" {
		return ds.TraceTable
	}
	if isJaegerEnabled(ds) {
		return ds.JaegerQueryURL
	}
	return ""
}

// isChronosphereEnabled mirrors _is_chronosphere_enabled.
func isChronosphereEnabled(ds Datasources) bool {
	if !ds.ChronosphereTracesEnabled {
		return false
	}
	if ds.ChronosphereTracesURL != "" {
		return true
	}
	return ds.PrometheusURL != "" && strings.Contains(ds.PrometheusURL, "chronosphere.io")
}

// isJaegerEnabled mirrors _is_jaeger_enabled.
func isJaegerEnabled(ds Datasources) bool {
	return ds.JaegerEnabled && ds.JaegerQueryURL != ""
}

// httpHealth probes GET <url> with a 5s budget. It returns ok=true iff the
// response is 2xx; on failure it also returns a short one-line reason
// (transport error or "HTTP <status>: <body-snippet>") that the UI surfaces
// next to the "Disconnected" status. reason is empty when ok is true.
func httpHealth(ctx context.Context, c *http.Client, url string) (ok bool, reason string) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := c.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, ""
	}
	return false, healthErr(resp.StatusCode, body)
}

// healthErr formats a non-2xx probe response into a compact single-line reason
// suitable for the health UI: whitespace collapsed and truncated so a verbose
// JSON error body doesn't blow up the payload.
func healthErr(status int, body []byte) string {
	msg := strings.Join(strings.Fields(string(body)), " ")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	if msg == "" {
		return fmt.Sprintf("HTTP %d", status)
	}
	return fmt.Sprintf("HTTP %d: %s", status, msg)
}
