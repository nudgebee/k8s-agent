// Command nudgebee-agent is the in-cluster Kubernetes agent — a Go binary
// that runs inside a customer's K8s cluster and exposes a primitive
// surface (kube reads, observability proxies, mutations, scanners,
// discovery) over a WebSocket connection to the Nudgebee backend.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nudgebee/nudgebee-agent/internal/k8sclient"
	"github.com/nudgebee/nudgebee-agent/pkg/alerts"
	"github.com/nudgebee/nudgebee-agent/pkg/auth"
	chclient "github.com/nudgebee/nudgebee-agent/pkg/clickhouse"
	"github.com/nudgebee/nudgebee-agent/pkg/config"
	"github.com/nudgebee/nudgebee-agent/pkg/control"
	"github.com/nudgebee/nudgebee-agent/pkg/discovery"
	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
	"github.com/nudgebee/nudgebee-agent/pkg/enrichers"
	"github.com/nudgebee/nudgebee-agent/pkg/grafana"
	"github.com/nudgebee/nudgebee-agent/pkg/kube"
	"github.com/nudgebee/nudgebee-agent/pkg/metrics"
	"github.com/nudgebee/nudgebee-agent/pkg/mutate"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/chronosphere"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/elasticsearch"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/gcp"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/httpproxy"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/jaeger"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/loki"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/pinot"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/signoz"
	"github.com/nudgebee/nudgebee-agent/pkg/podexec"
	"github.com/nudgebee/nudgebee-agent/pkg/podrunner"
	"github.com/nudgebee/nudgebee-agent/pkg/podshell"
	"github.com/nudgebee/nudgebee-agent/pkg/relay"
	"github.com/nudgebee/nudgebee-agent/pkg/rightsize"
	"github.com/nudgebee/nudgebee-agent/pkg/scanners"
	"github.com/nudgebee/nudgebee-agent/pkg/servicemap"
	"github.com/nudgebee/nudgebee-agent/pkg/svcdiscover"
	"github.com/nudgebee/nudgebee-agent/pkg/tasks"
	"github.com/nudgebee/nudgebee-agent/pkg/telemetry"
	"github.com/nudgebee/nudgebee-agent/pkg/triggers"
	"github.com/nudgebee/nudgebee-agent/pkg/version"
)

// triggerAdapter bridges pkg/triggers.Engine to alerts.TriggerEngine
// so pkg/alerts doesn't have to import pkg/triggers (which would couple
// two layers that should stay independent — alerts owns wire-shape,
// triggers owns matcher logic).
type triggerAdapter struct{ e *triggers.Engine }

