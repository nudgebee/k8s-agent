// Command actionctl is a local dev harness for invoking the agent's
// agent_task remediation handlers against a real cluster, without standing up
// the relay / backend / poller. It builds the same mutate.Handlers map the
// agent registers and calls one handler by action_name with a params map —
// exactly what the task poller does via HandleTrusted.
//
// Cluster creds come from in-cluster config or, locally, $KUBECONFIG /
// ~/.kube/config (k8sclient.New). Use it to exercise replica_rightsizing,
// rightsize_pvc (expand + downsize migration), and volume_delete.
//
// Examples:
//
//	go run ./cmd/actionctl -action replica_rightsizing -kind Deployment -namespace demo -name web -replicas 0
//	go run ./cmd/actionctl -action rightsize_pvc -namespace demo -name data -size 3Gi     # expand
//	go run ./cmd/actionctl -action rightsize_pvc -namespace demo -name data -size 1Gi     # downsize migration
//	go run ./cmd/actionctl -action volume_delete -pv pvc-abc123                            # PV name
//
// This is a mutation tool. It acts on whatever cluster your kubeconfig points
// at — double-check the context first.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/client-go/dynamic"

	"github.com/nudgebee/nudgebee-agent/internal/k8sclient"
	"github.com/nudgebee/nudgebee-agent/pkg/mutate"
	"github.com/nudgebee/nudgebee-agent/pkg/podexec"
)

func main() {
	var (
		action     = flag.String("action", "", "replica_rightsizing | rightsize_pvc | volume_delete")
		kubeconfig = flag.String("kubeconfig", "", "kubeconfig path (default: $KUBECONFIG or ~/.kube/config)")
		namespace  = flag.String("namespace", "", "namespace (rightsize_pvc, replica_rightsizing)")
		name       = flag.String("name", "", "PVC name (rightsize_pvc) or workload name (replica_rightsizing)")
		size       = flag.String("size", "", "target size for rightsize_pvc, e.g. 3Gi")
		kind       = flag.String("kind", "", "Deployment | StatefulSet | Rollout (replica_rightsizing)")
		replicas   = flag.Int("replicas", -1, "target replica count (replica_rightsizing)")
		pv         = flag.String("pv", "", "PersistentVolume name (volume_delete)")
		timeout    = flag.Duration("timeout", 55*time.Minute, "overall deadline (downsize copies can be slow)")
	)
	flag.Parse()
	if *action == "" {
		fmt.Fprintln(os.Stderr, "missing -action")
		flag.Usage()
		os.Exit(2)
	}

	cs, restCfg, err := k8sclient.New(*kubeconfig)
	if err != nil {
		fatal("build kube client", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		fatal("build dynamic client", err)
	}

	mut := mutate.New(cs, "", nil)
	mut.SetDynamic(dyn)
	mut.SetExec(podexec.New(cs, restCfg))

	handlers := mutate.Handlers(mut)
	h, ok := handlers[*action]
	if !ok {
		fatal("unknown action", fmt.Errorf("%q not registered (have it spelled right?)", *action))
	}

	params := map[string]any{}
	switch *action {
	case "replica_rightsizing":
		if *replicas < 0 {
			fatal("params", fmt.Errorf("replica_rightsizing needs -replicas >= 0"))
		}
		params = map[string]any{"kind": *kind, "namespace": *namespace, "name": *name, "replica_count": *replicas}
	case "rightsize_pvc":
		params = map[string]any{"namespace": *namespace, "name": *name, "size": *size}
	case "volume_delete":
		params = map[string]any{"name": *pv}
	default:
		fatal("params", fmt.Errorf("no param mapping for action %q", *action))
	}

	fmt.Printf("→ %s %s\n", *action, mustJSON(params))
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	start := time.Now()
	data, err := h(ctx, params)
	elapsed := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fmt.Printf("✗ FAILED in %s: %v\n", elapsed, err)
		os.Exit(1)
	}
	fmt.Printf("✓ OK in %s: %s\n", elapsed, mustJSON(data))
}

func fatal(ctx string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", ctx, err)
	os.Exit(1)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
