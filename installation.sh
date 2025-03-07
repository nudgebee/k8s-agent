#!/bin/bash
set -e  # Enable error handling

# Initialize variables with default values
auth_key=""
k8s_context=""
openshift_enable=""
additional_secret=""
prometheus_url=""
opencost_service_url=""
namespace="nudgebee-agent"
agent_name="nudgebee-agent"
env="prod"
disable_node_agent=""
values=""
alert_manager_url=""
prometheus_org_id=""

# Help function
usage() {
  echo "Usage: $0 [-a <auth_key>] [-k <k8s_context>] [-o <openshift_enable>] [-p <prometheus_url>] [-s <additional_secret>]"
  echo ""
  echo "Options:"
  echo "  -a <auth_key>           Authentication key (required)"
  echo "  -k <k8s_context>        Kubernetes context"
  echo "  -o <openshift_enable>   OpenShift enable option"
  echo "  -p <prometheus_url>     Prometheus URL"
  echo "  -s <additional_secret>  Additional secret"
  echo "  -n <namespace>          Namespace"
  echo "  -z <agent_name>         Agent_name"
  echo "  -h <help>               Help"
  echo "  -d <disable_node_agent> Disable node agent"
  echo "  -f <values>             Values yaml"
  echo "  -m <alert_manager_url>  Alert manager URL"
  echo "  -r <prometheus-org-id>  Prometheus org id"
  echo "Example:"
  echo "  $0 -a my_auth_key -k my_k8s_context -o true -p http://prometheus:9090 -s my_secret"
  exit 1
}

# Parse command-line arguments
while getopts ":a:k:o:p:s:n:z:h:e:d:f:m:r:" opt; do
  case $opt in
    a)
      auth_key="$OPTARG"
      ;;
    k)
      k8s_context="$OPTARG"
      ;;
    o)
      openshift_enable="$OPTARG"
      ;;
    p)
      prometheus_url="$OPTARG"
      ;;
    s)
      additional_secret="$OPTARG"
      ;;
    n)
      namespace="$OPTARG"
      ;;
    z)
      agent_name="$OPTARG"
      ;;
    d) 
      disable_node_agent="$OPTARG"
      ;;
    f) 
      values="$OPTARG"
      ;;
    m)
      alert_manager_url="$OPTARG"
      ;;
    r)
      prometheus_org_id="$OPTARG"
      ;;
    h)
      usage
      ;;
    \?)
      echo "Invalid option: -$OPTARG" >&2
      usage
      ;;
    :)
      echo "Option -$OPTARG requires an argument." >&2
      usage
      ;;
  esac
done

# Check if an access key is provided
if [ -z "$auth_key" ]; then
  echo "Error: Access key not provided. Please provide an access key using -a or --auth-key."
  exit 1
fi

# Kernel version check
REQUIRED_KERNEL_VERSION="4.16"  # Minimum required kernel version
DISABLE_NODE_AGENT="false"

# Function to compare kernel versions
compare_versions() {
    if (( $(echo "$1 < $2" | bc -l) )); then
        return 1  # Version is less than required
    else
        return 0  # Version is supported
    fi
}

# Kernel version check
echo "🔍 Checking kernel versions of all nodes..."
while read -r node kernel_version; do
    KERNEL_VERSION=$(echo "$kernel_version" | awk -F'.' '{print $1"."$2}')
    if compare_versions "$KERNEL_VERSION" "$REQUIRED_KERNEL_VERSION"; then
        echo "✅ Node $node has a supported kernel version ($KERNEL_VERSION)."
    else
        echo "❌ Error: Node $node has an unsupported kernel version ($KERNEL_VERSION)."
        echo "   Node Agent requires a minimum kernel version of $REQUIRED_KERNEL_VERSION for tracing and logging."
        read -p "Do you want to proceed by disabling the Node Agent? (yes/no): " disable_choice
        if [[ "$disable_choice" == "yes" ]]; then
            DISABLE_NODE_AGENT="true"
            echo "⚠️ Node Agent will be disabled. Tracing and logging capabilities will be limited."
        else
            echo "❌ Installation aborted. Please upgrade the kernel or rerun with Node Agent disabled."
            exit 1
        fi
    fi
done < <(kubectl get nodes -o custom-columns="NAME:.metadata.name,KERNEL:.status.nodeInfo.kernelVersion" --no-headers)

# Enforce Node Agent importance
if [ "$DISABLE_NODE_AGENT" == "true" ]; then
    echo "ℹ️ Node Agent is essential for logs and tracing. Disabling it may impact troubleshooting."
    echo "⚠️ WARNING: Node Agent is disabled. Tracing and logging capabilities will be limited."
else
    echo "✅ Node Agent is enabled. Tracing and logging capabilities are fully supported."
fi