func (a *triggerAdapter) MatchK8sEvent(operation, kind string, obj, oldObj map[string]any) []alerts.MatchedTrigger {
	matches := a.e.Match(triggers.IncomingK8sEvent{
		Operation: operation, Kind: kind, Obj: obj, OldObj: oldObj,
	})
	out := make([]alerts.MatchedTrigger, 0, len(matches))
	for _, m := range matches {
		// Copy ExtraBlocks across the layer boundary. The two slices have
		// the same element shape (open map[string]any) but different
		// declared types, so we re-wrap rather than alias.
		extras := make([]map[string]any, 0, len(m.ExtraBlocks))
		for _, b := range m.ExtraBlocks {
			extras = append(extras, map[string]any(b))
		}
		out = append(out, alerts.MatchedTrigger{
			AggregationKey:   m.Spec.AggregationKey,
			Priority:         m.Spec.Priority,
			FindingType:      m.Spec.FindingType,
			Fingerprint:      m.Fingerprint,
			Owner:            alerts.OwnerRef{Name: m.Owner.Name, Kind: m.Owner.Kind},
			SubjectName:      m.SubjectName,
			SubjectNamespace: m.SubjectNamespace,
			SubjectKind:      m.SubjectKind,
			SubjectNode:      m.SubjectNode,
			MatcherName:      m.Spec.Name,
			ExtraBlocks:      extras,
		})
	}
	return out
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	logger.Info("nudgebee-agent starting",
		"version", version.Version,
		"commit", version.Commit,
		"build_time", version.BuildTime,
	)

	cfg, err := config.FromEnv()
	if err != nil {
		logger.Error("config error", "err", err)
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	if err := run(ctx, logger, cfg); err != nil && err != context.Canceled {
		logger.Error("agent exited with error", "err", err)
		cancel()
		os.Exit(1)
	}
	cancel()
	logger.Info("nudgebee-agent stopped")
}

func run(ctx context.Context, logger *slog.Logger, cfg *config.Config) error {
	handlers := map[string]dispatch.Handler{
		"ping":   dispatch.SimpleHandler(handlePing),
		"echo":   dispatch.SimpleHandler(handleEcho),
		"health": dispatch.SimpleHandler(handleHealth),
	}
	lightActions := map[string]struct{}{
		"ping":   {},
		"echo":   {},
		"health": {},
	}

	// Build the K8s client up-front so service-discovery can run before any
	// observability subsystem is wired. If construction fails, we fall back
	// to env-only configuration — the agent still serves whatever URLs are
	// explicitly set. Discovery is opportunistic.
	var (
		typedKube   kubernetes.Interface
		dynamicKube dynamic.Interface
		kubeRestCfg *rest.Config
	)
	if cs, restCfg, err := k8sclient.New(""); err == nil {
		typedKube = cs
		kubeRestCfg = restCfg
		if dyn, derr := dynamic.NewForConfig(restCfg); derr == nil {
			dynamicKube = dyn
		} else {
			logger.Warn("dynamic client build failed — mutate alert-rule CRUD disabled", "err", derr)
		}
	} else {
		logger.Warn("k8s client unavailable — discovery / kube / podexec / scanners / mutate disabled, autodiscovery off", "err", err)
		cfg.DiscoveryEnabled = false
		cfg.KubeEnabled = false
		cfg.PodExecEnabled = false
		cfg.ScannersEnabled = false
		cfg.MutateEnabled = false
	}

	// Service-discovery: only fills blank URLs, never overrides env. Mirrors
	// the legacy PrometheusDiscovery / AlertManagerDiscovery / GrafanaLokiDiscovery.
	// A 1h cache avoids hammering the API server.
	disc := svcdiscover.New(typedKube, "")
	if cfg.PrometheusURL == "" {
		if u := disc.FindFirst(ctx, svcdiscover.PrometheusSelectors); u != "" {
			cfg.PrometheusURL = u
			logger.Info("prometheus auto-discovered", "url", u)
		}
	}
	if cfg.LokiURL == "" {
		if u := disc.FindFirst(ctx, svcdiscover.LokiSelectors); u != "" {
			cfg.LokiURL = u
			logger.Info("loki auto-discovered", "url", u)
		}
	}
	if cfg.AlertManagerURL == "" {
		if u := disc.FindFirst(ctx, svcdiscover.AlertManagerSelectors); u != "" {
			cfg.AlertManagerURL = u
			logger.Info("alertmanager auto-discovered", "url", u)
		}
	}
	// OpenCost can be disabled at the agent (OPENCOST_ENABLED=false) — cost is then
	// computed centrally on the server side. When disabled, skip BOTH the
	// OPENCOST_ENDPOINT env and the cluster-wide Service autodiscovery: discovery
	// matches `app=opencost` across all namespaces, so otherwise the agent latches
	// onto a neighbouring namespace's OpenCost and keeps reporting itself
	// cost-enabled, which suppresses the server-side takeover. Defaults to enabled.
	opencostEnabled := true
	if v := os.Getenv("OPENCOST_ENABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			opencostEnabled = b
		} else {
			logger.Warn("invalid OPENCOST_ENABLED, defaulting to enabled", "value", v, "err", err)
		}
	}
	opencostURL := ""
	if opencostEnabled {
		// Mirrors OpenCostDiscovery.find_open_cost_url.
		// OPENCOST_ENDPOINT env wins; falls back to in-cluster Service lookup.
		opencostURL = os.Getenv("OPENCOST_ENDPOINT")
		if opencostURL == "" {
			if u := disc.FindFirst(ctx, svcdiscover.OpencostSelectors); u != "" {
				opencostURL = u
				logger.Info("opencost auto-discovered", "url", u)
			}
		}
	} else {
		logger.Info("opencost disabled (OPENCOST_ENABLED=false); skipping discovery and cost polling")
	}

	var promClient *prometheus.Client
	if cfg.PrometheusURL != "" {
		promClient = prometheus.New(cfg.PrometheusURL, nil)
		promClient.ExtraHeaders = config.ParseHeaders(cfg.PrometheusHeaders)
		ph := prometheus.Handlers(promClient)
		maps.Copy(handlers, ph)
		for name := range ph {
			lightActions[name] = struct{}{}
		}
		logger.Info("prometheus enabled", "url", cfg.PrometheusURL, "actions", len(ph))

		// Enrichers: prometheus_enricher / prometheus_queries_enricher
		// produce Finding-shaped responses; api-server's relay.ExecuteAndExtractResponse
		// walks findings[].evidence[].data.
		promEnr := enrichers.NewPrometheusEnricher(promClient, cfg.AccountID)
		handlers["prometheus_enricher"] = func(ctx context.Context, p map[string]any) (any, error) {
			return promEnr.HandleEnricher(ctx, p)
		}
		handlers["prometheus_queries_enricher"] = func(ctx context.Context, p map[string]any) (any, error) {
			return promEnr.HandleQueriesEnricher(ctx, p)
		}
		// `prometheus_labels` returns label *values* for one
		// label_name in a Finding-wrapped JsonBlock. Overrides the raw passthrough
		// merged in via Handlers(c) above (now exposed under `prometheus_label_names`
		// since /api/v1/labels lists names, not values). All production callers —
		// api-server services/observability/{prometheus,chronosphere}.go and
		// llm/rag-server — send {label_name} and unwrap result_type/data/error.
		handlers["prometheus_labels"] = func(ctx context.Context, p map[string]any) (any, error) {
			return promEnr.HandleLabels(ctx, p)
		}
		lightActions["prometheus_enricher"] = struct{}{}
		lightActions["prometheus_queries_enricher"] = struct{}{}
		lightActions["prometheus_labels"] = struct{}{}

		// application_stats: thin {success,data} shape. Same Prometheus client.
		appStats := enrichers.NewAppStatsEnricher(promClient)
		handlers["application_stats"] = appStats.Handler()
		lightActions["application_stats"] = struct{}{}

		// slo_generator: thin {success,data} shape. Same client.
		sloGen := enrichers.NewSLOEnricher(promClient)
		handlers["slo_generator"] = sloGen.Handler()
		lightActions["slo_generator"] = struct{}{}

		logger.Info("prometheus compat enrichers enabled", "actions", 4)
	}

	// Service map needs the same Prometheus client (queries Coroot eBPF
	// metrics in-cluster). Gated on PROMETHEUS_URL being set.
	if promClient != nil {
		smSvc := servicemap.New(promClient, cfg.ClusterName)
		smHandlers := servicemap.Handlers(smSvc, cfg.AccountID)
		maps.Copy(handlers, smHandlers)
		for name := range smHandlers {
			lightActions[name] = struct{}{}
		}
		logger.Info("service_map enabled", "actions", len(smHandlers))
	}

	if cfg.LokiURL != "" {
		lc := loki.New(cfg.LokiURL, nil)
		lc.ExtraHeaders = config.ParseHeaders(cfg.LokiHeaders)
		lh := loki.Handlers(lc)
		maps.Copy(handlers, lh)
		for name := range lh {
			lightActions[name] = struct{}{}
		}

		// Compat handlers for the same Loki backend. Action names
		// are the ones api-server / playbooks dispatch today; the parameter
		// shape is `query` = raw URL query string.
		compat := enrichers.NewLokiCompat(lc)
		ch := compat.Handlers()
		maps.Copy(handlers, ch)
		for name := range ch {
			lightActions[name] = struct{}{}
		}
		logger.Info("loki enabled", "url", cfg.LokiURL, "actions", len(lh)+len(ch))
	}

	if cfg.ElasticsearchURL != "" {
		ec := elasticsearch.New(cfg.ElasticsearchURL, nil)
		ec.Username = cfg.ElasticsearchUser
		ec.Password = cfg.ElasticsearchPassword
		ec.APIKey = cfg.ElasticsearchAPIKey
		eh := elasticsearch.Handlers(ec)
		maps.Copy(handlers, eh)
		for name := range eh {
			lightActions[name] = struct{}{}
		}
		logger.Info("elasticsearch enabled", "url", cfg.ElasticsearchURL, "actions", len(eh))
	}

	if cfg.SignozURL != "" {
		sc := signoz.New(cfg.SignozURL, nil)
		sc.APIKey = cfg.SignozAPIKey
		sh := signoz.Handlers(sc)
		maps.Copy(handlers, sh)
		for name := range sh {
			lightActions[name] = struct{}{}
		}
		logger.Info("signoz enabled", "url", cfg.SignozURL, "actions", len(sh))
	}

	if cfg.JaegerURL != "" {
		jc := jaeger.New(cfg.JaegerURL, nil)
		jh := jaeger.Handlers(jc)
		maps.Copy(handlers, jh)
		for name := range jh {
			lightActions[name] = struct{}{}
		}
		logger.Info("jaeger enabled", "url", cfg.JaegerURL, "actions", len(jh))
	}

	if cfg.ChronosphereURL != "" {
		cc := chronosphere.New(cfg.ChronosphereURL, nil)
		cc.APIKey = cfg.ChronosphereAPIKey
		ch := chronosphere.Handlers(cc)
		maps.Copy(handlers, ch)
		for name := range ch {
			lightActions[name] = struct{}{}
		}
		logger.Info("chronosphere enabled", "url", cfg.ChronosphereURL, "actions", len(ch))
	}

	if cfg.PinotURL != "" {
		pc := pinot.New(cfg.PinotURL, nil)
		pc.AuthToken = cfg.PinotAuthToken
		pc.Username = cfg.PinotUsername
		pc.Password = cfg.PinotPassword
		ph := pinot.Handlers(pc)
		maps.Copy(handlers, ph)
		for name := range ph {
			lightActions[name] = struct{}{}
		}
		logger.Info("pinot enabled", "url", cfg.PinotURL, "actions", len(ph))
	}

	if targets := config.ParseTargets(cfg.HTTPProxyTargets); len(targets) > 0 {
		hp := httpproxy.New(targets, nil)
		ph := httpproxy.Handlers(hp)
		maps.Copy(handlers, ph)
		for name := range ph {
			lightActions[name] = struct{}{}
		}
		logger.Info("http proxy enabled", "targets", len(targets), "actions", len(ph))
	}

	if cfg.GCPEnabled {
		gcpClient, err := gcp.New(ctx)
		if err != nil {
			// Don't fail the whole agent — GCP creds might be missing on this
			// cluster. Log and continue; gke_logs / gke_traces just won't be
			// registered.
			logger.Warn("gcp disabled: ADC unavailable", "err", err)
		} else {
			gh := gcp.Handlers(gcpClient, cfg.GCPProjectID)
			maps.Copy(handlers, gh)
			for name := range gh {
				lightActions[name] = struct{}{}
			}
			logger.Info("gcp enabled", "default_project", cfg.GCPProjectID, "actions", len(gh))
		}
	}

	// logs_enricher (Finding-shape) reads pod logs through
	// the typed client. Light-action — log read is non-mutating; no signature
	// required for log reads.
	if typedKube != nil {
		logsEnr := enrichers.NewLogsEnricher(typedKube, cfg.AccountID)
		handlers["logs_enricher"] = logsEnr.Handle
		lightActions["logs_enricher"] = struct{}{}
		logger.Info("logs_enricher enabled")
	}

	// query_data: ClickHouse SQL through the legacy wire shape ({success,data}).
	// Registered unconditionally — `query_data` returns an empty
	// QueryResult when CLICKHOUSE_ENABLED=False, it doesn't fail-auth.
	// With clickhouse.enabled=false in the chart the host env is empty;
	// we hand a nil client to QueryData and the handler returns the same
	// empty-result shape. api-server callers see an empty response instead
	// of an "auth rejected" warning.
	var ch *chclient.Client
	if cfg.ClickHouseEnabled && cfg.ClickHouseHost != "" {
		ch = chclient.New(chclient.Config{
			Host:       cfg.ClickHouseHost,
			Port:       cfg.ClickHousePort,
			User:       cfg.ClickHouseUser,
			Password:   cfg.ClickHousePassword,
			Database:   cfg.ClickHouseDB,
			SSLEnabled: cfg.ClickHouseSSL,
		})
		logger.Info("query_data enabled", "host", cfg.ClickHouseHost, "port", cfg.ClickHousePort, "db", cfg.ClickHouseDB)
	} else {
		logger.Info("query_data registered without ClickHouse — returns empty result")
	}
	handlers["query_data"] = enrichers.QueryData(ch)
	lightActions["query_data"] = struct{}{}

	// api_traces_enricher_v2: OTel traces query against the same ClickHouse.
	// Same registration semantics as query_data — register unconditionally so
	// callers see an empty result instead of "auth rejected" when CH is off.
	tracesEnr := enrichers.NewAPITracesEnricher(ch, cfg.AccountID)
	handlers["api_traces_enricher_v2"] = tracesEnr.Handler()
	lightActions["api_traces_enricher_v2"] = struct{}{}

	if cfg.KubeEnabled {
		kc := kube.NewClient(dynamicKube, typedKube)
		kx := &kube.KubectlExecutor{} // uses kubectl on PATH
		kh := kube.Handlers(kc, kx, cfg.AccountID)
		maps.Copy(handlers, kh)
		for name := range kh {
			// get_resource / get_resource_yaml / list_resource_names are
			// read-only and OK as light-action. kubectl_command_executor is
			// also restricted to read verbs by pkg/kube/exec.go.
			lightActions[name] = struct{}{}
		}
		logger.Info("kube primitives enabled", "actions", len(kh))
	}

	if cfg.ScannersEnabled {
		runner := scanners.NewRunner(typedKube, cfg.ScannerNamespace, cfg.ScannerServiceAccount)
		sh := scanners.Handlers(runner)
		maps.Copy(handlers, sh)
		// schedule_k8s_job / wait_for_k8s_job / get_k8s_job_logs are light-actions.
		// Trust chain: api-server holds RELAY_SERVER_SECRET_KEY → relay gates inbound
		// requests on it → relay forwards to the agent's outbound WS. Adding HMAC at
		// the agent doesn't strengthen that — anyone reaching the relay's /request
		// already has the same secret api-server uses. Protection is the agent's
		// hygiene clamps (namespace, BackoffLimit=0, concurrency cap=5, TTL=600s,
		// 5 MiB log cap), which apply regardless of caller. Same posture as
		// kubectl_command_executor (also a light-action under this trust model).
		for name := range sh {
			lightActions[name] = struct{}{}
		}
		logger.Info("scanners enabled", "namespace", cfg.ScannerNamespace, "actions", len(sh))
	}

	if cfg.PodExecEnabled {
		execer := podexec.New(typedKube, kubeRestCfg)
		// pod_profiler reuses the same gating: it spawns a privileged
		// debugger pod + runs SPDY exec, both of which need the same
		// signing posture as pod_bash_enricher. ProfilerHandler is
		// constructed only when restCfg is available (file-copy needs
		// SPDY); HandlersWithProfiler omits the action otherwise.
		var profiler *podexec.ProfilerHandler
		if kubeRestCfg != nil {
			profiler = podexec.NewProfilerHandler(typedKube, kubeRestCfg)
		}
		ph := podexec.HandlersWithProfiler(execer, profiler)
		maps.Copy(handlers, ph)
		// pod_bash_enricher / pod_profiler run arbitrary commands or spawn
		// privileged pods. NOT light-action — require HMAC sig or RSA
		// partial-keys.
		logger.Info("pod-exec enabled", "actions", len(ph))

		// pod_script_run_enricher is gated on the same PodExecEnabled flag
		// but lives in pkg/podrunner (it spawns a fresh pod from an
		// image + sources env vars from a k8s Secret + returns logs as a
		// JsonBlock-wrapped Finding — the wire shape api-server's
		// relay.CommandExecutor, runbook-server, and llm-server all expect).
		// Service account: uses ScannerServiceAccount when set so we don't
		// add a new env knob just for this; falls back to "" (cluster default).
		runner := podrunner.New(typedKube, cfg.ScannerNamespace, cfg.ScannerServiceAccount, cfg.AccountID)
		rh := podrunner.Handlers(runner)
		maps.Copy(handlers, rh)
		// By trust posture pod_script_run_enricher belongs with pod_bash_enricher
		// (arbitrary command execution → HMAC/RSA only). But api-server's
		// relay.CommandExecutor dispatches it UNSIGNED behind the relay's
		// shared-secret gate — the k8s-mode DB integration connection tests
		// (postgresql/mysql/clickhouse/mssql/oracle), runbook-server, and
		// llm-server all reach it this way and none of them sign. Same situation
		// as the PrometheusRule actions carved in below. Without this the
		// dispatcher rejects every such request with "not in light-action
		// allowlist", surfacing in the UI as a misleading "failed to connect to
		// the cluster". Allowlist it here to close the gap without forcing
		// signing into api-server's relay client. pod_bash_enricher / pod_profiler
		// stay OUT — they have no unsigned api-server caller.
		// TODO(security): this trades per-request HMAC/RSA verification for the
		// relay's shared-secret gate, so a relay/secret compromise yields
		// arbitrary in-cluster command execution. Remove this entry once
		// api-server's relay client signs pod_script_run_enricher.
		lightActions["pod_script_run_enricher"] = struct{}{}
		logger.Info("pod-runner enabled",
			"default_namespace", cfg.ScannerNamespace,
			"service_account", cfg.ScannerServiceAccount,
			"actions", len(rh))
	}

	if cfg.MutateEnabled {
		amHeaders := map[string]string{}
		// AlertManagerHeaders use the same comma-separated env format as Loki/Prom.
		for k, v := range config.ParseHeaders(os.Getenv("ALERTMANAGER_HEADERS")) {
			if len(v) > 0 {
				amHeaders[k] = v[0]
			}
		}
		mut := mutate.New(typedKube, cfg.AlertManagerURL, amHeaders)
		mut.SetDynamic(dynamicKube) // unlocks PrometheusRule CRUD actions
		// INSTALLATION_NAMESPACE is set by the chart via the downward API.
		// Required by the legacy alert-rule path; falls back to the scanner
		// namespace (also chart-set) so a hand-rolled deployment without
		// INSTALLATION_NAMESPACE still resolves to the right namespace.
		installNs := os.Getenv("INSTALLATION_NAMESPACE")
		if installNs == "" {
			installNs = cfg.ScannerNamespace
		}
		if installNs == "" {
			logger.Warn("install namespace is empty (neither INSTALLATION_NAMESPACE nor SCANNER_NAMESPACE set) — legacy alert-rule actions will error at request time")
		}
		mut.SetNamespace(installNs)
		if cfg.LokiRulesURL != "" {
			lokiRulesHeaders := map[string]string{}
			for k, v := range config.ParseHeaders(os.Getenv("LOKI_RULES_HEADERS")) {
				if len(v) > 0 {
					lokiRulesHeaders[k] = v[0]
				}
			}
			mut.SetLokiRules(cfg.LokiRulesURL, lokiRulesHeaders)
		}
		mh := mutate.Handlers(mut)
		maps.Copy(handlers, mh)
		// Most Group-D mutations REQUIRE RSA partial-keys in production and
		// stay out of lightActions. The two PrometheusRule legacy actions are
		// the exception: api-server → relay → agent today sends them unsigned
		// behind the relay's shared-secret gate, same posture as the read
		// primitives. Carving them in here closes the gap without forcing
		// signing into api-server's relay client.
		lightActions["create_or_replace_alert_rule"] = struct{}{}
		lightActions["delete_alert_rule"] = struct{}{}
		logger.Info("mutate enabled",
			"alertmanager_url", cfg.AlertManagerURL,
			"loki_rules_url", cfg.LokiRulesURL,
			"install_namespace", installNs,
			"actions", len(mh))
	}

	// continuous_rightsizing: samples Prometheus usage and patches workload
	// resource requests/limits (recommend_only skips the patch). Needs both
	// Prometheus (usage) and the dynamic client (read + Update across
	// Deployment/StatefulSet/DaemonSet/ReplicaSet/Rollout). Driven by
	// runbook-server's optimizer over the relay's shared-secret /request path —
	// unsigned, same trust posture as the alert-rule mutations carved in above —
	// so it goes in lightActions. Skipped when either client is unavailable
	// rather than registered as a fail-auth stub.
	if promClient != nil && dynamicKube != nil {
		// typedKube (may be nil) enables zero-downtime in-place pod resize; the
		// rightsizer falls back to the template rollout when it's unavailable or
		// the cluster is < 1.33.
		rs := rightsize.New(promClient, dynamicKube, typedKube)
		maps.Copy(handlers, rightsize.Handlers(rs))
		lightActions["continuous_rightsizing"] = struct{}{}
		logger.Info("continuous_rightsizing enabled")
	} else {
		logger.Warn("continuous_rightsizing disabled — needs both Prometheus and a dynamic K8s client",
			"prometheus", promClient != nil, "dynamic_client", dynamicKube != nil)
	}

	// rightsizing_resource: applies a backend-computed recommendation (explicit
	// per-container cpu/mem request+limit + correlation annotations) to a
	// workload. Needs only the dynamic client (read + Update across
	// Deployment/StatefulSet/DaemonSet/Rollout/Pod). Delivered via the
	// agent_task poller (pkg/tasks) — a trusted path — so it is a plain handler
	// and is NOT a lightAction (an unsigned WS delivery of a mutation is
	// correctly rejected). Skipped when the dynamic client is unavailable.
	if dynamicKube != nil {
		handlers["rightsizing_resource"] = rightsize.NewApplier(dynamicKube).Handle
		logger.Info("rightsizing_resource enabled")
	} else {
		logger.Warn("rightsizing_resource disabled — needs a dynamic K8s client")
	}

	// Read primitives are light-action (no signature). Mutations and pod-exec
	// actions are NOT in lightActions, so the dispatcher rejects them unless
	// the request carries a valid HMAC signature OR RSA partial-keys.
	validator := &auth.Validator{
		SigningKey:   cfg.AuthSecretKey,
		LightActions: lightActions,
	}
	if cfg.RSAPrivateKeyPath != "" {
		priv, err := auth.LoadPrivateKey(cfg.RSAPrivateKeyPath)
		if err != nil {
			return fmt.Errorf("load RSA private key: %w", err)
		}
		validator.PrivateKey = priv
		logger.Info("RSA partial-keys auth enabled", "key_path", cfg.RSAPrivateKeyPath)
	}

	// refresh_playbook hot-reloads the allowlist from the backend so we can
	// add new actions to a running agent without a customer Helm upgrade.
	// Pre-load the validator's atomic allowlist with the startup-built set so
	// concurrent refresh + Validate calls share one source of truth.
	staticActions := []string{"ping", "echo", "health", "refresh_playbook"}
	refresher := control.New(cfg.BackendEndpoint, cfg.AuthSecretKey, cfg.AccountID, cfg.ClusterName, validator)
	refresher.StaticActions = staticActions
	maps.Copy(handlers, control.Handlers(refresher))
	lightActions["refresh_playbook"] = struct{}{}
	validator.SetLightActions(lightActions)

	mreg := metrics.New()
	disp := dispatch.New(dispatch.Config{Logger: logger}, validator, handlers)
	disp.SetMetrics(mreg)

	// Pod-shell session manager (TerminalRequest WS shape, output_type=Terminal).
	// The relay's interactive-shell handler unmarshals AgentResponse.Data as
	// a string — without this wired up, every shell session 500s on the relay
	// (handlers/ws.go:96). Disabled when the K8s client failed to build.
	// The cleanup goroutine is launched below alongside other long-running
	// loops (after errgroup is built).
	var shellMgr *podshell.Manager
	if typedKube != nil && kubeRestCfg != nil {
		shellMgr = podshell.NewManager(typedKube, kubeRestCfg)
		disp.SetTerminal(&shellTerminalAdapter{m: shellMgr})
		logger.Info("pod_shell enabled")
	}

	// Grafana / API proxy (GrafanaRequest WS shape, output_type=Grafana).
	// Same wire-shape concern as terminal: data must be JSON-stringified.
	grafanaURL := os.Getenv("GRAFANA_URL")
	{
		grafanaUser := os.Getenv("GRAFANA_USERNAME")
		grafanaPass := os.Getenv("GRAFANA_PASSWORD")
		var extraHeaders []string
		if h := os.Getenv("GRAFANA_EXTRA_HEADER"); h != "" {
			extraHeaders = strings.Split(h, ";")
		}
		// Even without GRAFANA_URL, the agent still needs an APIProxy adapter
		// so requests with X-API-Base-URL go through.
		// PROMETHEUS_URL + PROMETHEUS_HEADERS wire the X-NB-Request-Type=
		// Prometheus path the collector's `/prometheus-v2/*` route forwards
		// through (relay-server/pkg/utils/utils.go:77).
		gp := grafana.New(grafanaURL, grafanaUser, grafanaPass, extraHeaders,
			cfg.PrometheusURL, config.ParseHeaders(cfg.PrometheusHeaders), nil)
		disp.SetGrafana(&grafanaAdapter{p: gp})
		if grafanaURL != "" {
			logger.Info("grafana proxy enabled", "url", grafanaURL)
		} else {
			logger.Info("api proxy enabled (grafana unset; APIProxy still routed via X-API-Base-URL)")
		}
	}

	agentVersion := version.CurrentVersion()
	client := relay.NewClient(relay.Config{
		URL:           cfg.RelayURL,
		AuthSecretKey: cfg.AuthSecretKey,
		Greeting: relay.Greeting{
			Action:         "auth",
			Version:        agentVersion,
			AgentVersion:   agentVersion,
			AgentCommit:    version.Commit,
			AgentBuildTime: version.BuildTime,
		},
		Logger:          logger,
		HandlerPoolSize: cfg.RelayHandlerPoolSize,
		OnShed:          func() { mreg.ForwardShed.WithLabelValues("relay").Inc() },
	}, disp.Handle)

	logger.Info("starting relay client",
		"url", cfg.RelayURL,
		"account_id", cfg.AccountID,
		"cluster", cfg.ClusterName,
	)

	g, gctx := errgroup.WithContext(ctx)

	// Pod-shell idle-session reaper. Closes sessions that haven't been touched
	// in IdleTimeout.
	if shellMgr != nil {
		g.Go(func() error {
			shellMgr.Run(gctx)
			return nil
		})
	}

	// Relay WS client.
	g.Go(func() error {
		if err := client.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("relay: %w", err)
		}
		return nil
	})

	// HTTP server: AlertManager + kubewatch intake + /healthz. Always on so
	// K8s probes work even if the backend forward URL is empty. The forwarder
	// posts to the existing collector `/v1/k8s/events` endpoint with a top-
	// level `event_type` discriminator (`raw_alert` / `raw_k8s_event`); the
	// consumer at the backend routes those into the default-Finding
	// builder and falls through to the existing storage path.
	if cfg.HTTPListenAddr != "" {
		var fwdURL string
		if cfg.BackendEndpoint != "" {
			fwdURL = cfg.BackendEndpoint + "/v1/k8s/events"
		}
		fwd := alerts.NewForwarder(fwdURL, cfg.AuthSecretKey, cfg.AccountID, cfg.ClusterName, logger)
		fwd.SetForwardPoolSize(cfg.ForwardPoolSize)
		fwd.OnShed = func(source string) { mreg.ForwardShed.WithLabelValues(source).Inc() }
		// Wire the trigger engine. Without this, every kubewatch event is
		// dropped (safe default — see plan stage 2.1). With it, only
		// events matching a registered predicate produce a Finding.
		// When a typed K8s client is available, also pass an events-lister
		// so the engine appends a "Recent <Kind> events" table to every
		// matched Finding (kubelet BackOff / Killing / OOMKilling /
		// FailedScheduling / image-pull errors etc.) for free.
		eng := triggers.NewEngine(triggers.Builtins(), time.Now())
		if typedKube != nil {
			eng = eng.WithEventsLister(newK8sEventsLister(typedKube))
		}
		fwd.Engine = &triggerAdapter{e: eng}
		logger.Info("trigger engine enabled", "matcher_count", len(triggers.Builtins()))
		mux := http.NewServeMux()
		mux.Handle("/", fwd.Mux())
		mux.Handle("/metrics", mreg.Handler())
		// pprof: the runner exhibits transient heap bursts (live heap stays
		// small, heap_sys spikes to >1.5GB then releases). Expose the
		// standard profiles so `go tool pprof` can attribute allocations —
		// /debug/pprof/allocs is cumulative and survives the burst, which a
		// point-in-time heap snapshot does not. Gated off by default: the
		// endpoints are unauthenticated and abusable for DoS/info-disclosure,
		// so enable via PPROF_ENABLED only for active debugging.
		if cfg.PprofEnabled {
			logger.Info("pprof endpoints enabled", "path", "/debug/pprof/")
			mux.HandleFunc("/debug/pprof/", pprof.Index)
			mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
			mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
			mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
			mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		}
		mux.HandleFunc("/api/actions", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // cap request body at 1 MiB
			var body relay.ActionRequestBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
				return
			}
			msg, _ := json.Marshal(relay.ExternalActionRequest{Body: body, RequestID: "http-direct"})
			var resp *relay.Response
			disp.Handle(r.Context(), msg, func(res *relay.Response) error { resp = res; return nil })
			if resp == nil {
				http.Error(w, "no response", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			_ = json.NewEncoder(w).Encode(resp)
		})
		srv := &http.Server{
			Addr:              cfg.HTTPListenAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		g.Go(func() error {
			logger.Info("http server listening", "addr", cfg.HTTPListenAddr, "event_forward_url", fwdURL)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("http: %w", err)
			}
			return nil
		})
		g.Go(func() error {
			<-gctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		})
	}

	// Telemetry: periodic ClusterStatus → /v1/k8s/telemetry. The collector
	// stores activity_stats as the agent's `connection_status`, which the UI
	// reads to show "Prometheus / Loki / AlertManager / OpenCost / Traces /
	// NodeAgent connected".
	//
	// We mirror the legacy IntegrationHealthChecker probe set —
	// Prometheus, AlertManager, OpenCost get HTTP probes inside the
	// telemetry package; node-agent count is a Prometheus query we run
	// here so we don't wedge a Prom client into the telemetry package.
	if cfg.BackendEndpoint != "" {
		// Trace-related env vars. We snapshot them once at boot — the
		// operator restarts the pod to change JAEGER_ENABLED etc.
		jaegerEnabled := os.Getenv("JAEGER_ENABLED") == "true"
		jaegerQueryURL := os.Getenv("JAEGER_QUERY_URL")
		if jaegerQueryURL == "" {
			jaegerQueryURL = cfg.JaegerURL // fall back to JAEGER_URL the agent uses for the jaeger handlers
		}
		chronosphereEnabled := os.Getenv("CHRONOSPHERE_TRACES_ENABLED") == "true"
		chronosphereURL := os.Getenv("CHRONOSPHERE_TRACES_URL")
		traceTable := os.Getenv("TRACE_TABLE")
		// ClickHouse status for the otel_clickhouse trace provider. Mirrors
		// the legacy _check_clickhouse → db.health() probe: the Helm chart
		// only sets CLICKHOUSE_HOST when clickhouse/otel-collector is wired,
		// so an empty host means traces are off (skip the probe entirely
		// — there's nothing to reach). When set, do an HTTP /ping against
		// CLICKHOUSE_PORT (chart default 8123, the HTTP port) and reflect
		// the probe result. TRACES_ENABLED is honored as an explicit
		// override only if set ("true"/"false") — otherwise probe wins.
		clickhouseHost := os.Getenv("CLICKHOUSE_HOST")
		clickhousePort := os.Getenv("CLICKHOUSE_PORT")
		if clickhousePort == "" {
			clickhousePort = "8123"
		}
		agentURL := os.Getenv("AGENT_HTTP_URL")
		// Static heartbeat inputs decoded once at startup. PROMETHEUS_ADDITIONAL_LABELS
		// is the legacy wire format: comma-separated `key=value` pairs the
		// backend stamps onto every PromQL the cluster runs (multi-cluster Prom).
		promExtraLabels := parseLabelMap(os.Getenv("PROMETHEUS_ADDITIONAL_LABELS"))
		grafanaURL := os.Getenv("GRAFANA_URL")

		// One-shot cluster-provider detection. Provider type, region, zone,
		// and account/project/resource-group are stable per cluster lifetime;
		// cached for the agent's process lifetime and re-emitted on every tick.
		// Drives the backend's cloud_account_attrs auto-populate.
		var providerInfo telemetry.ProviderInfo
		var k8sServerVersion string
		if typedKube != nil {
			detectCtx, detectCancel := context.WithTimeout(gctx, 10*time.Second)
			providerInfo = telemetry.DetectProvider(detectCtx, typedKube, logger)
			detectCancel()
			logger.Info("cluster provider detected",
				"provider", providerInfo.Provider,
				"region", providerInfo.Region,
				"zone", providerInfo.Zone,
				"account_number_present", providerInfo.AccountNumber != "")

			// K8s server version → telemetry_handler's `stats.k8s_version`.
			// The UI's Agent Health card surfaces this as the "K8s
			// (Provider/Version)" cell; stays "GKE /" without it. Stable
			// per cluster lifetime; cached on the Service struct and
			// emitted on every tick.
			if serverVersion, err := typedKube.Discovery().ServerVersion(); err != nil {
				logger.Warn("k8s server version fetch failed", "err", err)
			} else {
				k8sServerVersion = serverVersion.GitVersion
				logger.Info("k8s server version detected", "version", k8sServerVersion)
			}
		}

		ts := &telemetry.Service{
			Endpoint:     cfg.BackendEndpoint,
			AuthSecret:   cfg.AuthSecretKey,
			AccountID:    cfg.AccountID,
			ClusterName:  cfg.ClusterName,
			AgentVersion: agentVersion,
			Namespace:    os.Getenv("INSTALLATION_NAMESPACE"),
			Period:       parseTelemetryPeriod(),
			Logger:       logger,
			Provider:     providerInfo,
			K8sVersion:   k8sServerVersion,
			Datasources: func() telemetry.Datasources {
				// Per-tick probes so the UI flips back when a datasource
				// goes down. 5s budget each (httpProbe), bounded by the
				// surrounding postOnce timeout.
				probeCtx, probeCancel := context.WithTimeout(gctx, 30*time.Second)
				defer probeCancel()
				probeClient := &http.Client{Timeout: 5 * time.Second}
				logsProvider, logsURL, logsOK, logCfg := probeLogsProvider(probeCtx, cfg)
				as := telemetry.DetectAutoScaler(probeCtx, typedKube, providerInfo.Provider, logger)
				clickhouseStatus := probeClickhouse(probeCtx, probeClient, clickhouseHost, clickhousePort)
				return telemetry.Datasources{
					PrometheusURL:              cfg.PrometheusURL,
					AlertManagerURL:            cfg.AlertManagerURL,
					LokiURL:                    cfg.LokiURL,
					OpencostURL:                opencostURL,
					LogsProvider:               logsProvider,
					LogsProviderURL:            logsURL,
					LogsProviderStatus:         logsOK,
					LogProviderConfig:          logCfg,
					NodeAgentCount:             queryNodeAgentCount(probeCtx, promClient, logger),
					PrometheusRetentionTime:    telemetry.PrometheusRetention(probeCtx, promClient, logger),
					PrometheusAdditionalLabels: promExtraLabels,
					TraceTable:                 traceTable,
					JaegerEnabled:              jaegerEnabled,
					JaegerQueryURL:             jaegerQueryURL,
					ChronosphereTracesEnabled:  chronosphereEnabled,
					ChronosphereTracesURL:      chronosphereURL,
					ClickHouseStatus:           clickhouseStatus,
					AgentURL:                   agentURL,
					GrafanaEnabled:             grafanaURL != "" && httpProbe(probeCtx, probeClient, grafanaURL+"/api/health"),
					AutoScalerEnabled:          as.Enabled,
					AutoScalerType:             as.Type,
					AutoScalerVersion:          as.Version,
					AutoScalerNamespace:        as.Namespace,
				}
			},
			LightActions: func() []string {
				set := validator.LightActionsSet()
				out := make([]string, 0, len(set))
				for k := range set {
					out = append(out, k)
				}
				return out
			},
		}
		g.Go(func() error {
			logger.Info("starting telemetry poster", "endpoint", cfg.BackendEndpoint, "period", ts.Period)
			return ts.Run(gctx)
		})
	}

	// Task poller: drains agent_task queue (recommendation jobs — krr_scan,
	// popeye_scan, image_scanner, certificate_scanner, trivy_cis_scan,
	// kube_bench_scan, k8s_version_upgrade, helm_chart_upgrade, …).
	// api-server inserts rows into agent_task with status=TODO; we GET
	// /v1/k8s/tasks, dispatch through the trusted handler path, and POST the
	// result back. Without this loop no recommendations reach the UI for
	// the tenant. Period defaults to 120s (matches TASK_RUNNER_WINDOW).
	if cfg.BackendEndpoint != "" {
		ts := &tasks.Service{
			Endpoint:   cfg.BackendEndpoint,
			AuthSecret: cfg.AuthSecretKey,
			Period:     tasks.ParseTaskWindow(os.Getenv("TASK_RUNNER_WINDOW")),
			Logger:     logger,
			Dispatch:   disp,
		}
		g.Go(func() error {
			logger.Info("starting task poller", "endpoint", cfg.BackendEndpoint, "period", ts.Period)
			return ts.Run(gctx)
		})
	}

	// Discovery: K8s informer-driven resource sync. Reuses typedKube built above.
	if cfg.DiscoveryEnabled {
		discoverySink := discovery.NewSink(cfg.BackendEndpoint, cfg.AuthSecretKey, cfg.AccountID, cfg.ClusterName, logger)
		discSvc := discovery.NewService(typedKube, discoverySink, cfg.DiscoveryResync, logger)
		discSvc.SetOptions(discovery.Options{
			SnapshotBatching:  cfg.DiscoverySnapshotBatching,
			BatchSize:         cfg.DiscoveryBatchSize,
			IncrementalBatch:  cfg.IncrementalBatchSize,
			IncrementalWindow: cfg.IncrementalBatchWindow,
			EmitTombstones:    cfg.EmitTombstones,
		})
		discSvc.RegisterAll() // Pod, Deployment, StatefulSet, DaemonSet, Node, Namespace
		// TODO(phase-4): ReplicaSet, Job, CronJob, Helm releases — each requires
		// a converter + shadow-diff before promotion.
		g.Go(func() error {
			logger.Info("starting discovery", "resync", cfg.DiscoveryResync)
			if err := discSvc.Run(gctx); err != nil && !errors.Is(err, context.Canceled) {
				return fmt.Errorf("discovery: %w", err)
			}
			return nil
		})

		// Alert-rules push: feeds the collector's `event_rules` table so the
		// UI's eventrules + runbook-fire paths know about every alert in the
		// cluster. Skipped when neither a Prom client nor a dynamic K8s client
		// is available — both sources would be empty.
		if promClient != nil || dynamicKube != nil {
			var kubeForRules *kube.Client
			if dynamicKube != nil {
				kubeForRules = kube.NewClient(dynamicKube, typedKube)
			}
			ar := &discovery.AlertRulesCollector{
				Prom:   promClient,
				Kube:   kubeForRules,
				Sink:   discoverySink,
				Logger: logger,
			}
			g.Go(func() error {
				logger.Info("starting alert_rules collector", "interval", cfg.AlertRulesInterval)
				return ar.Run(gctx, cfg.AlertRulesInterval)
			})
		}
	}

	return g.Wait()
}

