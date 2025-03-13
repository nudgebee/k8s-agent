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
  echo "  -f <values> values yaml"
  echo "  -m <alert_manager_url> Alert manager URL"
  echo "  -r <prometheus-org-id> Prometheus org id"
  echo "Example:"
  echo "  $0 -a my_auth_key -k my_k8s_context -o true -p http://prometheus:9090 -s my_secret"
  exit 1
}

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
# Check if service_url is empty
if [ -z "$prometheus_url" ]; then
   echo "Prometheus not found..!"
   read -p "Installing Prometheus using helm , do you want to continue? (yes/no): " install_prometheus
    if [ "$install_prometheus" == "yes" ]; then
        # Add Helm installation command here or instructions
        helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
        helm repo update
        helm upgrade --install nudgebee-prometheus prometheus-community/kube-prometheus-stack -n $namespace --create-namespace --set nodeExporter.enabled=true --set pushgateway.enabled=false --set alertmanager.enabled=true --set kubeStateMetrics.enabled=true --set grafana.enabled=true -f https://raw.githubusercontent.com/nudgebee/k8s-agent/main/extra-scrape-config.yaml
        prometheus_url="http://nudgebee-prometheus-kube-p-prometheus:9090"
        grafana_command=" --set runner.grafana.enabled=true --set runner.grafana.url=http://nudgebee-prometheus-grafana.${namespace}.svc --set runner.grafana.username=admin --set runner.grafana.password=admin "
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

# discover loki as log server if not found then provide link to nudgebee doc to configure log provider
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
