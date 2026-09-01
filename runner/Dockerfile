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
# we test against. The download follows TARGETARCH so the arch matches the image
# being built — dl.k8s.io's path segments are literally `amd64` / `arm64`, the
# same values buildx sets. This used to be hardcoded to amd64 while
# runner-image.yaml built both arches, so the arm64 image shipped an x86-64
# kubectl and every kubectl_command_executor action on an arm64 node died with
# `fork/exec /usr/local/bin/kubectl: exec format error` (pkg/kube/exec.go).
# Pinned to BUILDPLATFORM: this stage only curls a binary, so there is no reason
# to run it emulated on the arm64 leg.
FROM --platform=$BUILDPLATFORM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS kubectl
ARG TARGETARCH
# v1.36.2 (current stable): upstream builds the 1.32 line on Go 1.24.13, whose
# stale stdlib + x/net account for 33 of the image's 37 fixable Trivy findings
# (20 HIGH). v1.36.2 is built on Go 1.26.4 — residual is only the industry-wide
# 1.26.4 stdlib pair, which clears when upstream rebuilds on 1.26.5. kubectl's
# CLI surface used by kubectl_command_executor is stable across these versions.
ARG KUBECTL_VERSION=v1.36.2
RUN apk add --no-cache curl ca-certificates && \
	curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" && \
	chmod +x /usr/local/bin/kubectl

# Assert the downloaded binary's ELF e_machine matches TARGETARCH, so this
# cannot silently regress to shipping the wrong arch again.
#
# Byte 18 of the ELF header is e_machine: 0x3e = x86-64, 0xb7 = aarch64.
# We check the header rather than running `kubectl version --client` on
# purpose — both Docker Desktop (Rosetta) and CI (binfmt, registered by
# setup-qemu-action) will happily execute an amd64 binary inside an arm64
# container, so an exec-based check passes even when the arch is wrong. That
# is the same emulation that made this bug so hard to see from the outside.
RUN set -eu; \
	case "$TARGETARCH" in \
		amd64) want=3e ;; \
		arm64) want=b7 ;; \
		*) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1 ;; \
	esac; \
	got="$(hexdump -n 1 -s 18 -e '1/1 "%02x"' /usr/local/bin/kubectl)"; \
	if [ "$got" != "$want" ]; then \
		echo "kubectl ELF e_machine=0x${got}, expected 0x${want} for TARGETARCH=${TARGETARCH}" >&2; \
		exit 1; \
	fi

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
WORKDIR /
RUN apk add --no-cache ca-certificates && \
	addgroup -S nonroot -g 65532 && \
	adduser -S -D -u 65532 -G nonroot nonroot
COPY --from=build /out/nudgebee-agent /nudgebee-agent
COPY --from=kubectl /usr/local/bin/kubectl /usr/local/bin/kubectl
USER nonroot:nonroot
ENTRYPOINT ["/nudgebee-agent"]