// shellTerminalAdapter bridges pkg/dispatch's TerminalHandler interface to
// the pkg/podshell.Manager's typed Handle method.
type shellTerminalAdapter struct{ m *podshell.Manager }

func (a *shellTerminalAdapter) Handle(ctx context.Context, r *dispatch.TerminalRequest) (any, int) {
	return a.m.Handle(ctx, &podshell.Request{
		Action:    r.Action,
		SessionID: r.SessionID,
		Name:      r.Name,
		Namespace: r.Namespace,
		Command:   r.Command,
		RequestID: r.RequestID,
	})
}

// grafanaAdapter bridges pkg/dispatch's GrafanaHandler interface to
// pkg/grafana.Proxy.
type grafanaAdapter struct{ p *grafana.Proxy }

func (a *grafanaAdapter) HandleGrafana(ctx context.Context, r *dispatch.GrafanaRequest) any {
	return a.p.HandleGrafana(ctx, &grafana.Request{
		Method:        r.Method,
		URL:           r.URL,
		ContentLength: r.ContentLength,
		Body:          r.Body,
		Header:        r.Header,
	})
}

func (a *grafanaAdapter) HandleAPI(ctx context.Context, baseURL string, r *dispatch.GrafanaRequest) any {
	return a.p.HandleAPI(ctx, baseURL, &grafana.Request{
		Method:        r.Method,
		URL:           r.URL,
		ContentLength: r.ContentLength,
		Body:          r.Body,
		Header:        r.Header,
	})
}

