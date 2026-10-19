# Copyright 2026 NVIDIA.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# REGISTRY defaults to a registry that does not exist, so a push without an
# explicit REGISTRY cannot overwrite published artifacts. Releases publish to
# GitHub Container Registry with REGISTRY=ghcr.io/nvidia/doca-platform-scale-sim.
REGISTRY ?= example.com
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

# Not ./...: controller-gen, unlike the go tool, descends into .cache/, where the
# mock-dpuagent build checks out doca-platform and its CRDs.
CONTROLLER_GEN_PATHS ?= ./api/...;./cmd/...;./internal/...

.PHONY: manifests
manifests: controller-gen ## Generate ClusterRole and CustomResourceDefinition objects.
	"$(CONTROLLER_GEN)" crd paths="$(CONTROLLER_GEN_PATHS)" output:crd:artifacts:config=config/crd/bases
	"$(CONTROLLER_GEN)" rbac:roleName=simfleet-controller-manager-role paths="$(CONTROLLER_GEN_PATHS)" output:rbac:dir=$(CHART_DIR)/templates
	# CHART_DIR is a copy of the chart during a release. Clear the CRDs it was
	# copied with so a CRD that was renamed or removed cannot be packaged
	# alongside its replacement.
	mkdir -p $(CHART_DIR)/templates/crds
	rm -f $(CHART_DIR)/templates/crds/*.yaml
	cp config/crd/bases/*.yaml $(CHART_DIR)/templates/crds/

.PHONY: generate
generate: controller-gen ## Generate DeepCopy, DeepCopyInto, and DeepCopyObject implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="$(CONTROLLER_GEN_PATHS)"

.PHONY: generate-modules
generate-modules: ## Run go mod tidy to update go modules.
	go mod tidy

.PHONY: generate-third-party-notices
generate-third-party-notices: go-licenses ## Regenerate the Go module licenses in THIRD_PARTY_NOTICES.md.
	GO_LICENSES="$(GO_LICENSES)" ./hack/third-party-notices.sh

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

# GO_TEST_ARGS defines additional go test arguments. linkmode=internal links
# with the Go linker, which avoids the macOS linker's "malformed LC_DYSYMTAB"
# warnings under -race; see https://github.com/golang/go/issues/61229.
GO_TEST_ARGS ?= -race -ldflags=-linkmode=internal
TESTPKGS ?= $$(go list ./... | grep -v -e /e2e -e /test/consistency)

.PHONY: test
test: setup-envtest ## Run the unit and envtest tests. `make verify` checks the generated files.
	# The consistency package asserts facts about files the go test cache does
	# not track, so a cached pass would survive a drift it exists to catch.
	go test -count=1 ./test/consistency/
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $(TESTPKGS) $(GO_TEST_ARGS) -coverprofile cover.out

# e2e uses a local Kind cluster and DPF prerequisite charts from
# https://github.com/NVIDIA/doca-platform/tree/public-release-v26.8
# pinned to e343a51b63ee8cddd29f71891455ff6f83e50d94.
# `make test-e2e` also builds and deploys the controller, then deletes the cluster.
KIND_CLUSTER ?= simfleet-controller-test-e2e
KIND_KUBERNETES_VERSION ?= v1.36.4
KIND_NODE_IMAGE ?= kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed
DPF_HELMFILES_REPO ?= https://github.com/NVIDIA/doca-platform.git
DPF_HELMFILES_REF ?= e343a51b63ee8cddd29f71891455ff6f83e50d94
DPF_HELMFILES_DIR ?= $(LOCALBIN)/dpf-helmfiles
HELMFILE_FILE ?= $(DPF_HELMFILES_DIR)/deploy/helmfiles/prereqs.yaml

.PHONY: check-e2e-host
check-e2e-host: ## Verify test-e2e runs on Linux amd64
	@if [ "$(TOOL_OS)" != "linux" ]; then \
		echo "test-e2e requires Linux amd64 (detected OS=$(TOOL_OS)); use the e2e GitHub workflow" >&2; \
		exit 1; \
	fi
	@if [ "$(TOOL_ARCH)" != "x86_64" ]; then \
		echo "test-e2e requires Linux amd64 (detected arch=$(TOOL_ARCH)); use the e2e GitHub workflow" >&2; \
		exit 1; \
	fi

.PHONY: setup-test-e2e
setup-test-e2e: check-e2e-host kind ## Create a Kind cluster for e2e tests if it does not exist
	KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) KIND_KUBERNETES_VERSION=$(KIND_KUBERNETES_VERSION) \
		KIND_NODE_IMAGE=$(KIND_NODE_IMAGE) KUBECTL=$(KUBECTL) ./hack/e2e/setup-kind.sh

.PHONY: fetch-e2e-helmfiles
fetch-e2e-helmfiles: ## Sparse-clone DPF helmfiles from NVIDIA/doca-platform
	DPF_HELMFILES_REPO=$(DPF_HELMFILES_REPO) DPF_HELMFILES_REF=$(DPF_HELMFILES_REF) \
		DPF_HELMFILES_DIR=$(DPF_HELMFILES_DIR) ./hack/e2e/fetch-helmfiles.sh

.PHONY: deploy-e2e-prereqs
deploy-e2e-prereqs: setup-test-e2e fetch-e2e-helmfiles helmfile helm helm-diff helm-git ## Deploy DPF prereqs onto the Kind cluster
	cd "$(dir $(HELMFILE_FILE))" && \
		PATH="$(LOCALBIN):$(PATH)" $(HELMFILE) --helm-binary "$(HELM)" --file "$(notdir $(HELMFILE_FILE))" sync

DPF_HELM_REPO ?= https://helm.ngc.nvidia.com/nvidia/doca
DPF_HELM_CHART ?= dpf-operator
DPF_HELM_VERSION ?= v26.8.0
DPF_OPERATOR_NAMESPACE ?= dpf-operator-system
DPF_OPERATOR_RELEASE ?= dpf-operator
DPF_OPERATOR_CONFIG ?= $(CURDIR)/hack/e2e/dpfoperatorconfig.yaml

.PHONY: deploy-e2e-dpf
deploy-e2e-dpf: deploy-e2e-prereqs helm ## Install the DPF operator Helm chart and apply DPFOperatorConfig
	HELM=$(HELM) KUBECTL=$(KUBECTL) \
		DPF_HELM_REPO=$(DPF_HELM_REPO) DPF_HELM_CHART=$(DPF_HELM_CHART) DPF_HELM_VERSION=$(DPF_HELM_VERSION) \
		DPF_OPERATOR_NAMESPACE=$(DPF_OPERATOR_NAMESPACE) DPF_OPERATOR_RELEASE=$(DPF_OPERATOR_RELEASE) \
		DPF_OPERATOR_CONFIG=$(DPF_OPERATOR_CONFIG) ./hack/e2e/deploy-dpf.sh

# The e2e provisions from a trimmed BF3 BFB served inside the cluster; see
# hack/e2e/bfb-fixtures.yaml. sync-bfb-fixture.sh copies it out of the
# doca-platform revision hack/dpuagent/mock.ref pins and checks it against
# hack/dpuagent/bfb-fixture.sha256. busybox is pinned by digest and never resolved
# by the Kind node, which loads the image from the host daemon.
BFB_FIXTURE_FILE ?= $(LOCALBIN)/bf3-trimmed.bfb
BFB_FIXTURES_MANIFEST ?= $(CURDIR)/hack/e2e/bfb-fixtures.yaml
BFB_FIXTURES_SOURCE_IMAGE ?= busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0
BFB_FIXTURES_IMAGE ?= simfleet-e2e/bfb-fixtures:busybox

.PHONY: deploy-e2e-bfb-fixtures
deploy-e2e-bfb-fixtures: setup-test-e2e ## Serve the trimmed BFB fixture in the Kind cluster
	BFB_FIXTURE_OUT=$(BFB_FIXTURE_FILE) ./hack/dpuagent/sync-bfb-fixture.sh
	KUBECTL=$(KUBECTL) KIND=$(KIND) KIND_CLUSTER=$(KIND_CLUSTER) \
		BFB_FIXTURE_FILE=$(BFB_FIXTURE_FILE) BFB_FIXTURES_MANIFEST=$(BFB_FIXTURES_MANIFEST) \
		BFB_FIXTURES_SOURCE_IMAGE=$(BFB_FIXTURES_SOURCE_IMAGE) BFB_FIXTURES_IMAGE=$(BFB_FIXTURES_IMAGE) \
		./hack/e2e/deploy-bfb-fixtures.sh

KUBEVIRT_VERSION ?= v1.9.0
E2E_IMG ?= example.com/simfleet-controller:e2e
# Not overridable: the suite asserts this exact local tag on the
# VirtualMachine it expects the controller to create. The tag is never
# published; the Kubernetes version lives in an OCI label instead.
override E2E_NODE_IMG := simfleet-node:e2e
NODE_IMAGE_DIR ?= $(CURDIR)/images/simfleet-node
E2E_DPUAGENT_BIN := $(CURDIR)/bin/mock-dpuagent
# Must exceed the suite's own waits: go test panics the process at its timeout,
# which kills the failure diagnostics before Ginkgo can report them. Under
# software emulation those waits are 20m for VirtualMachine Ready plus 20m for
# the guest agent, 2m per instance for a delegated pod address, 5m for the BFB
# download, and 27m for Redfish provisioning and the tenant join; then 6m to
# tear the replaced ordinal down and 27m to bring it back, and 6m to tear the
# scaled-away ordinal down. That is 115m, which leaves the rest ~4m. The 24h
# management token lifetime is the hard ceiling.
E2E_TEST_TIMEOUT ?= 119m

.PHONY: deploy-e2e-kubevirt
deploy-e2e-kubevirt: setup-test-e2e ## Install KubeVirt on the Kind cluster
	KUBECTL=$(KUBECTL) KIND_CLUSTER=$(KIND_CLUSTER) KUBEVIRT_VERSION=$(KUBEVIRT_VERSION) \
		./hack/e2e/deploy-kubevirt.sh

.PHONY: test-e2e
test-e2e: check-e2e-host kind ## Run the e2e tests on Kind with DPF and KubeVirt.
	# Recursive makes ensure cleanup finishes before setup, even under make -j.
	$(MAKE) cleanup-test-e2e
	$(MAKE) test-e2e-run

.PHONY: test-e2e-run
test-e2e-run: check-e2e-host setup-test-e2e deploy-e2e-dpf deploy-e2e-kubevirt deploy-e2e-bfb-fixtures manifests generate fmt vet
	# Cached build: where Docker images persist between runs, an image whose
	# stamped source hash matches this checkout's recipe is reused, and any
	# recipe change rebuilds it. The agent binary is guest content, so it is
	# built first and passed in.
	$(MAKE) dpuagent-build
	$(MAKE) -C $(NODE_IMAGE_DIR) node-image-build-cached NODE_IMG=$(E2E_NODE_IMG) \
		DPUAGENT_BIN=$(E2E_DPUAGENT_BIN)
	$(KIND) load docker-image $(E2E_NODE_IMG) --name $(KIND_CLUSTER)
	$(MAKE) docker-build IMG=$(E2E_IMG)
	$(KIND) load docker-image $(E2E_IMG) --name $(KIND_CLUSTER)
	# Pod Security admission only checks a pod when it is created, so the
	# namespace is labelled before the chart creates the controller pod.
	$(KUBECTL) create namespace simfleet-controller-system --dry-run=client -o yaml | $(KUBECTL) apply -f -
	$(KUBECTL) label --overwrite ns simfleet-controller-system pod-security.kubernetes.io/enforce=restricted
	$(MAKE) deploy IMG=$(E2E_IMG)
	$(KUBECTL) wait --for=condition=Available deployment -l control-plane=controller-manager -n simfleet-controller-system --timeout=180s
	PATH="$(LOCALBIN):$(PATH)" go test -tags=e2e ./test/e2e/ -v -ginkgo.v -timeout $(E2E_TEST_TIMEOUT)
	$(MAKE) cleanup-test-e2e

.PHONY: cleanup-test-e2e
cleanup-test-e2e: kind ## Tear down the Kind cluster used for e2e tests
	$(KIND) delete cluster --name $(KIND_CLUSTER)

# GOOS=linux lints the code the controller image ships, whatever the host OS.
.PHONY: lint
lint: golangci-lint lint-config ## Run golangci-lint linter
	GOOS=linux "$(GOLANGCI_LINT)" run --timeout 5m

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	GOOS=linux "$(GOLANGCI_LINT)" run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	"$(GOLANGCI_LINT)" config verify

.PHONY: lint-helm
lint-helm: helm ## Run helm lint for the Helm chart.
	"$(HELM)" lint $(CHART_DIR)

.PHONY: generate-docs-helm
generate-docs-helm: helm-docs ## Generate the Helm chart README from the values.yaml comments.
	"$(HELM_DOCS)" --chart-search-root=$(CHART_DIR)

VERIFY_TARGETS ?= generate copyright shfmt manifests

.PHONY: verify
verify: $(addprefix verify-,$(VERIFY_TARGETS)) ## Run all verify-* targets

.PHONY: verify-generate
verify-generate: manifests generate generate-modules fmt generate-docs-helm generate-third-party-notices ## Verify auto-generated code, modules and docs did not change
	# Use intent-to-add to check for untracked files after generation.
	git add -N .
	@git diff --quiet || { git diff --stat; echo "Please, commit the result of 'make manifests generate generate-modules fmt generate-docs-helm generate-third-party-notices'"; exit 1; }

# Tracked and untracked files, so a new file is checked before it is committed.
SOURCE_FILES = git ls-files --cached --others --exclude-standard --

.PHONY: verify-copyright
verify-copyright: ## Verify Go files, shell scripts and Makefiles carry the NVIDIA copyright notice
	@missing=$$($(SOURCE_FILES) '*.go' '*.sh' '*Makefile' | xargs grep -L -E 'Copyright [0-9]{4} NVIDIA'); \
	if [ -n "$$missing" ]; then \
		echo "These files lack the copyright notice in hack/boilerplate.go.txt:"; \
		echo "$$missing"; \
		exit 1; \
	fi

.PHONY: fmt-shell
fmt-shell: shfmt ## Format shell scripts with shfmt.
	$(SOURCE_FILES) '*.sh' | xargs "$(SHFMT)" -w -bn -sr

.PHONY: verify-shfmt
verify-shfmt: shfmt ## Verify shell scripts are formatted with shfmt
	@unformatted=$$($(SOURCE_FILES) '*.sh' | xargs "$(SHFMT)" -l -bn -sr); \
	if [ -n "$$unformatted" ]; then \
		echo "These shell scripts need formatting; run 'make fmt-shell':"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

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

# CONFORM_FLAGS defaults to checking the HEAD commit; CI passes a revision
# range to check every commit of a pull request.
CONFORM_FLAGS ?=

.PHONY: commit-check
commit-check: conform ## Run conform to validate commit messages against .conform.yaml.
	"$(CONFORM)" enforce $(CONFORM_FLAGS)

##@ Build

.PHONY: dpuagent-build
dpuagent-build: ## Build pinned mock-dpuagent binary into bin/mock-dpuagent.
	DPUAGENT_OUT="$(E2E_DPUAGENT_BIN)" "$(CURDIR)/hack/dpuagent/build.sh"

.PHONY: dpuagent-build-test
dpuagent-build-test: ## Run dpuagent build contract test.
	bash "$(CURDIR)/hack/dpuagent/build_test.sh"

.PHONY: test-scripts
test-scripts: dpuagent-build-test ## Run the shell contract tests for the hack, node image and scenario scripts.
	bash "$(CURDIR)/hack/dpuagent/sync-bfb-fixture_test.sh"
	bash "$(CURDIR)/docs/scenarios/zero-trust-passthrough/test.sh"
	$(MAKE) -C "$(CURDIR)/images/simfleet-node" test

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
FULL_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
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

##@ Deployment

DEPLOY_NAMESPACE ?= simfleet-controller-system

# IMG must be repository:tag. It is split at the last colon, so a digest
# reference or an untagged image on a registry with a port is split wrongly.
.PHONY: deploy
deploy: manifests helm ## Install or upgrade the Helm chart, running IMG, on the cluster in ~/.kube/config. The chart carries the CRDs.
	img='$(IMG)'; \
	"$(HELM)" upgrade --install simfleet-controller $(CHART_DIR) \
		--namespace $(DEPLOY_NAMESPACE) --create-namespace \
		--set-string controllerManager.image.repository="$${img%:*}" \
		--set-string controllerManager.image.tag="$${img##*:}"

.PHONY: undeploy
undeploy: helm ## Uninstall the Helm chart from the cluster in ~/.kube/config.
	"$(HELM)" uninstall simfleet-controller --namespace $(DEPLOY_NAMESPACE) --ignore-not-found

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
# Each binary is named after its version, so bumping a version names a file
# that does not exist yet and make installs it.
KUBECTL ?= kubectl
KIND ?= $(LOCALBIN)/kind-$(KIND_VERSION)
HELM ?= $(LOCALBIN)/helm-$(HELM_VERSION)
HELMFILE ?= $(LOCALBIN)/helmfile-$(HELMFILE_VERSION)
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)
ENVTEST ?= $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION)
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
HELM_DOCS ?= $(LOCALBIN)/helm-docs-$(HELM_DOCS_VERSION)
SHFMT ?= $(LOCALBIN)/shfmt-$(SHFMT_VERSION)
CONFORM ?= $(LOCALBIN)/conform-$(CONFORM_VERSION)
GO_LICENSES ?= $(LOCALBIN)/go-licenses-$(GO_LICENSES_VERSION)

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
KIND_VERSION ?= v0.33.0
HELM_VERSION ?= v3.18.3
HELMFILE_VERSION ?= v1.4.1
HELM_DIFF_VERSION ?= 3.12.2
HELM_GIT_VERSION ?= 1.4.0
HELM_DOCS_VERSION ?= v1.14.2
SHFMT_VERSION ?= v3.11.0
CONFORM_VERSION ?= v0.1.0-alpha.30
GO_LICENSES_VERSION ?= v2.0.1

# Defined before its first use: ENVTEST names a rule target, and make expands
# rule targets while it parses.
define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef

#ENVTEST_VERSION is the controller-runtime version to use for setup-envtest, derived from go.mod
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v")

#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND): | $(LOCALBIN)
	curl -fsSL -o $(KIND) https://kind.sigs.k8s.io/dl/$(KIND_VERSION)/kind-$(TOOL_OS)-$(KIND_ARCH)
	chmod +x $(KIND)

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): | $(LOCALBIN)
	curl -fsSL https://get.helm.sh/helm-$(HELM_VERSION)-$(TOOL_OS)-$(HELM_ARCH).tar.gz | \
		tar -xzf - --strip-components=1 -C $(LOCALBIN) $(TOOL_OS)-$(HELM_ARCH)/helm
	mv $(LOCALBIN)/helm $(HELM)
	ln -sfn $(HELM) $(LOCALBIN)/helm
	chmod +x $(HELM)

.PHONY: helmfile
helmfile: $(HELMFILE) ## Download helmfile locally if necessary.
$(HELMFILE): | $(LOCALBIN)
	curl -fsSL https://github.com/helmfile/helmfile/releases/download/$(HELMFILE_VERSION)/helmfile_$(subst v,,$(HELMFILE_VERSION))_$(TOOL_OS)_$(HELM_ARCH).tar.gz | \
		tar -xzf - -C $(LOCALBIN) helmfile
	mv $(LOCALBIN)/helmfile $(HELMFILE)
	ln -sfn $(HELMFILE) $(LOCALBIN)/helmfile
	chmod +x $(HELMFILE)

.PHONY: helm-diff
helm-diff: helm ## Install the helm-diff plugin if necessary.
	$(HELM) plugin list | awk '$$1 == "diff" && $$2 == "$(HELM_DIFF_VERSION)" { found = 1 } END { exit !found }' || \
		( ($(HELM) plugin uninstall diff || true) && \
		  $(HELM) plugin install https://github.com/databus23/helm-diff --version $(HELM_DIFF_VERSION) )

.PHONY: helm-git
helm-git: helm ## Install the helm-git plugin if necessary.
	$(HELM) plugin list | awk '$$1 == "helm-git" && $$2 == "$(HELM_GIT_VERSION)" { found = 1 } END { exit !found }' || \
		( ($(HELM) plugin uninstall helm-git || true) && \
		  $(HELM) plugin install https://github.com/aslafy-z/helm-git --version $(HELM_GIT_VERSION) )

.PHONY: helm-docs
helm-docs: $(HELM_DOCS) ## Download helm-docs locally if necessary.
$(HELM_DOCS): | $(LOCALBIN)
	$(call go-install-tool,$(HELM_DOCS),github.com/norwoodj/helm-docs/cmd/helm-docs,$(HELM_DOCS_VERSION))

.PHONY: shfmt
shfmt: $(SHFMT) ## Download shfmt locally if necessary.
$(SHFMT): | $(LOCALBIN)
	$(call go-install-tool,$(SHFMT),mvdan.cc/sh/v3/cmd/shfmt,$(SHFMT_VERSION))

.PHONY: conform
conform: $(CONFORM) ## Download conform locally if necessary.
$(CONFORM): | $(LOCALBIN)
	$(call go-install-tool,$(CONFORM),github.com/siderolabs/conform/cmd/conform,$(CONFORM_VERSION))

.PHONY: go-licenses
go-licenses: $(GO_LICENSES) ## Download go-licenses locally if necessary.
# Not go-install-tool: the module path ends in /v2, which is not the binary's
# name. Built with the local toolchain because hack/third-party-notices.sh
# scans with it, and go-licenses only recognizes its own GOROOT's packages.
$(GO_LICENSES): | $(LOCALBIN)
	GOTOOLCHAIN=local GOBIN="$(LOCALBIN)" go install github.com/google/go-licenses/v2@$(GO_LICENSES_VERSION)
	mv "$(LOCALBIN)/go-licenses" "$(GO_LICENSES)"

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): | $(LOCALBIN)
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
$(ENVTEST): | $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): | $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))
	@test -f .custom-gcl.yml && { \
		echo "Building custom golangci-lint with plugins..." && \
		$(GOLANGCI_LINT) custom --destination $(LOCALBIN) --name golangci-lint-custom && \
		mv -f $(LOCALBIN)/golangci-lint-custom $(GOLANGCI_LINT); \
	} || true

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary, including the version
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(2)")" "$(1)" ;\
}
endef
