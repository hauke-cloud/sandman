# Makefile for sandman

BINARY_NAME  ?= sandman
CHART_DIR    ?= deployments/helm/sandman
IMAGE        ?= ghcr.io/hauke-cloud/sandman
IMAGE_TAG    ?= dev
PLATFORMS    ?= linux/amd64,linux/arm64

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

PKG     := github.com/hauke-cloud/sandman/internal/version
LDFLAGS := -w -s -X $(PKG).version=$(VERSION) -X $(PKG).commit=$(COMMIT) -X $(PKG).date=$(DATE)

# Container engine. podman and docker are interchangeable here.
CONTAINER_ENGINE ?= $(shell command -v docker 2>/dev/null || command -v podman 2>/dev/null)

# Pinned so that a local run and a CI run report the same findings.
GOLANGCI_LINT_VERSION ?= v2.13.2
GOLANGCI_LINT         ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# The CRD and the deepcopy functions are generated from the types in api/.
CONTROLLER_GEN_VERSION ?= v0.22.0
CONTROLLER_GEN         ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

# The generated CRD lives inside the package that embeds it. There is exactly
# one copy on purpose: the binary installs it, so a second copy in the chart or
# under config/ could only ever be out of date.
CRD_DIR := internal/crd/assets

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	  /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: build
build: ## Build the binary
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY_NAME) ./cmd/$(BINARY_NAME)

.PHONY: run
run: ## Run sandman against the cluster in your kubeconfig
	go run -ldflags="$(LDFLAGS)" ./cmd/$(BINARY_NAME)

.PHONY: test
test: ## Run tests with the race detector
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and write a coverage profile
	go test -race -count=1 -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: cover-html
cover-html: cover ## Open the coverage report in a browser
	go tool cover -html=coverage.out

.PHONY: fmt
fmt: ## Format the code
	gofmt -s -w .

.PHONY: fmt-check
fmt-check: ## Fail if any file is unformatted
	@# `gofmt -l` only lists offenders and still exits 0, so the check has to
	@# be inverted to be able to fail.
	@unformatted="$$(gofmt -s -l . | grep -v '^vendor/' || true)"; \
	if [ -n "$$unformatted" ]; then \
	  echo "not gofmt-clean:"; echo "$$unformatted"; gofmt -s -d .; exit 1; \
	fi

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	$(GOLANGCI_LINT) run

.PHONY: ci-lint
ci-lint: fmt-check vet lint manifests-check ## Every lint CI runs, in one target

.PHONY: tidy
tidy: ## Tidy and verify go.mod
	go mod tidy
	go mod verify

.PHONY: check
check: fmt ci-lint test ## Everything CI runs on the Go code

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf bin coverage.out

##@ Code generation

.PHONY: generate
generate: ## Regenerate the deepcopy functions
	$(CONTROLLER_GEN) object:headerFile=hack/boilerplate.go.txt paths=./api/...

.PHONY: manifests
manifests: ## Regenerate the CustomResourceDefinition the binary embeds
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=false paths=./api/... \
	  output:crd:artifacts:config=$(CRD_DIR)

.PHONY: manifests-check
manifests-check: ## Fail if the embedded CRD is behind the Go types
	@# The CRD ships inside the binary, so a stale one is not a documentation
	@# problem -- it is a field the API server silently drops at runtime.
	@tmp="$$(mktemp -d)"; \
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=false paths=./api/... \
	  output:crd:artifacts:config="$$tmp"; \
	if ! diff -ruN $(CRD_DIR) "$$tmp"; then \
	  rm -rf "$$tmp"; \
	  echo; echo "the embedded CRD is out of date; run 'make manifests'"; exit 1; \
	fi; \
	rm -rf "$$tmp"

##@ Container image

.PHONY: image
image: ## Build the container image for the host platform
	$(CONTAINER_ENGINE) build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg DATE=$(DATE) \
	  -t $(IMAGE):$(IMAGE_TAG) .

.PHONY: image-multiarch
image-multiarch: ## Build and push a multi-arch image (docker buildx)
	docker buildx build \
	  --platform $(PLATFORMS) \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg DATE=$(DATE) \
	  -t $(IMAGE):$(IMAGE_TAG) \
	  --push .

.PHONY: image-push
image-push: image ## Push the image
	$(CONTAINER_ENGINE) push $(IMAGE):$(IMAGE_TAG)

##@ Helm chart

.PHONY: helm-lint
helm-lint: ## Lint the chart
	helm lint $(CHART_DIR)

.PHONY: helm-template
helm-template: ## Render the chart with every optional resource enabled
	helm template sandman $(CHART_DIR) \
	  --namespace sandman \
	  --set api.token=render \
	  --set metrics.serviceMonitor.enabled=true \
	  --set metrics.prometheusRule.enabled=true \
	  --set metrics.grafanaDashboard.enabled=true \
	  --set httpRoute.enabled=true \
	  --set httpRoute.parentRefs[0].name=render

.PHONY: helm-docs
helm-docs: ## Regenerate the chart README from values.yaml
	helm-docs --chart-search-root=$(CHART_DIR) --template-files=README.md.gotmpl

.PHONY: helm-package
helm-package: ## Package the chart
	helm package $(CHART_DIR)

.PHONY: helm-install
helm-install: ## Install the chart into the current cluster
	helm upgrade --install sandman $(CHART_DIR) \
	  --namespace sandman --create-namespace

.PHONY: helm-uninstall
helm-uninstall: ## Remove the release
	helm uninstall sandman --namespace sandman

##@ Cluster

.PHONY: install-crds
install-crds: ## Apply the CRD by hand, for a cluster where sandman may not
	kubectl apply -f $(CRD_DIR)

.PHONY: devices
devices: ## Show every device and its power state
	kubectl get sandmandevices
