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

# mock-ovs is its own Go module, so its OVSDB and OpenFlow dependencies stay out
# of the controller's go.mod. The root Makefile runs test and lint from here.

# Same binary the root Makefile pins. Pass GOLANGCI_LINT when running from the root.
GOLANGCI_LINT ?= $(CURDIR)/../../bin/golangci-lint-v2.12.2
# Written to the root bin/ next to mock-dpuagent, which is what the root
# Makefile and the e2e expect.
MOCK_OVS_OUT ?= $(CURDIR)/../../bin/mock-ovs
GOARCH ?= amd64
# Matches the root Makefile's GO_TEST_ARGS. The root passes its own value down.
GO_TEST_ARGS ?= -race -ldflags=-linkmode=internal

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build mock-ovs into the repository's bin/mock-ovs.
	mkdir -p "$(dir $(MOCK_OVS_OUT))"
	rm -f "$(MOCK_OVS_OUT)"
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -trimpath -o "$(MOCK_OVS_OUT)" .

.PHONY: test
test: ## Run the mock-ovs unit tests.
	go test -count=1 $(GO_TEST_ARGS) ./...

.PHONY: vet
vet: ## Run go vet against the mock-ovs module.
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint against the mock-ovs module, with the root config.
	GOOS=linux "$(GOLANGCI_LINT)" run --timeout 5m --config "$(CURDIR)/../../.golangci.yml"