# Log the Kubernetes context that will be used
if [ -n "$k8s_context" ]; then
    echo "Using the specified Kubernetes context: $k8s_context"
    kubectl config use-context "$k8s_context"
else
    current_context=$(kubectl config current-context)
    echo "Using the current Kubernetes context: $current_context"
fi

# Function to get Prometheus URL
getPrometheusURL() {
    local selectors=("$@")
    for selector in "${selectors[@]}"; do
        service_info=$(kubectl get svc --all-namespaces -l "$selector" -o custom-columns=NAME:.metadata.name,NAMESPACE:.metadata.namespace,PORT:.spec.ports[0].port --no-headers 2>/dev/null)

        if [ -n "$service_info" ]; then
            local name=$(echo "$service_info" | awk '{print $1}')
            local namespace=$(echo "$service_info" | awk '{print $2}')
            local port=$(echo "$service_info" | awk '{print $3}')

            # Generate and return Prometheus URL
            local service_url="http://${name}.${namespace}.svc:${port}"
            echo "$service_url"
            return
        fi
    done

    # If no Prometheus service is found, return an empty string
    echo ""
}

# Check if kubectl is installed
if ! command -v kubectl &> /dev/null; then
    echo "Error: kubectl is not installed. You can install it by following the instructions at:"
    echo "https://kubernetes.io/docs/tasks/tools/install-kubectl/"
    exit 1
fi

# Check if helm is installed
if ! command -v helm &> /dev/null; then
    echo "Error: Helm is not installed. You can install it by following the instructions at:"
    echo "https://helm.sh/docs/intro/install/"
    exit 1
fi

# Discover Prometheus URL if not provided
if [ -z "$prometheus_url" ]; then
    prometheus_selectors=(
            "app=kube-prometheus-stack-prometheus"
            "app=prometheus,component=server,release!=kubecost"
            "app=prometheus-server"
            "app=prometheus-operator-prometheus"
    )
    prometheus_url=$(getPrometheusURL "${prometheus_selectors[@]}")
fi

existingPrometheus=false
grafana_command=""

# Check if Prometheus URL is empty
if [ -z "$prometheus_url" ]; then
   echo "Prometheus not found..!"
   read -p "Installing Prometheus using helm, do you want to continue? (yes/no): " install_prometheus
    if [ "$install_prometheus" == "yes" ]; then
        # Add Helm installation command here or instructions
        helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
        helm repo update

        # Check if the rohit-monitoring namespace already exists
        if kubectl get namespace rohit-monitoring &> /dev/null; then
            echo "ℹ️ Using existing 'rohit-monitoring' namespace."
        else
            echo "ℹ️ Creating 'rohit-monitoring' namespace."
            kubectl create namespace rohit-monitoring
        fi

        helm upgrade --install nudgebee-prometheus prometheus-community/kube-prometheus-stack -n rohit-monitoring --set nodeExporter.enabled=true --set pushgateway.enabled=false --set alertmanager.enabled=true --set kubeStateMetrics.enabled=true --set grafana.enabled=true -f https://raw.githubusercontent.com/nudgebee/k8s-agent/main/extra-scrape-config.yaml
        prometheus_url="http://nudgebee-prometheus-kube-p-prometheus.rohit-monitoring.svc:9090"
        grafana_command=" --set runner.grafana.enabled=true --set runner.grafana.url=http://nudgebee-prometheus-grafana.rohit-monitoring.svc --set runner.grafana.username=admin --set runner.grafana.password=admin "
    else
        echo "Prometheus installation not requested. Exiting."
        exit 0
    fi
else
    existingPrometheus=true
fi

echo "Discovered Prometheus URL: $prometheus_url"

echo "Installing nudgebee agent using helm"
helm repo add nudgebee-agent https://nudgebee.github.io/k8s-agent/
helm repo update > /dev/null 2>&1

addition_secret_command=""
if [ -n "$additional_secret" ]; then
    addition_secret_command=" --set-string runner.additional_env_froms[0].secretRef.name=$additional_secret --set-string runner.additional_env_froms[0].secretRef.optional=true"
fi

openshift_enable_command=""
if [ -n "$openshift_enable" ]; then
    openshift_enable_command=" --set-string openshift.enable=true --set-string openshift.createScc=true"
fi

disable_node_agent_command=""
if [ -n "$disable_node_agent" ]; then
  disable_node_agent_command=" --set nodeAgent.enabled=false"
fi

values_command=""
if [ -n "$values" ]; then
  values_command=" -f $values"
fi

alert_manager_url_command=""
if [ -n "$alert_manager_url" ]; then
  alert_manager_url_command=" --set globalConfig.alertmanager_url=$alert_manager_url"
fi

