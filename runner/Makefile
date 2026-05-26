# nudgebee-agent — minimal Makefile for the Go agent.

BINARY      := nudgebee-agent
PKG         := github.com/nudgebee/nudgebee-agent
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/pkg/version.Version=$(VERSION) \
	-X $(PKG)/pkg/version.Commit=$(COMMIT) \
	-X $(PKG)/pkg/version.BuildTime=$(BUILD_TIME)

.PHONY: fmt lint test test-coverage test-integration build run validate clean docker-build setup-envtest

# K8s version pinned for envtest; bump in lockstep with the cluster versions
# we support. setup-envtest downloads kube-apiserver/etcd/kubectl on first use.
ENVTEST_K8S_VERSION ?= 1.32.x

fmt:
	gofmt -s -w .

lint:
	go vet ./...
	@command -v golangci-lint >/dev/null 2>&1 && golangci-lint run --timeout 5m || \
		echo "golangci-lint not installed; skipping"

# Unit tests only (no Docker, no envtest). Fast — runs in seconds.
test:
	go test -race -count=1 ./...

# Coverage report for unit tests. Open coverage.html in a browser.
# Excludes cmd/agent (integration glue, exercised by integration tests) and
# pkg/version (linker-stamped variables). Standard pattern for Go projects.
COVER_PKG := $(shell go list ./pkg/... ./internal/... | grep -v /pkg/version | tr '\n' ',' | sed 's/,$$//')

test-coverage:
	go test -race -count=1 -coverpkg=$(COVER_PKG) -coverprofile=coverage.out -covermode=atomic \
		./pkg/... ./internal/...
	go tool cover -func=coverage.out | tail -1
	@command -v go >/dev/null && go tool cover -html=coverage.out -o coverage.html && \
		echo "→ coverage.html"

# Integration tests: real kube-apiserver via envtest + real Prometheus/Loki
# via testcontainers-go (Docker required). Slower — minutes.
#
# Requires `setup-envtest` on PATH. Run `make setup-envtest` once.
test-integration: ensure-envtest
	KUBEBUILDER_ASSETS="$$(setup-envtest use $(ENVTEST_K8S_VERSION) -p path)" \
		go test -race -count=1 -tags=integration -timeout 5m ./...

# Idempotent: installs setup-envtest into $GOPATH/bin and pre-downloads
# the K8s test binaries so test-integration is reproducible.
setup-envtest:
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	$$(go env GOPATH)/bin/setup-envtest use $(ENVTEST_K8S_VERSION)

# Internal: fail with a helpful message if setup-envtest isn't on PATH.
ensure-envtest:
	@command -v setup-envtest >/dev/null 2>&1 || { \
		echo "setup-envtest not found on PATH."; \
		echo "Run: make setup-envtest"; \
		echo "Then ensure $$(go env GOPATH)/bin is on PATH."; \
		exit 1; }

validate: fmt lint test

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='$(LDFLAGS)' -o bin/$(BINARY) ./cmd/agent

run:
	go run ./cmd/agent

docker-build:
	docker build -t nudgebee-agent:$(VERSION) -f Dockerfile .

clean:
	rm -rf bin/ coverage.out coverage.html
