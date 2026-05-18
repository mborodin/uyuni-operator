# Image URL to use for building/pushing the operator image
IMG ?= ghcr.io/mborodin/uyuni-operator:dev

# ENVTEST_K8S_VERSION pins the embedded API server used by webhook tests.
# Update when the controller-runtime dependency moves.
ENVTEST_K8S_VERSION = 1.31.0

# Operator namespace for `make deploy`. Kustomize overlay is config/default,
# which sets this same namespace. Override only if you re-namespace the kustomization.
OPERATOR_NAMESPACE ?= uyuni-operator-system

# Container tool. Override with `make CONTAINER_TOOL=podman ...` if needed.
CONTAINER_TOOL ?= docker

# Where to install pinned tool binaries. Out-of-tree so a clean checkout
# is one `rm -rf bin/` away.
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool versions. Bump deliberately; mismatched controller-gen against
## controller-runtime can produce subtly wrong manifests.
CONTROLLER_TOOLS_VERSION ?= v0.16.5
KUSTOMIZE_VERSION ?= v5.4.3
ENVTEST_VERSION ?= release-0.19
GOLANGCI_LINT_VERSION ?= v1.61.0

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
KUSTOMIZE ?= $(LOCALBIN)/kustomize
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint

# Shell setup: bash strict mode + pipefail so generate-then-diff catches errors.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.DEFAULT_GOAL := help

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: generate
generate: controller-gen ## Generate DeepCopy methods from kubebuilder markers.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: manifests
manifests: controller-gen ## Generate CRDs, RBAC, and webhook manifests from markers.
	$(CONTROLLER_GEN) rbac:roleName=manager-role \
		crd webhook \
		paths="./api/..." paths="./internal/controller/..." paths="./internal/webhook/..." \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac \
		output:webhook:artifacts:config=config/webhook

.PHONY: fmt
fmt: ## Run gofmt against the codebase.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against the codebase.
	go vet ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint.
	$(GOLANGCI_LINT) run ./...

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint with --fix.
	$(GOLANGCI_LINT) run --fix ./...

##@ Test

.PHONY: test
test: manifests generate fmt vet test-unit ## Run all fast-feedback tests (unit + validation).

.PHONY: test-unit
test-unit: ## Run unit tests (validation + controller-fake; no envtest).
	go test ./internal/validation/... ./internal/controller/... ./internal/uyuni/... \
		-race -coverprofile=cover.out

.PHONY: test-webhook
test-webhook: manifests generate envtest ## Run webhook integration tests (envtest).
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test ./internal/webhook/... -race -coverprofile=cover-webhook.out

.PHONY: test-all
test-all: test test-webhook ## Run unit + webhook integration tests.

.PHONY: verify
verify: manifests generate fmt vet test-unit ## CI gate: regenerate, format, vet, test. Fails on any diff.
	@git diff --exit-code -- api/ config/ || { \
		echo "ERROR: 'make verify' produced uncommitted changes. Run 'make manifests generate' and commit."; \
		exit 1; \
	}

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build the operator binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run the operator against the current kubectl context.
	go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build the operator container image.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the operator container image.
	$(CONTAINER_TOOL) push $(IMG)

##@ Deployment

.PHONY: install
install: manifests kustomize ## Install CRDs into the current kubectl context.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Remove CRDs from the current kubectl context.
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found=true -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy the operator (CRDs + RBAC + webhook + manager).
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default | kubectl apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy the operator.
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found=true -f -

##@ Tool installation

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Install controller-gen at the pinned version.
$(CONTROLLER_GEN): $(LOCALBIN)
	@test -x $(CONTROLLER_GEN) && $(CONTROLLER_GEN) --version | grep -q $(CONTROLLER_TOOLS_VERSION) || \
		GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Install kustomize at the pinned version.
$(KUSTOMIZE): $(LOCALBIN)
	@test -x $(KUSTOMIZE) && $(KUSTOMIZE) version | grep -q $(KUSTOMIZE_VERSION) || \
		GOBIN=$(LOCALBIN) go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

.PHONY: envtest
envtest: $(ENVTEST) ## Install setup-envtest at the pinned version.
$(ENVTEST): $(LOCALBIN)
	@test -x $(ENVTEST) || \
		GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Install golangci-lint at the pinned version.
$(GOLANGCI_LINT): $(LOCALBIN)
	@test -x $(GOLANGCI_LINT) && $(GOLANGCI_LINT) version | grep -q $(GOLANGCI_LINT_VERSION) || \
		GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

##@ Cleanup

.PHONY: clean
clean: ## Remove generated binaries and coverage output.
	rm -rf bin/ cover.out cover-webhook.out

.PHONY: clean-all
clean-all: clean ## Also remove envtest binary caches.
	rm -rf $(LOCALBIN)
