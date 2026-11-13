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

CONTAINER_TOOL ?= docker
NODE_IMG ?= simfleet-node:local
# linux/amd64 manifest of quay.io/containerdisks/ubuntu:24.04.
BASE_DIGEST ?= sha256:f3824dc850e0655eae3a6f744ec71a38b216da163636e1d0a724715316515555
BASE_IMAGE ?= quay.io/containerdisks/ubuntu:24.04@$(BASE_DIGEST)
DISK_SIZE ?= 10G
IMAGE_VERSION ?=
# linux/amd64 mock-dpuagent built from the commit in hack/dpuagent/mock.ref.
# Produced by `make dpuagent-build`, which writes
# exactly this path.
DPUAGENT_BIN ?= $(CURDIR)/../../bin/mock-dpuagent

# The node-in-a-pod image. Same architecture as the VM image.
NODE_CONTAINER_IMG ?= simfleet-node-container:local
# lib.sh is the only Kubernetes pin; read it rather than repeat it here.
KUBE_APT_VERSION ?= $(shell bash -c '. "$(CURDIR)/lib.sh" && printf %s "$$SIMFLEET_KUBE_APT_VERSION"')

# Carries libguestfs. The CI hosts cannot run it directly, so the disk stages
# happen in here. Local only; never published.
BUILDER_IMAGE ?= simfleet-node-builder:local

BUILD_ENV = BASE_IMAGE="$(BASE_IMAGE)" NODE_IMG="$(NODE_IMG)" DISK_SIZE="$(DISK_SIZE)" \
	  CONTAINER_TOOL="$(CONTAINER_TOOL)" BUILDER_IMAGE="$(BUILDER_IMAGE)" \
	  IMAGE_VERSION="$(IMAGE_VERSION)" DPUAGENT_BIN="$(DPUAGENT_BIN)"

.PHONY: check-host
check-host:
	CONTAINER_TOOL="$(CONTAINER_TOOL)" BUILDER_IMAGE="$(BUILDER_IMAGE)" ./check-host.sh

.PHONY: builder-image
builder-image: ## Build the libguestfs build host image.
	$(CONTAINER_TOOL) build --platform linux/amd64 -t $(BUILDER_IMAGE) \
	  -f Dockerfile.builder .

.PHONY: node-image-build
node-image-build:
	$(BUILD_ENV) ./build.sh

# Same image, but skipped when an existing one carries this recipe's source
# hash. Pays off only where Docker images persist between runs.
.PHONY: node-image-build-cached
node-image-build-cached:
	$(BUILD_ENV) ./build-cached.sh

# Builds the node-in-a-pod image from the same guest/ files and the same
# mock-dpuagent binary as the VM image. The binary lives outside the build
# context, so it comes in as the "dpuagent" named context.
.PHONY: node-container-image-build
node-container-image-build:
	@test -s "$(DPUAGENT_BIN)" || { \
	  echo "mock-dpuagent binary not found at $(DPUAGENT_BIN)" >&2; \
	  echo "build it with: make dpuagent-build" >&2; \
	  exit 1; }
	@test -n "$(KUBE_APT_VERSION)" || { \
	  echo "could not read SIMFLEET_KUBE_APT_VERSION from lib.sh" >&2; \
	  exit 1; }
	$(CONTAINER_TOOL) build --platform linux/amd64 \
	  --build-arg KUBE_APT_VERSION="$(KUBE_APT_VERSION)" \
	  --build-arg DPUAGENT_FILE="$(notdir $(DPUAGENT_BIN))" \
	  --build-context dpuagent="$(dir $(DPUAGENT_BIN))" \
	  -t $(NODE_CONTAINER_IMG) -f Dockerfile.container .

.PHONY: node-image-source-hash
node-image-source-hash:
	@BASE_IMAGE="$(BASE_IMAGE)" DISK_SIZE="$(DISK_SIZE)" \
	  IMAGE_VERSION="$(IMAGE_VERSION)" DPUAGENT_BIN="$(DPUAGENT_BIN)" \
	  ./source-hash.sh

.PHONY: node-image-push
node-image-push:
	$(CONTAINER_TOOL) push $(NODE_IMG)

.PHONY: test
test:
	./test.sh
