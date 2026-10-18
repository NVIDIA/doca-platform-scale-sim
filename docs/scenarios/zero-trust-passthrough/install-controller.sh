#!/usr/bin/env bash

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

# Install or upgrade the SimFleet controller Helm chart.
# Usage, locally or on a host with cluster access:
#   CHART_VERSION=... ./install-controller.sh
#   ssh <host> "sudo CHART_VERSION=... bash -s" < install-controller.sh
#
# The chart carries the SimFleet CRDs as templates, so an upgrade updates them.

set -euo pipefail

CHART="${CHART:-oci://ghcr.io/nvidia/doca-platform-scale-sim/charts/simfleet-controller}"
: "${CHART_VERSION:?set CHART_VERSION to a simfleet-controller chart release}"

HELM_VERSION="v3.18.3"
HELM_LINUX_AMD64_SHA256="6ec85f306dd8fe9eb05c61ba4593182b2afcfefb52f21add3fe043ebbdc48e39"

if ! command -v helm > /dev/null; then
	if [[ "$(uname -s)/$(uname -m)" != "Linux/x86_64" ]]; then
		echo "helm not found; install helm ${HELM_VERSION} first (auto-install covers Linux amd64 only)" >&2
		exit 1
	fi
	helm_tmp="$(mktemp -d)"
	trap 'rm -rf "${helm_tmp}"' EXIT
	curl -fsSL -o "${helm_tmp}/helm.tar.gz" \
		"https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz"
	echo "${HELM_LINUX_AMD64_SHA256}  ${helm_tmp}/helm.tar.gz" | sha256sum -c -
	tar -xzf "${helm_tmp}/helm.tar.gz" -C /usr/local/bin --strip-components=1 linux-amd64/helm
fi
helm version --short

echo "Installing simfleet-controller ${CHART_VERSION} from ${CHART}..."
helm upgrade --install simfleet-controller "${CHART}" \
	--version "${CHART_VERSION}" \
	--namespace simfleet-controller-system \
	--create-namespace \
	--wait --timeout 5m
kubectl wait --for=condition=Established \
	crd/simfleets.scale.dpu.nvidia.com crd/siminstances.scale.dpu.nvidia.com --timeout=2m

kubectl -n simfleet-controller-system get deploy,pods
