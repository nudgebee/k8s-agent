# Build from the repository root, after `helm dependency build charts/nudgebee-agent`:
#   docker build -f operator/Dockerfile .
# The chart is taken from charts/, so the operator cannot drift from it.
FROM quay.io/operator-framework/helm-operator:v1.42.3

ENV HOME=/opt/helm
COPY operator/watches.yaml ${HOME}/watches.yaml
COPY charts/nudgebee-agent ${HOME}/helm-charts/nudgebee-agent
WORKDIR ${HOME}