func (a *grafanaAdapter) HandlePrometheus(ctx context.Context, r *dispatch.GrafanaRequest) any {
	return a.p.HandlePrometheus(ctx, &grafana.Request{
		Method:        r.Method,
		URL:           r.URL,
		ContentLength: r.ContentLength,
		Body:          r.Body,
		Header:        r.Header,
	})
}

// queryNodeAgentCount
// (lines 246-264). Counts pods that match the upstream nudgebee node-agent
// job regex; the daemonset reports `up{job=~"...nudgebee(-.*)?-node-agent"}`
// per-pod, so len(result) is the count. Returns 0 on any error so the UI
// shows Disconnected rather than panicking the telemetry tick.
//
// Note: the legacy query embeds `__CLUSTER__` for multi-cluster Prom setups;
// __CLUSTER__ is replaced upstream by the relay before queries reach the
// agent. Our local probe runs against the agent's own cluster so we drop
// the token entirely.
func queryNodeAgentCount(ctx context.Context, c *prometheus.Client, logger *slog.Logger) int {
	if c == nil || c.BaseURL == "" {
		return 0
	}
	const q = `up{job=~"(.+/)?nudgebee(-.*)?-node-agent"}`
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := c.Query(cctx, q, "", "")
	if err != nil {
		logger.Debug("node-agent prom query failed", "err", err)
		return 0
	}
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			Result []json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Status != "success" {
		return 0
	}
	return len(resp.Data.Result)
}

