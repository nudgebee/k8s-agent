#!/bin/bash
set -euo pipefail

# ============================================================================
# Defaults
# ============================================================================

auth_key=""
k8s_context=""
openshift_enable="false"
additional_secret=""
prometheus_url=""
namespace="nudgebee-agent"
agent_name="nudgebee-agent"
disable_node_agent="false"
values=""
alert_manager_url=""
prometheus_org_id=""
relay_address=""
collector_endpoint=""
image_registry=""
disable_opencost="false"
disable_otel="false"
disable_prometheus_stack="false"
non_interactive="false"
dry_run="false"

# Release name we use for kube-prometheus-stack. Kept here so CRD preflight
# and post-install URL generation stay in sync.
PROMETHEUS_RELEASE_NAME="nudgebee-prometheus"

# Pin chart version so repeated runs are reproducible and customers don't
# silently get a new upstream CRD set. Bump deliberately.
KUBE_PROMETHEUS_STACK_VERSION="82.9.0"

# Pin the scrape-config overlay to a commit SHA instead of main, so customers
# installing at different times don't pick up unrelated changes.
EXTRA_SCRAPE_CONFIG_REF="f8c6ca8"

# CRDs shipped by kube-prometheus-stack / prometheus-operator.
PROM_OP_CRDS=(
  alertmanagerconfigs
  alertmanagers
  podmonitors
  probes
  prometheusagents
  prometheuses
  prometheusrules
  scrapeconfigs
  servicemonitors
  thanosrulers
)

# Populated by preflight_crds()
CRD_POLICY=""            # "manage" or "skip"
grafana_enabled_by_us="false"

# ============================================================================
# Usage
# ============================================================================

usage() {
  cat >&2 <<'EOF'
Usage: installation.sh -a <auth_key> [options]

Required:
  -a <auth_key>             Authentication key

Common:
  -k <k8s_context>          Kubernetes context (used for this install only;
                            does not change your default context)
  -n <namespace>            Namespace (default: nudgebee-agent)
  -z <agent_name>           Helm release name (default: nudgebee-agent)
  -p <prometheus_url>       Existing Prometheus URL; skips auto-discovery
  -m <alert_manager_url>    Alertmanager URL
  -r <prometheus_org_id>    Prometheus X-Scope-OrgID header value
  -f <values.yaml>          Extra Helm values file
  -s <additional_secret>    Name of extra secret to mount as envFrom

Self-hosted / air-gapped:
  -w <relay_address>        WebSocket relay address
  -c <collector_endpoint>   Collector endpoint URL
  -i <image_registry>       Image registry override

Toggles (pass 'true' to enable):
  -o true                   Enable OpenShift SCC
  -d true                   Disable node agent
  -x true                   Disable OpenCost
  -t true                   Disable OpenTelemetry Collector + ClickHouse
  -g true                   Disable auto-install of kube-prometheus-stack

Other:
  -y                        Non-interactive mode (fail instead of prompting)
  -D                        Dry-run (helm --dry-run; CRD preflight in report-only mode; no writes)
  -h                        Show this help

Example:
  installation.sh -a MY_KEY -k my-ctx -p http://prom.mon.svc:9090
EOF
  exit 1
}

# ============================================================================
# Flag parsing
# ============================================================================

while getopts ":a:k:o:p:s:n:z:d:f:m:r:w:c:i:x:t:g:yDh" opt; do
  case $opt in
    a) auth_key="$OPTARG" ;;
    k) k8s_context="$OPTARG" ;;
    o) openshift_enable="$OPTARG" ;;
    p) prometheus_url="$OPTARG" ;;
    s) additional_secret="$OPTARG" ;;
    n) namespace="$OPTARG" ;;
    z) agent_name="$OPTARG" ;;
    d) disable_node_agent="$OPTARG" ;;
    f) values="$OPTARG" ;;
    m) alert_manager_url="$OPTARG" ;;
    r) prometheus_org_id="$OPTARG" ;;
    w) relay_address="$OPTARG" ;;
    c) collector_endpoint="$OPTARG" ;;
    i) image_registry="$OPTARG" ;;
    x) disable_opencost="$OPTARG" ;;
    t) disable_otel="$OPTARG" ;;
    g) disable_prometheus_stack="$OPTARG" ;;
    y) non_interactive="true" ;;
    D) dry_run="true" ;;
    h) usage ;;
    \?) echo "Invalid option: -$OPTARG" >&2; usage ;;
    :)  echo "Option -$OPTARG requires an argument." >&2; usage ;;
  esac
