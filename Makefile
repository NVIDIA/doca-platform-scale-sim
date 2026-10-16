# Image and Helm chart publish to GitHub Container Registry.
REGISTRY ?= ghcr.io/nvidia/doca-platform-scale-sim
VERSION ?= 0.1.0
TAG ?= v$(VERSION)
APP_VERSION ?= $(TAG)
IMG ?= $(REGISTRY)/simfleet-controller:$(TAG)
HELM_REGISTRY ?= oci://$(REGISTRY)/charts
CHART_DIR ?= charts/simfleet-controller
DIST_DIR ?= dist
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" crd paths="./..." output:crd:artifacts:config=config/crd/bases
	"$(CONTROLLER_GEN)" rbac:roleName=simfleet-controller-manager-role paths="./..." output:rbac:dir=$(CHART_DIR)/templates
	# CHART_DIR is a copy of the chart during a release. Clear the CRDs it was
	# copied with so a CRD that was renamed or removed cannot be packaged
	# alongside its replacement.
	mkdir -p $(CHART_DIR)/templates/crds
	rm -f $(CHART_DIR)/templates/crds/*.yaml
	cp config/crd/bases/*.yaml $(CHART_DIR)/templates/crds/

.PHONY: generate
generate: controller-gen ## Generate DeepCopy, DeepCopyInto, and DeepCopyObject implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	# The consistency package asserts facts about files the go test cache does
	# not track, so a cached pass would survive a drift it exists to catch.
	go test -count=1 ./test/consistency/
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $$(go list ./... | grep -v -e /e2e -e /test/consistency) -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	"$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

.PHONY: lint-helm
lint-helm: helm ## Run helm lint for the Helm chart.
	"$(HELM)" lint $(CHART_DIR)

.PHONY: generate-docs-helm
generate-docs-helm: helm-docs ## Generate the Helm chart README from the values.yaml comments.
	"$(HELM_DOCS)" --chart-search-root=$(CHART_DIR)

VERIFY_TARGETS ?= generate manifests

.PHONY: verify
verify: $(addprefix verify-,$(VERIFY_TARGETS)) ## Run all verify-* targets

.PHONY: verify-generate
verify-generate: manifests generate fmt generate-docs-helm ## Verify auto-generated code, manifests and docs did not change
	# Use intent-to-add to check for untracked files after generation.
	git add -N .
	@git diff --quiet || { git diff --stat; echo "Please, commit the result of 'make manifests generate fmt generate-docs-helm'"; exit 1; }

ARTIFACTS_DIR ?= artifacts
RENDERED_MANIFESTS_DIR ?= $(ARTIFACTS_DIR)/rendered-manifests
CHECKOV_IMAGE ?= bridgecrew/checkov@sha256:675d68b0c9043041727bccab8318485118d80531700ec55ed266146bb71c34b8 # version 3.2.497

# checkov runs in a container, so this needs a working docker daemon. The image
# tag is rendered with a digest because CKV_K8S_15 and CKV_K8S_43 require one.
.PHONY: verify-manifests
verify-manifests: helm ## Run the checkov checks in .checkov.yaml against the rendered Helm chart.
	mkdir -p $(RENDERED_MANIFESTS_DIR)
	"$(HELM)" template simfleet-controller $(CHART_DIR) --namespace simfleet-controller-system \
		--set controllerManager.image.tag=$(TAG)@sha256:A \
		> $(RENDERED_MANIFESTS_DIR)/simfleet-controller.yaml
	$(CONTAINER_TOOL) run --rm \
		-v "$(abspath $(RENDERED_MANIFESTS_DIR))":/data:ro \
		-v "$(CURDIR)/.checkov.yaml":/config/.checkov.yaml:ro \
		$(CHECKOV_IMAGE) --config-file /config/.checkov.yaml -f /data/simfleet-controller.yaml

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# Build flags to reduce binary size:
# GO_LDFLAGS:
#  -s: strips debug information from the binary
#  -w: strips DWARF symbol table information
#  -extldflags '-static': enables static linking of external libraries
# NOTE: -s and -w make debugging with delve and profiling with pprof impossible.
# Override GO_LDFLAGS= during development/debugging sessions.
GO_LDFLAGS ?= -s -w -extldflags '-static'
# GO_GCFLAGS:
#  -trimpath: removes file system paths from the binary
GO_GCFLAGS ?= -trimpath

GO_VERSION ?= $(shell awk '/^go /{print $$2}' go.mod)
BUILD_IMAGE ?= docker.io/library/golang:$(GO_VERSION)
# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
BASE_IMAGE ?= gcr.io/distroless/static:nonroot

ARCH ?= $(shell go env GOARCH)
# PLATFORMS are the platforms docker-buildx builds and pushes the manager image for.
PLATFORMS ?= linux/amd64,linux/arm64

PROJECT_NAME ?= doca-platform-scale-sim
PROJECT_REPO ?= https://github.com/NVIDIA/doca-platform-scale-sim
FULL_COMMIT ?= $(shell git rev-parse HEAD)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

DOCKER_BUILD_FLAGS = \
	--label=org.opencontainers.image.created=$(DATE) \
	--label=org.opencontainers.image.name=$(PROJECT_NAME) \
	--label=org.opencontainers.image.revision=$(FULL_COMMIT) \
	--label=org.opencontainers.image.version=$(TAG) \
	--label=org.opencontainers.image.source=$(PROJECT_REPO) \
	--provenance=false \
	--progress=plain \
	--build-arg builder_image=$(BUILD_IMAGE) \
	--build-arg base_image=$(BASE_IMAGE) \
	--build-arg ldflags="$(GO_LDFLAGS)" \
	--build-arg gcflags="$(GO_GCFLAGS)" \
	-f Dockerfile \
	-t $(IMG)

# The docker-container driver is required to build for several platforms at once.
.PHONY: docker-buildx-setup
docker-buildx-setup:
	@if ! $(CONTAINER_TOOL) buildx inspect simfleet-controller-builder > /dev/null 2>&1; then \
		echo "Creating buildx builder 'simfleet-controller-builder'..."; \
		$(CONTAINER_TOOL) buildx create --name simfleet-controller-builder --driver docker-container --use --bootstrap > /dev/null; \
	else \
		$(CONTAINER_TOOL) buildx use simfleet-controller-builder > /dev/null 2>&1 || true; \
	fi

.PHONY: docker-build
docker-build: docker-buildx-setup ## Build the manager image for linux/$(ARCH) and load it into the local image store.
	$(CONTAINER_TOOL) buildx build --load --platform=linux/$(ARCH) $(DOCKER_BUILD_FLAGS) .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: docker-buildx
docker-buildx: docker-buildx-setup ## Build and push the manager image for every platform in PLATFORMS.
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) $(DOCKER_BUILD_FLAGS) .

.PHONY: helm-package
helm-package: manifests lint-helm ## Lint, render, and package the Helm chart into DIST_DIR.
	mkdir -p $(DIST_DIR)
	"$(HELM)" template simfleet-controller $(CHART_DIR) --namespace simfleet-controller-system >/dev/null
	"$(HELM)" package $(CHART_DIR) --version $(VERSION) --app-version $(APP_VERSION) --destination $(DIST_DIR)

.PHONY: helm-push
helm-push: ## Package and push the Helm chart to HELM_REGISTRY.
	# Sub-makes rather than prerequisites: as prerequisites, `make -j helm-push`
	# would start the push before the package existed.
	$(MAKE) helm-package
	$(MAKE) helm-push-package

.PHONY: helm-push-package
helm-push-package: helm ## Push an already packaged chart to HELM_REGISTRY (login first).
	"$(HELM)" push $(DIST_DIR)/simfleet-controller-$(VERSION).tgz $(HELM_REGISTRY)

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
HELM ?= $(LOCALBIN)/helm-$(HELM_VERSION)
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
HELM_DOCS ?= $(LOCALBIN)/helm-docs

TOOL_OS ?= $(shell uname -s | tr A-Z a-z)
TOOL_ARCH ?= $(shell uname -m)
KIND_ARCH = $(TOOL_ARCH)
ifeq ($(TOOL_ARCH),x86_64)
KIND_ARCH = amd64
else ifeq ($(TOOL_ARCH),aarch64)
KIND_ARCH = arm64
endif
HELM_ARCH = $(KIND_ARCH)

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.21.0
HELM_VERSION ?= v3.18.3
HELM_DOCS_VERSION ?= v1.14.2

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): $(LOCALBIN)
	curl -fsSL https://get.helm.sh/helm-$(HELM_VERSION)-$(TOOL_OS)-$(HELM_ARCH).tar.gz | \
		tar -xzf - --strip-components=1 -C $(LOCALBIN) $(TOOL_OS)-$(HELM_ARCH)/helm
	mv $(LOCALBIN)/helm $(HELM)
	ln -sfn $(HELM) $(LOCALBIN)/helm
	chmod +x $(HELM)

.PHONY: helm-docs
helm-docs: $(HELM_DOCS) ## Download helm-docs locally if necessary.
$(HELM_DOCS): $(LOCALBIN)
	$(call go-install-tool,$(HELM_DOCS),github.com/norwoodj/helm-docs/cmd/helm-docs,$(HELM_DOCS_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