// probeLogsProvider
// (lines 204-242). Picks the first configured provider (ES → Signoz → Loki),
// probes its health endpoint, and returns the wire-shape fields the UI reads.
//
// We only do the cheap HTTP probe (no auth) — clients with creds use the
// configured provider's own client at action-handler time. Fail-closed: any
// non-2xx → status=false, URL stays in payload so the UI can show "URL
// configured but unhealthy".
func probeLogsProvider(ctx context.Context, cfg *config.Config) (provider, url string, ok bool, providerCfg map[string]any) {
	httpClient := &http.Client{Timeout: 5 * time.Second}
	switch {
	case cfg.PinotURL != "":
		ok = httpProbe(ctx, httpClient, cfg.PinotURL+"/health")
		return "pinot", cfg.PinotURL, ok, map[string]any{}
	case cfg.ElasticsearchURL != "":
		// ES exposes a `_cluster/health` endpoint; we treat 200 as healthy.
		ok = httpProbe(ctx, httpClient, cfg.ElasticsearchURL+"/_cluster/health")
		providerCfg = map[string]any{}
		if v := os.Getenv("ELASTICSEARCH_LOG_INDEX"); v != "" {
			providerCfg["default_index"] = v
		}
		return "ES", cfg.ElasticsearchURL, ok, providerCfg
	case cfg.SignozURL != "":
		// Signoz health endpoint: /api/v1/health.
		ok = httpProbe(ctx, httpClient, cfg.SignozURL+"/api/v1/health")
		return "signoz", cfg.SignozURL, ok, map[string]any{}
	case cfg.LokiURL != "":
		ok = httpProbe(ctx, httpClient, cfg.LokiURL+"/ready")
		providerCfg = map[string]any{"url": cfg.LokiURL}
		return "loki", cfg.LokiURL, ok, providerCfg
	default:
		return "", "", false, map[string]any{}
	}
}