done

# ============================================================================
# Helpers
# ============================================================================

die() { echo "Error: $*" >&2; exit 1; }

is_true() { [[ "${1:-}" == "true" ]]; }

# kubectl / helm wrappers that honour -k without mutating the user's default
# kube context.
kctl() {
  if [ -n "$k8s_context" ]; then
    kubectl --context="$k8s_context" "$@"
  else
    kubectl "$@"
  fi
}

hlm() {
  if [ -n "$k8s_context" ]; then
    helm --kube-context="$k8s_context" "$@"
  else
    helm "$@"
  fi
}

# 0 if $1 >= $2 semver-style ("4.19" >= "4.16" yes; "4.9" >= "4.16" no).
version_ge() {
  [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" = "$2" ]
}

# ============================================================================
# Preflight: required binaries & auth_key
# ============================================================================

for bin in kubectl helm sort awk curl; do
  command -v "$bin" >/dev/null 2>&1 || die "'$bin' is required but not installed."
done

if [ -z "$auth_key" ]; then
  die "Authentication key not provided (use -a)."
fi

if [ -n "$k8s_context" ]; then
  echo "Using Kubernetes context: $k8s_context (will not change your default context)"
else
  echo "Using current Kubernetes context: $(kctl config current-context)"
fi

# ============================================================================
# Kernel version check (min 4.16 for nodeAgent eBPF probes)
# ============================================================================

echo "Checking node kernel versions (minimum 4.16 for node-agent)..."
any_node_too_old="false"
while IFS=$'\t' read -r node_name kernel; do
  [ -z "${kernel:-}" ] && continue
  if version_ge "$kernel" "4.16"; then
    echo "  $node_name: $kernel OK"
  else
    echo "  $node_name: $kernel (too old; node-agent would fail on this node)" >&2
    any_node_too_old="true"
  fi
done < <(kctl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.nodeInfo.kernelVersion}{"\n"}{end}')

if is_true "$any_node_too_old" && ! is_true "$disable_node_agent"; then
  echo "At least one node has kernel <4.16; disabling node-agent globally." >&2
  disable_node_agent="true"
fi

# ============================================================================
# Service URL discovery
# ============================================================================

# Return the first matching service as a DNS URL. Prefers port named 'web' or
# 'http' (Prometheus convention) over ports[0].
getServiceURL() {
  local selectors=("$@")
  for selector in "${selectors[@]}"; do
    local svc ns port_web port_http port_first
    read -r svc ns port_web port_http port_first < <(
      kctl get svc --all-namespaces -l "$selector" \
        -o jsonpath='{range .items[0]}{.metadata.name}{" "}{.metadata.namespace}{" "}{.spec.ports[?(@.name=="web")].port}{" "}{.spec.ports[?(@.name=="http")].port}{" "}{.spec.ports[0].port}{"\n"}{end}' \
        2>/dev/null || echo ""
    )
    [ -z "${svc:-}" ] && continue
    local port="${port_web:-${port_http:-${port_first:-}}}"
    [ -z "$port" ] && continue
    echo "http://${svc}.${ns}.svc:${port}"
    return 0
  done
  return 0
}

# ============================================================================
# CRD preflight for kube-prometheus-stack
# ============================================================================
#
# kube-prometheus-stack renders its CRDs as regular Helm templates (via the
# bundled `crds` subchart). If the cluster already has those CRDs under a
# different field manager / Helm release, an upgrade triggers a server-side
# apply field-ownership conflict.
#
# Decision tree:
#   * All CRDs absent or already owned by our release → install normally.
#   * CRDs owned by a DIFFERENT Helm release → skip CRD management
#     (`--set crds.enabled=false`) and trust that release's CRDs.
#   * CRDs owned by a non-Helm manager (kubectl, raw manifests, etc.) → adopt
#     them into our Helm release: re-apply current content with field-manager
#     `helm` and `--force-conflicts` to take over ownership, then stamp the
#     standard Helm release annotations/labels. Helm will reconcile them on
#     install and create any missing CRDs normally.

preflight_crds() {
  local other_helm_release=""
  local adopt_list=() crd full release managers

  for crd in "${PROM_OP_CRDS[@]}"; do
    full="${crd}.monitoring.coreos.com"
    if ! kctl get crd "$full" >/dev/null 2>&1; then
      continue
    fi
    release=$(kctl get crd "$full" \
      -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null || echo "")
    if [ "$release" = "$PROMETHEUS_RELEASE_NAME" ]; then
      continue
    fi
    if [ -n "$release" ]; then
      other_helm_release="$release"
      echo "  $full: owned by Helm release '$release'" >&2
      continue
    fi
    managers=$(kctl get crd "$full" \
      -o jsonpath='{range .metadata.managedFields[*]}{.manager}{","}{end}' 2>/dev/null || echo "")
    echo "  $full: non-Helm owner (managers: ${managers%,})" >&2
    adopt_list+=("$full")
  done

  if [ -n "$other_helm_release" ]; then
    echo "Some CRDs are owned by Helm release '$other_helm_release'." >&2
    echo "Installing with --set crds.enabled=false to avoid fighting that release." >&2
    echo "Ensure that release ships CRDs compatible with kube-prometheus-stack $KUBE_PROMETHEUS_STACK_VERSION." >&2
    CRD_POLICY="skip"
    return
  fi

  if [ ${#adopt_list[@]} -gt 0 ]; then
    if is_true "$dry_run"; then
      echo "[dry-run] Would adopt ${#adopt_list[@]} CRD(s) into Helm release '$PROMETHEUS_RELEASE_NAME':" >&2
      for full in "${adopt_list[@]}"; do echo "  [dry-run] adopt $full" >&2; done
    else
      echo "Adopting ${#adopt_list[@]} existing CRD(s) into Helm release '$PROMETHEUS_RELEASE_NAME'..." >&2
      for full in "${adopt_list[@]}"; do
        kctl get crd "$full" -o yaml \
          | kctl apply --server-side --force-conflicts --field-manager=helm -f - >/dev/null
        kctl annotate crd "$full" \
          "meta.helm.sh/release-name=$PROMETHEUS_RELEASE_NAME" \
          "meta.helm.sh/release-namespace=$namespace" \
          --overwrite >/dev/null
        kctl label crd "$full" app.kubernetes.io/managed-by=Helm --overwrite >/dev/null
        echo "  adopted $full" >&2
      done
    fi
  fi

  CRD_POLICY="manage"
}

# ============================================================================
# Prometheus discovery / install
# ============================================================================

install_kube_prometheus_stack() {
  preflight_crds
  local extra=()
  if [ "$CRD_POLICY" = "skip" ]; then
    extra+=(--set crds.enabled=false)
  fi

  hlm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null
  hlm repo update >/dev/null

  local dry=()
  is_true "$dry_run" && dry=(--dry-run)

  hlm upgrade --install "$PROMETHEUS_RELEASE_NAME" prometheus-community/kube-prometheus-stack \
    --version "$KUBE_PROMETHEUS_STACK_VERSION" \
    --namespace "$namespace" --create-namespace \
    --wait --timeout 10m \
    --set nodeExporter.enabled=true \
    --set nodeExporter.service.targetPort=9101 \
    --set pushgateway.enabled=false \
    --set alertmanager.enabled=true \
    --set kubeStateMetrics.enabled=true \
    --set grafana.enabled=true \
    ${extra[@]+"${extra[@]}"} ${dry[@]+"${dry[@]}"} \
    -f "https://raw.githubusercontent.com/nudgebee/k8s-agent/${EXTRA_SCRAPE_CONFIG_REF}/extra-scrape-config.yaml"

  prometheus_url="http://${PROMETHEUS_RELEASE_NAME}-kube-p-prometheus.${namespace}.svc:9090"
  grafana_enabled_by_us="true"
}

discover_or_prompt_prometheus() {
  if is_true "$disable_prometheus_stack"; then
    echo "Prometheus stack disabled via -g; skipping Prometheus discovery and install."
    return
  fi

  if [ -n "$prometheus_url" ]; then
    echo "Using Prometheus URL from -p: $prometheus_url"
    return
  fi

  local selectors=(
    "app=kube-prometheus-stack-prometheus"
    "app=prometheus,component=server,release!=kubecost"
    "app=prometheus-server"
    "app=prometheus-operator-prometheus"
  )
  prometheus_url=$(getServiceURL "${selectors[@]}")
  if [ -n "$prometheus_url" ]; then
    echo "Discovered existing Prometheus: $prometheus_url"
    return
  fi

  echo "Prometheus was not auto-detected in this cluster."
  if is_true "$non_interactive"; then
    die "In non-interactive mode (-y) with no -p supplied and no Prometheus detected. Pass -p <url>, or -g true to skip the stack."
  fi

  echo "Options:"
  echo "  1) I have an existing Prometheus — enter its URL"
  echo "  2) Install kube-prometheus-stack now"
  echo "  3) Abort"
  local choice
  read -r -p "Choose [1/2/3]: " choice
  case "$choice" in
    1)
      read -r -p "Enter Prometheus URL (e.g. http://prometheus.monitoring.svc:9090): " prometheus_url
      [ -z "$prometheus_url" ] && die "No URL provided."
      # Best-effort reachability check. Cluster-internal names won't resolve
      # from the local shell, so we only fail-loud for external-looking URLs.
      if [[ "$prometheus_url" != *.svc* && "$prometheus_url" != *.local* ]]; then
        if ! curl -sf --max-time 5 "${prometheus_url%/}/-/ready" >/dev/null 2>&1; then
          echo "Warning: ${prometheus_url} not reachable from this shell; continuing anyway." >&2
        fi
      fi
      ;;
    2)
      install_kube_prometheus_stack
      ;;
    *)
      die "Aborted."
      ;;
  esac
}

