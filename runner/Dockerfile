# Multi-stage build. The final image is alpine-based so we can ship a
# working `kubectl` binary alongside the agent — the kubectl_command_executor
# action shells out to it (pkg/kube/exec.go).

FROM golang:1.26-alpine@sha256:3ad57304ad93bbec8548a0437ad9e06a455660655d9af011d58b993f6f615648 AS build
WORKDIR /src

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
ENV CGO_ENABLED=0 GOFLAGS=-trimpath

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build \
	-ldflags="-s -w \
		-X github.com/nudgebee/nudgebee-agent/pkg/version.Version=${VERSION} \
		-X github.com/nudgebee/nudgebee-agent/pkg/version.Commit=${COMMIT} \
		-X github.com/nudgebee/nudgebee-agent/pkg/version.BuildTime=${BUILD_TIME}" \
	-o /out/nudgebee-agent \
	./cmd/agent

# Pin kubectl to a known-good version. Bump alongside the Kubernetes versions
# we test against. The binary is amd64; if the agent runs on arm64 nodes, the
# build pipeline should pull the arm64 variant. Currently we only build amd64
# images (.github/workflows/build-image.yaml).
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS kubectl
ARG KUBECTL_VERSION=v1.32.1
RUN apk add --no-cache curl ca-certificates && \
	curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" && \
	chmod +x /usr/local/bin/kubectl

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
WORKDIR /
RUN apk add --no-cache ca-certificates && \
	addgroup -S nonroot -g 65532 && \
	adduser -S -D -u 65532 -G nonroot nonroot
COPY --from=build /out/nudgebee-agent /nudgebee-agent
COPY --from=kubectl /usr/local/bin/kubectl /usr/local/bin/kubectl
USER nonroot:nonroot
ENTRYPOINT ["/nudgebee-agent"]
