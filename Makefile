# pg-k8s-proxy — PostgreSQL gateway and Kubernetes operator.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
.PHONY: help

# ---- Configuration ----------------------------------------------------------

IMAGE_REGISTRY ?= ghcr.io/tokaco
IMAGE_NAME     ?= pg-k8s-proxy
# version.txt is maintained by release-please and is the authority for the
# released version; git describe only refines it for local builds.
CHART_VERSION  := $(shell cat version.txt 2>/dev/null || echo 0.0.0)
IMAGE_TAG      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo $(CHART_VERSION))
IMAGE          ?= $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)
PLATFORMS      ?= linux/amd64,linux/arm64

VERSION    ?= $(IMAGE_TAG)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

NAMESPACE    ?= pgproxy
RELEASE      ?= pg-k8s-proxy
CHART        := charts/pg-k8s-proxy
SRC          := src
BIN          := $(CURDIR)/bin

CONTROLLER_GEN_VERSION ?= v0.19.0
GOLANGCI_LINT_VERSION  ?= v2.5.0

LDFLAGS := -s -w \
	-X github.com/tokaco/pg-k8s-proxy/internal/version.Version=$(VERSION) \
	-X github.com/tokaco/pg-k8s-proxy/internal/version.Commit=$(COMMIT) \
	-X github.com/tokaco/pg-k8s-proxy/internal/version.BuildDate=$(BUILD_DATE)

help: ## Show this help
	@echo "pg-k8s-proxy"
	@echo
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# ---- Tools ------------------------------------------------------------------

$(BIN):
	@mkdir -p $(BIN)

CONTROLLER_GEN := $(BIN)/controller-gen
$(CONTROLLER_GEN): | $(BIN)
	GOBIN=$(BIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

GOLANGCI_LINT := $(BIN)/golangci-lint
$(GOLANGCI_LINT): | $(BIN)
	GOBIN=$(BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: tools
tools: $(CONTROLLER_GEN) $(GOLANGCI_LINT) ## Install the development tools into ./bin

# ---- Code generation --------------------------------------------------------

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Regenerate the DeepCopy methods
	cd $(SRC) && $(CONTROLLER_GEN) object paths=./api/...

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Regenerate the CRDs and sync them into the chart
	cd $(SRC) && $(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=$(CURDIR)/deploy/crds
	./hack/sync-crds.sh

.PHONY: verify-generated
verify-generated: generate manifests ## Fail if generated files are out of date
	@if ! git diff --quiet -- $(SRC)/api deploy/crds $(CHART)/templates/crd-postgresroute.yaml; then \
		echo "error: generated files are out of date; run 'make generate manifests'" >&2; \
		git --no-pager diff -- $(SRC)/api deploy/crds $(CHART)/templates/crd-postgresroute.yaml; \
		exit 1; \
	fi

# ---- Go ---------------------------------------------------------------------

.PHONY: tidy
tidy: ## Tidy the Go module
	cd $(SRC) && go mod tidy

.PHONY: fmt
fmt: ## Format the Go sources
	cd $(SRC) && gofmt -w .

.PHONY: vet
vet: ## Run go vet
	cd $(SRC) && go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) ## Run golangci-lint
	cd $(SRC) && $(GOLANGCI_LINT) run

.PHONY: test
test: ## Run the unit tests
	cd $(SRC) && go test -race -count=1 ./...

.PHONY: test-integration
test-integration: ## Run the integration tests against a throwaway PostgreSQL
	@docker rm -f pg-k8s-proxy-it >/dev/null 2>&1 || true
	docker run -d --name pg-k8s-proxy-it -p 55432:5432 \
		-e POSTGRES_DB=billing -e POSTGRES_USER=postgres \
		-e POSTGRES_PASSWORD=not-a-real-password postgres:17-alpine
	@echo "waiting for PostgreSQL..."
	@until docker exec pg-k8s-proxy-it pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	cd $(SRC) && PGPROXY_TEST_BACKEND=127.0.0.1:55432 \
		PGPROXY_TEST_PASSWORD=not-a-real-password \
		go test -tags=integration -count=1 -v -timeout=5m ./internal/proxy/ \
		|| { docker rm -f pg-k8s-proxy-it >/dev/null; exit 1; }
	@docker rm -f pg-k8s-proxy-it >/dev/null

.PHONY: cover
cover: ## Run the tests and open the coverage report
	cd $(SRC) && go test -race -count=1 -coverprofile=coverage.out ./... && go tool cover -html=coverage.out

.PHONY: build
build: ## Build the binary into ./bin
	cd $(SRC) && CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BIN)/pg-k8s-proxy ./cmd/pg-k8s-proxy

.PHONY: run
run: ## Run the gateway against the current kubeconfig context
	cd $(SRC) && go run ./cmd/pg-k8s-proxy \
		--leader-elect=false --log-format=text --log-level=debug \
		--proxy-bind-address=:15432 --health-bind-address=:18080 --metrics-bind-address=:19090

# ---- Container --------------------------------------------------------------

.PHONY: image
image: ## Build the container image for the host platform
	docker build -f docker/Dockerfile -t $(IMAGE) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) .