discover_or_prompt_prometheus
echo "Prometheus URL in use: ${prometheus_url:-<none>}"

# ============================================================================
# nudgebee-agent install
# ============================================================================

install_agent() {
  echo "Installing nudgebee-agent..."
  hlm repo add nudgebee-agent https://nudgebee.github.io/k8s-agent/ >/dev/null
  hlm repo update >/dev/null 2>&1 || true

  local args=(
    upgrade --install "$agent_name" nudgebee-agent/nudgebee-agent
    --namespace "$namespace" --create-namespace
    --set-string "runner.nudgebee.auth_secret_key=$auth_key"
  )
  is_true "$dry_run" && args+=(--dry-run)

  # Only push a Prometheus URL if we actually have one. Leaving
  # opencost.*.external.url empty causes opencost to report an invalid target.
  if [ -n "$prometheus_url" ]; then
    args+=(--set-string "globalConfig.prometheus_url=$prometheus_url")
    if ! is_true "$disable_opencost"; then
      args+=(--set-string "opencost.opencost.prometheus.external.url=$prometheus_url")
    fi
  fi

  is_true "$disable_node_agent"       && args+=(--set nodeAgent.enabled=false)
  is_true "$openshift_enable"         && args+=(--set openshift.enable=true --set openshift.createScc=true)
  is_true "$disable_opencost"         && args+=(--set opencost.enabled=false)
  is_true "$disable_otel"             && args+=(--set opentelemetry-collector.enabled=false --set clickhouse.enabled=false)
  is_true "$disable_prometheus_stack" && args+=(--set enablePrometheusStack=false)

  if [ -n "$additional_secret" ]; then
    args+=(
      --set-string "runner.additional_env_froms[0].secretRef.name=$additional_secret"
      --set-string "runner.additional_env_froms[0].secretRef.optional=true"
    )
  fi
  [ -n "$values" ]             && args+=(-f "$values")
  [ -n "$alert_manager_url" ]  && args+=(--set-string "globalConfig.alertmanager_url=$alert_manager_url")
  [ -n "$relay_address" ]      && args+=(--set-string "runner.relay_address=$relay_address")
  [ -n "$collector_endpoint" ] && args+=(--set-string "runner.nudgebee.endpoint=$collector_endpoint")
  [ -n "$image_registry" ]     && args+=(--set-string "runner.image_registry=$image_registry")

  if [ -n "$prometheus_org_id" ]; then
    args+=(
      --set-string "globalConfig.prometheus_headers=X-Scope-OrgID: $prometheus_org_id"
      --set-string "globalConfig.alertmanager_headers=X-Scope-OrgID: $prometheus_org_id"
      --set-string "opencost.opencost.extraEnv.PROMETHEUS_HEADER_X_SCOPE_ORGID=$prometheus_org_id"
    )
  fi

  if is_true "$grafana_enabled_by_us"; then
    args+=(
      --set runner.grafana.enabled=true
      --set-string "runner.grafana.url=http://${PROMETHEUS_RELEASE_NAME}-grafana.${namespace}.svc"
      --set-string "runner.grafana.username=admin"
      --set-string "runner.grafana.password=admin"
    )
  fi

  # Print the command with the auth key redacted so log captures don't leak it.
  local printable=()
  for a in "${args[@]}"; do
    case "$a" in
      runner.nudgebee.auth_secret_key=*) printable+=("runner.nudgebee.auth_secret_key=***REDACTED***") ;;
      *) printable+=("$a") ;;
    esac
  done
  echo "Running: helm ${printable[*]}"

  hlm "${args[@]}"
}

