#!/usr/bin/env bash
# Install KubeVirt v1.9.0 and configure it for SimFleet guests.
# Usage, locally or on a host with cluster access:
#   ./install-kubevirt.sh
#   ssh <host> 'sudo bash -s' < install-kubevirt.sh

set -euo pipefail

KUBEVIRT_VERSION="${KUBEVIRT_VERSION:-v1.9.0}"
KUBEVIRT_RELEASE_URL="${KUBEVIRT_RELEASE_URL:-https://github.com/kubevirt/kubevirt/releases/download/${KUBEVIRT_VERSION}}"

echo "Installing KubeVirt ${KUBEVIRT_VERSION}..."
kubectl apply -f "${KUBEVIRT_RELEASE_URL}/kubevirt-operator.yaml"
kubectl apply -f "${KUBEVIRT_RELEASE_URL}/kubevirt-cr.yaml"
kubectl wait --for=condition=Available \
	kubevirt.kubevirt.io/kubevirt \
	--namespace kubevirt \
	--timeout=10m

# KubeVirt 1.9 turns Beta gates on by default. ImageVolume needs kubelet
# ImageVolume (GA in Kubernetes 1.35). On 1.33/1.34, virt-launcher fails with
# exec: "/container-disk-binary/usr/bin/container-disk": no such file.
# Empty featureGates does not disable it; use disabledFeatureGates.
# Do not set cpuModel (e.g. qemu64): nodes will not advertise that CPU model.
# jsonpath must use index() — keys like devices.kubevirt.io/kvm cannot use
# dotted field access even with backslash escapes.
kvm_alloc="$(kubectl get nodes -o jsonpath='{range .items[*]}{index .status.allocatable "devices.kubevirt.io/kvm"}{"\n"}{end}')"
if grep -q '[1-9]' <<<"${kvm_alloc}"; then
	echo "KVM allocatable on nodes; using hardware virtualization."
	dev_cfg='{"disabledFeatureGates":["ImageVolume"],"useEmulation":false}'
else
	echo "No KVM allocatable; enabling software emulation."
	dev_cfg='{"disabledFeatureGates":["ImageVolume"],"useEmulation":true}'
fi
# Bridge binding on the primary pod network is refused unless KubeVirt is told
# to permit it; the guest otherwise falls back to the masquerade NAT address.
kubevirt_patch="$(printf \
	'{"spec":{"configuration":{"developerConfiguration":%s,"network":{"permitBridgeInterfaceOnPodNetwork":true}}}}' \
	"${dev_cfg}")"
kubectl patch kubevirt kubevirt -n kubevirt --type merge -p "${kubevirt_patch}"
kubectl -n kubevirt rollout status deploy/virt-controller --timeout=2m

kubectl -n kubevirt get kubevirt,pods