.PHONY: image-push
image-push: ## Build and push a multi-architecture image
	docker buildx build -f docker/Dockerfile -t $(IMAGE) \
		--platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--push .

# ---- Chart ------------------------------------------------------------------

.PHONY: chart-lint
chart-lint: ## Lint the Helm chart in both scopes
	helm lint $(CHART)
	helm lint $(CHART) --set scope.type=Namespaced --set 'scope.namespaces={apps,databases}'

.PHONY: chart-template
chart-template: ## Render the chart to stdout
	helm template $(RELEASE) $(CHART) -n $(NAMESPACE)

.PHONY: chart-package
chart-package: ## Package the chart into ./dist
	@mkdir -p dist
	helm package $(CHART) -d dist

# ---- Deployment -------------------------------------------------------------

.PHONY: install
install: ## Install or upgrade the release, cluster-wide
	helm upgrade --install $(RELEASE) $(CHART) \
		-n $(NAMESPACE) --create-namespace \
		--set image.repository=$(IMAGE_REGISTRY)/$(IMAGE_NAME) \
		--set image.tag=$(IMAGE_TAG) \
		--wait

.PHONY: install-namespaced
install-namespaced: ## Install or upgrade the release confined to one namespace
	helm upgrade --install $(RELEASE) $(CHART) \
		-n $(NAMESPACE) --create-namespace \
		--set scope.type=Namespaced \
		--set image.repository=$(IMAGE_REGISTRY)/$(IMAGE_NAME) \
		--set image.tag=$(IMAGE_TAG) \
		--wait

.PHONY: uninstall
uninstall: ## Uninstall the release
	helm uninstall $(RELEASE) -n $(NAMESPACE) --ignore-not-found

.PHONY: examples
examples: ## Apply the example PostgreSQL instances and routes
	kubectl apply -f examples/

.PHONY: logs
logs: ## Follow the gateway logs
	kubectl logs -n $(NAMESPACE) -l app.kubernetes.io/name=pg-k8s-proxy -f --max-log-requests=10

.PHONY: routes
routes: ## List every route and its status
	kubectl get postgresroutes -A

.PHONY: status
status: ## Show the release status
	helm status $(RELEASE) -n $(NAMESPACE)
	kubectl get pods,svc -n $(NAMESPACE) -l app.kubernetes.io/name=pg-k8s-proxy

.PHONY: port-forward
port-forward: ## Forward the gateway to localhost:5432
	kubectl port-forward -n $(NAMESPACE) svc/$(RELEASE) 5432:5432

# ---- Aggregates -------------------------------------------------------------

.PHONY: check
check: fmt vet lint test chart-lint ## Run every check

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN) dist $(SRC)/coverage.out
