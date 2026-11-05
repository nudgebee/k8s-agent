# Multi-stage build. The final image is Red Hat UBI 9 micro (Red Hat container
# certification, #40411). Both the agent and kubectl are static Go binaries, so
# the only OS content they need is the CA bundle and tzdata, installed into
# /rootfs below. The kubectl_command_executor action shells out to kubectl
# (pkg/kube/exec.go); nothing in the image needs a shell.
#
# Build with the repo root as a named context so the LICENSE can be copied in:
#   docker buildx build --build-context repo=. -f runner/Dockerfile runner

# golang 1.27.3: 1.27.1 builds carry the stdlib CVEs GO-2026-6612 (critical) and
# GO-2026-6603/6605/6607/6608/6610/6611/6613/6617 (high), fixed in 1.27.2.
FROM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /src

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
# GOTOOLCHAIN=local: always compile with the base image's Go, never an
# auto-downloaded toolchain — keeps the stdlib at the base image's patched release.
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
# we test against. Built from the release tag rather than downloaded: the
# published v1.36.6 binary still carries x/net v0.56.0, and this also gives it the
# same patched Go as the agent. GOARCH follows TARGETARCH so the arch matches the
# image being built. This used to be hardcoded to amd64 while runner-image.yaml
# built both arches, so the arm64 image shipped an x86-64 kubectl and every
# kubectl_command_executor action on an arm64 node died with
# `fork/exec /usr/local/bin/kubectl: exec format error` (pkg/kube/exec.go).
# Pinned to BUILDPLATFORM: Go cross-compiles, so there is no reason to run this
# emulated on the arm64 leg.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS kubectl
ARG TARGETARCH
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
ARG KUBECTL_VERSION=v1.36.6
RUN apk add --no-cache git && \
	git clone --depth 1 --branch "${KUBECTL_VERSION}" https://github.com/kubernetes/kubernetes /src
WORKDIR /src
RUN rm -rf vendor && \
	go work edit -replace golang.org/x/net=golang.org/x/net@v0.60.0 && \
	go work sync
RUN GOOS=linux GOARCH="$TARGETARCH" go build -trimpath \
	-ldflags "-s -w -X k8s.io/client-go/pkg/version.gitVersion=${KUBECTL_VERSION} -X k8s.io/component-base/version.gitVersion=${KUBECTL_VERSION}" \
	-o /usr/local/bin/kubectl ./cmd/kubectl

# Assert the built binary's ELF e_machine matches TARGETARCH, so this
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

# Everything the runtime installs from packages is assembled under /rootfs here.
# Bump OS_PKG_EPOCH (ISO week) to change this layer's cache key so a registry
# cache can't serve a stale package layer.
FROM registry.access.redhat.com/ubi9 AS rootfs-builder
ARG OS_PKG_EPOCH=2026-W45
RUN echo "os-pkg-epoch: ${OS_PKG_EPOCH}" \
 && dnf install -y --installroot /rootfs --releasever 9 --setopt=install_weak_deps=false --nodocs \
      ca-certificates tzdata \
 && dnf --installroot /rootfs clean all && rm -rf /rootfs/var/cache/*

FROM registry.access.redhat.com/ubi9/ubi-micro
ARG IMAGE_VERSION=0.0.0
ARG IMAGE_RELEASE=1
LABEL name="nudgebee-agent" \
      maintainer="nudgebee" \
      vendor="nudgebee" \
      version="${IMAGE_VERSION}" \
      release="${IMAGE_RELEASE}" \
      summary="Nudgebee Kubernetes agent" \
      description="Runs Nudgebee actions and enrichers inside a customer cluster."
COPY --from=rootfs-builder /rootfs/ /
# Red Hat certification (HasLicense): license text must be under /licenses.
COPY --from=repo LICENSE /licenses/LICENSE
# The agent writes nothing itself; /tmp is the one world-writable path for kubectl's
# discovery cache ($HOME/.kube/cache) under an arbitrary UID with no home directory.
ENV HOME=/tmp
WORKDIR /tmp
# --chmod sets the mode in the COPY itself; root-owned and world-executable so
# the arbitrary UID OpenShift assigns can run but not overwrite them.
COPY --chmod=0755 --from=build /out/nudgebee-agent /nudgebee-agent
COPY --chmod=0755 --from=kubectl /usr/local/bin/kubectl /usr/local/bin/kubectl
# Numeric uid so runAsNonRoot can be verified without an /etc/passwd entry.
USER 65532:65532
ENTRYPOINT ["/nudgebee-agent"]
