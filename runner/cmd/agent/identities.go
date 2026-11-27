package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nudgebee/nudgebee-agent/pkg/config"
	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
	"github.com/nudgebee/nudgebee-agent/pkg/identity"
	"github.com/nudgebee/nudgebee-agent/pkg/kube"
)

const (
	identityReaderWait   = 60 * time.Second
	identityRecheckEvery = 5 * time.Minute
)

// readerProbeResources are the resources the agent's informers list and watch
// (discovery, k8swatch, the events cache). A reader that cannot list one of
// them would leave that informer unsynced forever, so the startup check
// requires all of them.
var readerProbeResources = []identity.Resource{
	{Resource: "pods"}, {Resource: "nodes"}, {Resource: "namespaces"},
	{Resource: "configmaps"}, {Resource: "services"}, {Resource: "events"},
	{Group: "apps", Resource: "deployments"}, {Group: "apps", Resource: "statefulsets"},
	{Group: "apps", Resource: "daemonsets"}, {Group: "apps", Resource: "replicasets"},
	{Group: "batch", Resource: "jobs"}, {Group: "batch", Resource: "cronjobs"},
	{Group: "networking.k8s.io", Resource: "ingresses"},
}

// setupIdentities builds the identity set. In split mode it mints the tokens,
// waits for the reader's, and checks every identity. readerReady is false
// when the reader check failed: the caller must not start informers, and a
// background watcher restarts the agent once the reader passes.
func setupIdentities(ctx context.Context, cfg *config.Config, typed kubernetes.Interface, dyn dynamic.Interface, restCfg *rest.Config, logger *slog.Logger) (ids *identity.Set, readerReady bool, err error) {
	if typed == nil || cfg.RunnerIdentityMode != "split" {
		if typed != nil {
			logger.Info("identities: legacy mode, every Kubernetes call runs as the runner's own account")
		}
		return identity.Legacy(typed, dyn, restCfg), true, nil
	}
	ids, err = identity.NewSplit(restCfg, typed, dyn, identity.Config{
		Namespace: installNamespace(cfg),
		ReaderSA:  cfg.ReaderServiceAccount,
		ExecSA:    cfg.ExecServiceAccount,
		WriterSA:  cfg.WriterServiceAccount,
	}, logger)
	if err != nil {
		return nil, false, fmt.Errorf("split identities: %w", err)
	}
	ids.Start(ctx)
	if err := ids.WaitTokens(ctx, identityReaderWait); err != nil {
		logger.Error("identities: not every identity has a token yet", "err", err)
	}
	st := ids.Check(ctx, readerProbeResources, logger)
	logger.Info("identities: split mode",
		"reader", cfg.ReaderServiceAccount, "exec", cfg.ExecServiceAccount, "writer", cfg.WriterServiceAccount,
		"reader_ok", st.OK)
	startedReady := st.OK
	go ids.Watch(ctx, identityRecheckEvery, readerProbeResources, logger, func(st identity.Status) {
		if st.OK && !startedReady {
			// Informers were not started; a restart starts them.
			logger.Warn("identities: reader check passes now, restarting so informers start")
			os.Exit(3)
		}
	})
	return ids, st.OK, nil
}

// identityOrReader returns c, or the reader when c is disabled. Packages then
// always get a working client; the action guards refuse the call with a clear
// error before it is made.
func identityOrReader(ids *identity.Set, c *identity.Clients) *identity.Clients {
	if c == nil {
		return ids.Reader
	}
	return c
}

// kubectlCredentials gives the kubectl executor per-command kubeconfigs for the
// reader and (when enabled) the writer.
func kubectlCredentials(ids *identity.Set, restCfg *rest.Config, namespace string) *kube.Credentials {
	c := &kube.Credentials{
		Server:    restCfg.Host,
		CAFile:    restCfg.CAFile,
		CAData:    restCfg.CAData,
		Namespace: namespace,
		Reader:    ids.Reader.Tokens.Token,
	}
	if ids.Writer != nil {
		c.Writer = ids.Writer.Tokens.Token
	}
	return c
}

// writerActions change the customer's cluster; they need the writer identity.
// Alert-rule actions are not listed: rules in the agent's namespace are
// housekeeping, and the API server decides for rules elsewhere.
var writerActions = []string{
	"delete_pod", "delete_job", "cordon", "uncordon", "drain", "rollout_restart",
	"create_workload", "replace_workload", "delete_workload", "revert_workload",
	"replica_rightsizing", "rightsize_pvc", "volume_delete",
	"rightsizing_resource", "pod_profiler",
}

// execActions exec into customer pods; they need the exec identity.
var execActions = []string{"pod_bash_enricher"}

// guardIdentityActions wraps the handlers that need the writer or exec
// identity so a call is refused with a clear reason when that identity is
// disabled on this agent or failed its last check. continuous_rightsizing is
// guarded only when it applies (not recommend-only).
func guardIdentityActions(handlers map[string]dispatch.Handler, ids *identity.Set) {
	if !ids.Split {
		return
	}
	wrap := func(name, identityName string, applies func(map[string]any) bool) {
		h, ok := handlers[name]
		if !ok {
			return
		}
		handlers[name] = func(ctx context.Context, p map[string]any) (any, error) {
			if applies == nil || applies(p) {
				if err := ids.Usable(identityName); err != nil {
					return nil, fmt.Errorf("%s: not permitted on this agent: %w", name, err)
				}
			}
			return h(ctx, p)
		}
	}
	for _, n := range writerActions {
		wrap(n, identity.NameWriter, nil)
	}
	wrap("continuous_rightsizing", identity.NameWriter, isRightsizingApply)
	for _, n := range execActions {
		wrap(n, identity.NameExec, nil)
	}
}

// readyzHandler reports not ready while the reader identity fails its check.
func readyzHandler(ids *identity.Set) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if err := ids.Usable(identity.NameReader); err != nil {
			http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}
}

func identityMode(ids *identity.Set) string {
	if ids != nil && ids.Split {
		return "split"
	}
	return "legacy"
}

func identityProblems(ids *identity.Set) map[string]string {
	if ids == nil || !ids.Split {
		return nil
	}
	out := map[string]string{}
	for name, st := range ids.Statuses() {
		if !st.OK {
			out[name] = st.Reason
		} else if st.Warning != "" {
			out[name] = "warning: " + st.Warning
		}
	}
	return out
}
