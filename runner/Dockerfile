# Multi-stage build. The final image is alpine-based so we can ship a
# working `kubectl` binary alongside the agent — the kubectl_command_executor
# action shells out to it (pkg/kube/exec.go).

# golang 1.26.5: fixes the stdlib pair CVE-2026-39822 (HIGH, os.Root symlink
# traversal) / CVE-2026-42505 (crypto/tls ECH) present in 1.26.4 builds.
FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
WORKDIR /src

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
# GOTOOLCHAIN=local: always compile with the base image's Go, never an
# auto-downloaded toolchain — keeps the stdlib at the patched 1.26.5.
ENV CGO_ENABLED=0 GOFLAGS=-trimpath GOTOOLCHAIN=local

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
# v1.36.2 (current stable): upstream builds the 1.32 line on Go 1.24.13, whose
# stale stdlib + x/net account for 33 of the image's 37 fixable Trivy findings
# (20 HIGH). v1.36.2 is built on Go 1.26.4 — residual is only the industry-wide
# 1.26.4 stdlib pair, which clears when upstream rebuilds on 1.26.5. kubectl's
# CLI surface used by kubectl_command_executor is stable across these versions.
ARG KUBECTL_VERSION=v1.36.2
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