// probeClickhouse mirrors the legacy _check_clickhouse → db.health() probe.
// Returns false (without probing) when CLICKHOUSE_HOST is unset — the Helm
// chart only wires the host when clickhouse/otel-collector is enabled, so
// an empty host means traces are off and there's nothing to reach. When
// the host is set, hit `/ping` on the HTTP port and reflect the result.
// TRACES_ENABLED=true|false acts as an explicit override (some users run
// an external clickhouse the agent can't reach).
func probeClickhouse(ctx context.Context, c *http.Client, host, port string) bool {
	if v := os.Getenv("TRACES_ENABLED"); v == "true" {
		return true
	} else if v == "false" {
		return false
	}
	if host == "" {
		return false
	}
	return httpProbe(ctx, c, fmt.Sprintf("http://%s:%s/ping", host, port))
}

// httpProbe is a copy of telemetry.httpHealth — kept here to avoid exporting
// it. Returns true iff GET <url> returns 2xx within the client's timeout.
//
// URL is sourced from operator-provided config (PROMETHEUS_URL, LOKI_URL,
// etc.), not request-derived — taint flow is operator → probe by design.
func httpProbe(ctx context.Context, c *http.Client, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil) //nolint:gosec // operator-provided URL
	if err != nil {
		return false
	}
	resp, err := c.Do(req) //nolint:gosec // operator-provided URL
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// parseTelemetryPeriod reads CLUSTER_STATUS_PERIOD_SEC (default 60s).
// We parse as integer seconds rather than a Go Duration so the chart's
// existing env can be reused unchanged.
func parseTelemetryPeriod() time.Duration {
	s := os.Getenv("CLUSTER_STATUS_PERIOD_SEC")
	if s == "" {
		return 60 * time.Second
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 60 * time.Second
	}
	return time.Duration(n) * time.Second
}

// parseLabelMap decodes the comma-separated `key=value` syntax used by
// PROMETHEUS_ADDITIONAL_LABELS into a map. Empty input → nil so the JSON
// payload omits the field rather than emitting `{}`.
func parseLabelMap(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func handlePing(_ map[string]any) (any, error) {
	return map[string]any{"pong": true, "ts": time.Now().UTC().Format(time.RFC3339Nano)}, nil
}

func handleEcho(params map[string]any) (any, error) {
	return params, nil
}

func handleHealth(_ map[string]any) (any, error) {
	return map[string]any{
		"healthy":    true,
		"version":    version.Version,
		"commit":     version.Commit,
		"build_time": version.BuildTime,
		"go":         runtime.Version(),
		"goroutines": runtime.NumGoroutine(),
	}, nil
}
