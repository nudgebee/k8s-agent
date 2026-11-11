# Multi-stage build. The final image is Red Hat UBI 9 micro (Red Hat container
# certification). Both the agent and kubectl are static Go binaries, so the only
# OS content they need beyond ubi-micro is the CA bundle. It is installed in a
# throwaway stage and only its files are copied over, so ubi-micro's own package
# database and files stay untouched. Timezone data is embedded in the agent
# binary (-tags timetzdata) instead of copied: ubi-micro's rpmdb lists tzdata
# but its zoneinfo files are stripped, and adding them back is flagged by
# preflight's HasModifiedFiles.
# The kubectl_command_executor action shells out to kubectl (pkg/kube/exec.go),
# not to a shell; ubi-micro does ship bash.
#
# Build with the repo root as a named context so the LICENSE can be copied in:
#   docker buildx build --build-context repo=. -f runner/Dockerfile runner

# golang 1.27.3: 1.27.1 builds carry the stdlib CVEs GO-2026-6612 (critical) and
# GO-2026-6603/6605/6607/6608/6610/6611/6613/6617 (high), fixed in 1.27.2.
FROM golang:1.27.3-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS build
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
RUN go build -tags timetzdata \
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
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS kubectl
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

# Throwaway stage: bring the CA trust store up to date. Only its files are
# copied into the final image (never /var/lib/rpm or system binaries).
FROM registry.access.redhat.com/ubi9@sha256:87f66688da4e206d5a1877088626c542288a5ccd97898cf363b588d57817e6be AS pkgs
RUN dnf install -y --setopt=install_weak_deps=false --nodocs ca-certificates \
 && dnf clean all

FROM registry.access.redhat.com/ubi9/ubi-micro@sha256:d3499a0896e2d6a1642f922615831bc5c2fd513bbb34f3446cabba530d9d54a2
ARG IMAGE_VERSION=0.0.0
ARG IMAGE_RELEASE=1
LABEL name="nudgebee-agent" \
      maintainer="nudgebee" \
      vendor="nudgebee" \
      version="${IMAGE_VERSION}" \
      release="${IMAGE_RELEASE}" \
      summary="Nudgebee Kubernetes agent" \
      description="Runs Nudgebee actions and enrichers inside a customer cluster."
# Go reads /etc/pki/tls/certs/ca-bundle.crt (a symlink into ca-trust/extracted).
COPY --from=pkgs /etc/pki/ca-trust/extracted /etc/pki/ca-trust/extracted
COPY --from=pkgs /etc/pki/tls/certs /etc/pki/tls/certs
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
