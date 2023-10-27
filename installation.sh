#!/bin/bash
set -e  # Enable error handling

# Check if an access key is provided as a command-line argument
if [ $# -ne 1 ]; then
    echo "Error: Access key not provided. Please provide an access key as a command-line argument."
    exit 1
fi
auth_key="$1"
k8s_context="$2"  # Optionally provided Kubernetes context

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
            local service_url="${name}.${namespace}.svc.cluster.local:${port}"
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
prometheus_selectors=(
        "app=kube-prometheus-stack-prometheus"
        "app=prometheus,component=server,release!=kubecost"
        "app=prometheus-server"
        "app=prometheus-operator-prometheus"
        "app=prometheus-msteams"
        "app=rancher-monitoring-prometheus"
        "app=prometheus-prometheus"
)
# Call the function with the array as an argument
PROMETHEUS_SERVER_ENDPOINT=$(getPrometheusURL "${prometheus_selectors[@]}")

# Check if service_url is empty
if [ -z "$PROMETHEUS_SERVER_ENDPOINT" ]; then
   echo "Prometheus not found..!"
   read -p "Installing Prometheus using helm , do you want to continue? (yes/no): " install_prometheus
    if [ "$install_prometheus" == "yes" ]; then
        # Add Helm installation command here or instructions
        echo "Installing prometheus using"
        helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
        helm repo update
        helm install prometheus prometheus-community/kube-prometheus-stack -n prometheus --create-namespace -f https://raw.githubusercontent.com/opencost/opencost/develop/kubernetes/prometheus/extraScrapeConfigs.yaml
        # Call the function with the array as an argument
        PROMETHEUS_SERVER_ENDPOINT=$(getPrometheusURL "${prometheus_selectors[@]}")
    else
        echo "Prometheus installation not requested. Exiting."
        exit 0
    fi
else
    echo "Discovered Prometheus URL: $PROMETHEUS_SERVER_ENDPOINT"
fi

opencost_selectors=(
        "app=opencost"
)
# Call the function with the array as an argument
opencost_service_url=$(getPrometheusURL "${opencost_selectors[@]}")
if [ -z "$opencost_service_url" ]; then
    wget https://raw.githubusercontent.com/nudgebee/k8s-agent/main/opencost/opencost.yaml
    envsubst < opencost.yaml
    kubectl apply -f opencost.yaml
    rm opencost.yaml
    opencost_service_url=$(getPrometheusURL "${opencost_selectors[@]}")
else
   echo "Discovered OpenCost URL: $opencost_service_url"
fi

echo "Installing nudgebee agent using helm"
helm repo add nudgebee-agent https://nudgebee.github.io/k8s-agent/
helm repo update
# Use helm upgrade --install to either install or upgrade the Helm chart
helm upgrade --install nudgebee-agent nudgebee-agent/nudgebee-agent --namespace nudgebee-agent --create-namespace --set runner.nudgebee.auth_secret_key="$auth_key"

echo "Installation/upgrade completed."