install_agent

# ============================================================================
# Loki detection (informational)
# ============================================================================

loki_url=$(getServiceURL "app=loki" "app.kubernetes.io/instance=loki" || true)
if [ -z "$loki_url" ]; then
  echo "Log provider not detected in cluster."
  if ! is_true "$non_interactive"; then
    read -r -p "Install Loki (grafana/loki-stack) now? (yes/no): " install_loki
    if [ "$install_loki" = "yes" ]; then
      hlm repo add grafana https://grafana.github.io/helm-charts >/dev/null
      hlm repo update >/dev/null 2>&1 || true
      hlm upgrade --install nudgebee-loki grafana/loki-stack \
        --namespace "$namespace" --create-namespace \
        --set loki.persistence.enabled=true \
        --set loki.persistence.size=10Gi \
        --set promtail.enabled=true \
        --set loki.isDefault=false
      echo "Loki installed at http://nudgebee-loki.${namespace}.svc:3100"
      echo "Configure NudgeBee's logging provider per the docs: https://app.nudgebee.com/help/docs/installation/agent/installation/logging/"
    fi
  fi
else
  echo "Existing Loki detected at $loki_url"
  echo "Configure NudgeBee to use it: https://app.nudgebee.com/help/docs/installation/agent/installation/logging/"
fi

# ============================================================================
# Summary
# ============================================================================

echo
echo "Installation summary:"
echo "  Namespace:         $namespace"
echo "  Agent release:     $agent_name"
echo "  Prometheus URL:    ${prometheus_url:-<disabled>}"
echo "  OpenCost:          $(is_true "$disable_opencost" && echo disabled || echo enabled)"
echo "  OpenTelemetry/CH:  $(is_true "$disable_otel" && echo disabled || echo enabled)"
echo "  Prometheus stack:  $(is_true "$disable_prometheus_stack" && echo disabled || echo enabled)"
if ! is_true "$disable_otel"; then
  echo "  Note: ClickHouse requires a PVC. If your cluster has no default storage class, provision one manually."
fi
echo
echo "Checking pod status in namespace $namespace:"
kctl -n "$namespace" get pods || true
echo
echo "Pods may still be starting. Monitor with: kubectl -n $namespace get pods -w"