prometheus_org_id_command=""
echo "Prometheus org id: $prometheus_org_id"
if [ -n "$prometheus_org_id" ]; then
  prometheus_org_id_command=" --set globalConfig.prometheus_headers='X-Scope-OrgID: $prometheus_org_id' --set globalConfig.alertmanager_headers='X-Scope-OrgID: $prometheus_org_id' --set opencost.opencost.extraEnv.PROMETHEUS_HEADER_X_SCOPE_ORGID=$prometheus_org_id"
fi

# Use helm upgrade --install to either install or upgrade the Helm chart
a="helm upgrade --install $agent_name nudgebee-agent/nudgebee-agent  --namespace $namespace --create-namespace --set runner.nudgebee.auth_secret_key="$auth_key" --set globalConfig.prometheus_url="$prometheus_url" --set opencost.opencost.prometheus.external.url="$prometheus_url" $disable_node_agent_command $openshift_enable_command $addition_secret_command $values_command $grafana_command $alert_manager_url_command $prometheus_org_id_command"

echo "Running command: $a"
eval $a

# Discover Loki as log server if not found, then provide link to nudgebee doc to configure log provider
loki_selectors=(
        "app=loki"
        "app.kubernetes.io/instance=loki"
)

RED='\033[0;31m'
NC='\033[0m'
loki_url=$(getPrometheusURL "${loki_selectors[@]}")
if [ -z "$loki_url" ]; then
    echo "Log provider not found..!"
    echo "${RED}Please configure Loki/ELK as log provider by following the instructions at: https://app.nudgebee.com/help/docs/installation/agent/installation/logging/${NC}"
fi

# If existing Prometheus, provide link to configure alert manager, scrape config, and additionalRulesMap
if [ "$existingPrometheus" = true ]; then
    echo "${RED}Please configure alert manager, scrape config, and alert rules by following the instructions at: https://app.nudgebee.com/help/docs/installation/agent/installation/existing-prometheus/${NC}"
fi

# Installation status check
echo "🔍 Verifying installation status of all components..."

# Node Agent status
if [ "$DISABLE_NODE_AGENT" == "true" ]; then
    echo "⚠️ Node Agent is disabled. Skipping status check."
else
    if kubectl get pods -n $namespace | grep -q "node-agent"; then
        echo "✅ Node Agent is running."
    else
        echo "❌ Node Agent is not running. Possible reasons:"
        echo "   - Insufficient permissions to create Node Agent resources."
        echo "   - Kernel version is not supported."
        echo "   - Node Agent image pull failed."
        echo "➡️ Troubleshooting guide: https://app.nudgebee.com/help/docs/installation/agent/installation/node-agent-failures/"
    fi
fi

# Loki/ELK status
if kubectl get pods -n $namespace | grep -q "loki"; then
    echo "✅ Loki is running."
else
    echo "❌ Loki is not running. Possible reasons:"
    echo "   - Insufficient storage for Loki PVCs."
    echo "   - Helm chart installation failed."
    echo "   - Loki service is not exposed correctly."
    echo "➡️ Troubleshooting guide: https://app.nudgebee.com/help/docs/installation/agent/installation/loki-failures/"
fi

# Prometheus status
if kubectl get pods -n $namespace | grep -q "prometheus"; then
    echo "✅ Prometheus is running."
else
    echo "❌ Prometheus is not running. Possible reasons:"
    echo "   - Insufficient resources (CPU/Memory) for Prometheus."
    echo "   - Helm chart installation failed."
    echo "   - Prometheus service is not exposed correctly."
    echo "➡️ Troubleshooting guide: https://app.nudgebee.com/help/docs/installation/agent/installation/prometheus-failures/"
fi

# ClickHouse status
if kubectl get pods -n $namespace | grep -q "clickhouse"; then
    echo "✅ ClickHouse is running."
else
    echo "❌ ClickHouse is not running. Possible reasons:"
    echo "   - PVCs were not created or are not bound."
    echo "   - Helm chart installation failed."
    echo "   - Insufficient resources (CPU/Memory) for ClickHouse."
    echo "➡️ Troubleshooting guide: https://app.nudgebee.com/help/docs/installation/agent/installation/clickhouse-failures/"
fi

# Final output
echo "🚀 Installation completed successfully!"
if [ "$DISABLE_NODE_AGENT" == "true" ]; then
    echo "⚠️ Node Agent is disabled. Tracing and logging capabilities may be limited."
fi
if [ "$existingPrometheus" == "true" ]; then
    echo "ℹ️ Using existing Prometheus server at $prometheus_url."
else
    echo "ℹ️ Prometheus installed and configured at $prometheus_url."
fi
echo "ℹ️ ClickHouse is installed and configured with PVCs for storage."
echo "➡️ For further assistance, visit the NudgeBee documentation: https://app.nudgebee.com/help/docs/installation/agent/installation/"
