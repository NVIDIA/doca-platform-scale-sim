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

set -o nounset
set -o pipefail
set -o errexit

KUBECTL_BIN="${KUBECTL:-kubectl}"
KUBEVIRT_VERSION="${KUBEVIRT_VERSION:-v1.9.0}"
KUBEVIRT_RELEASE_URL="${KUBEVIRT_RELEASE_URL:-https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}}"
KIND_CLUSTER="${KIND_CLUSTER:-simfleet-controller-test-e2e}"

echo "Installing KubeVirt ${KUBEVIRT_VERSION}..."
"${KUBECTL_BIN}" apply -f "${KUBEVIRT_RELEASE_URL}/kubevirt-operator.yaml"
# virt-operator, virt-controller and virt-exportproxy run two replicas of
# 275-450Mi requests each, which a single node gains nothing from and a 2-vCPU,
# 8 GB runner cannot spare next to the guests. infra.replicas covers every
# deployment the operator creates; the operator itself is scaled here.
"${KUBECTL_BIN}" -n kubevirt scale deployment virt-operator --replicas=1
"${KUBECTL_BIN}" apply -f "${KUBEVIRT_RELEASE_URL}/kubevirt-cr.yaml"
"${KUBECTL_BIN}" patch kubevirt kubevirt -n kubevirt --type merge -p '{"spec":{"infra":{"replicas":1}}}'

"${KUBECTL_BIN}" wait --for=condition=Available \
	kubevirt.kubevirt.io/kubevirt \
	--namespace kubevirt \
	--timeout 10m

# KubeVirt 1.9 enables Beta ImageVolume by default. That path needs kubelet
# ImageVolume (GA in Kubernetes 1.35). Disable it so containerDisk uses the
# init-container binary at /usr/bin/container-disk.
# Kind on machines without nested KVM (typical Docker Desktop) must emulate.
# The e2e suite reads this back: arm64 guests cannot boot under emulation
# because KubeVirt forces host-passthrough, which needs a kvm domain.
# Do not set cpuModel; qemu64 is not advertised on host-model nodes.
if docker exec "${KIND_CLUSTER}-control-plane" test -e /dev/kvm; then
	dev_cfg='{"disabledFeatureGates":["ImageVolume"],"useEmulation":false}'
else
	echo "No /dev/kvm in the Kind node; enabling KubeVirt software emulation."
	dev_cfg='{"disabledFeatureGates":["ImageVolume"],"useEmulation":true}'
fi
# Bridge binding on the primary pod network is refused unless KubeVirt is told
# to permit it; the guest otherwise falls back to the masquerade NAT address.
kubevirt_patch="$(printf \
	'{"spec":{"configuration":{"developerConfiguration":%s,"network":{"permitBridgeInterfaceOnPodNetwork":true}}}}' \
	"${dev_cfg}")"
"${KUBECTL_BIN}" patch kubevirt kubevirt -n kubevirt --type merge -p "${kubevirt_patch}"
"${KUBECTL_BIN}" -n kubevirt rollout status deploy/virt-controller --timeout 2m

"${KUBECTL_BIN}" label node --overwrite \
	-l node-role.kubernetes.io/control-plane \
	scale.dpu.nvidia.com/vm-pool=simfleet

echo "KubeVirt ${KUBEVIRT_VERSION} is installed."
