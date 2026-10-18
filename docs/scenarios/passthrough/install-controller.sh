#!/usr/bin/env bash
# Install or upgrade the SimFleet controller Helm chart.
# Usage, locally or on a host with cluster access:
#   CHART_VERSION=... ./install-controller.sh
#   ssh <host> "sudo CHART_VERSION=... bash -s" < install-controller.sh
#
# The chart carries the SimFleet CRDs as templates, so an upgrade updates them.

set -euo pipefail

CHART="${CHART:-oci://ghcr.io/nvidia/doca-platform-scale-sim/charts/simfleet-controller}"
: "${CHART_VERSION:?set CHART_VERSION to a simfleet-controller chart release}"

if ! command -v helm >/dev/null; then
	curl -fsSL https://get.helm.sh/helm-v3.18.3-linux-amd64.tar.gz |
		tar -xzf - -C /usr/local/bin --strip-components=1 linux-amd64/helm
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
