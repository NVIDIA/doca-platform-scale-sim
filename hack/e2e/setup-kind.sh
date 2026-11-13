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

CLUSTER_NAME="${KIND_CLUSTER:-simfleet-controller-test-e2e}"
KIND_BIN="${KIND:-kind}"
KIND_KUBERNETES_VERSION="${KIND_KUBERNETES_VERSION:-v1.36.4}"
KIND_NODE_IMAGE="${KIND_NODE_IMAGE:-kindest/node:${KIND_KUBERNETES_VERSION}}"
KUBECTL_BIN="${KUBECTL:-kubectl}"
# Kamaji TenantControlPlane NodePort used by DPF Kind environments. Publishing
# it is opt-in: the e2e suite reaches the tenant control plane in-cluster, and
# a fixed host port collides with other jobs on a shared CI host.
DPUCLUSTER_NODE_PORT="${DPUCLUSTER_NODE_PORT:-}"
KIND_CONFIG_FILE="$(mktemp)"
trap 'rm -f "${KIND_CONFIG_FILE}"' EXIT

EXTRA_PORT_MAPPINGS=""
if [[ -n "${DPUCLUSTER_NODE_PORT}" ]]; then
	EXTRA_PORT_MAPPINGS=$(printf '%s\n' \
		'  extraPortMappings:' \
		"  - containerPort: ${DPUCLUSTER_NODE_PORT}" \
		"    hostPort: ${DPUCLUSTER_NODE_PORT}" \
		'    protocol: TCP')
fi

# hostPath is resolved by the Docker daemon, so this test is only meaningful
# when the daemon is local. Set KIND_MOUNT_KVM=true on a remote daemon that
# does expose /dev/kvm.
KVM_EXTRA_MOUNTS=""
if [[ "${KIND_MOUNT_KVM:-}" == "true" ]] || [[ -e /dev/kvm ]]; then
	KVM_EXTRA_MOUNTS=$(printf '%s\n' \
		'  extraMounts:' \
		'  - hostPath: /dev/kvm' \
		'    containerPath: /dev/kvm')
fi

# A remote daemon (such as a docker:dind service) publishes the API server inside the
# daemon's container, so the default 127.0.0.1 kubeconfig is unreachable from
# this job. Bind the daemon host instead; kind puts that address in the
# kubeconfig and its serving certificate.
KIND_API_SERVER_ADDRESS="${KIND_API_SERVER_ADDRESS:-}"
if [[ -z "${KIND_API_SERVER_ADDRESS}" && "${DOCKER_HOST:-}" == tcp://* ]]; then
	docker_host="${DOCKER_HOST#tcp://}"
	docker_host="${docker_host%%:*}"
	KIND_API_SERVER_ADDRESS="$(getent hosts "${docker_host}" | awk 'NR==1 {print $1}')"
	if [[ -z "${KIND_API_SERVER_ADDRESS}" ]]; then
		echo "Could not resolve the Docker daemon host '${docker_host}'." >&2
		exit 1
	fi
	echo "Remote Docker daemon; binding the API server to ${KIND_API_SERVER_ADDRESS}."
fi

# kind picks a random API server port by binding it locally first, which a
# remote daemon's address cannot satisfy. Pinning the port skips that probe.
KIND_API_SERVER_PORT="${KIND_API_SERVER_PORT:-6443}"

# The pod-runtime e2e nests a DPU cluster inside the host cluster's Pods, and
# the DPU cluster's kamaji service CIDR (10.96.0.0/16) is kind's default service
# CIDR. Both subnets are opt-in so the VM e2e keeps kind's defaults: when a
# variable is empty, its key is left out of the generated config.
KIND_POD_SUBNET="${KIND_POD_SUBNET:-}"
KIND_SERVICE_SUBNET="${KIND_SERVICE_SUBNET:-}"

networking_lines=()
if [[ -n "${KIND_API_SERVER_ADDRESS}" ]]; then
	networking_lines+=(
		"  apiServerAddress: \"${KIND_API_SERVER_ADDRESS}\""
		"  apiServerPort: ${KIND_API_SERVER_PORT}"
	)
fi
if [[ -n "${KIND_POD_SUBNET}" ]]; then
	networking_lines+=("  podSubnet: \"${KIND_POD_SUBNET}\"")
fi
if [[ -n "${KIND_SERVICE_SUBNET}" ]]; then
	networking_lines+=("  serviceSubnet: \"${KIND_SERVICE_SUBNET}\"")
fi

KIND_NETWORKING=""
if ((${#networking_lines[@]} > 0)); then
	KIND_NETWORKING=$(printf '%s\n' 'networking:' "${networking_lines[@]}")
fi

# Docker Desktop defaults the kind bridge to MTU 65535. KubeVirt tap/bridge
# creation requires a normal Ethernet MTU. Recreate the unused kind network
# at 1500 so kindnet copies that MTU into pods.
KIND_NETWORK_MTU="${KIND_NETWORK_MTU:-1500}"
ensure_kind_network_mtu() {
	if docker network inspect kind > /dev/null 2>&1; then
		current="$(docker network inspect kind -f '{{index .Options "com.docker.network.driver.mtu"}}' 2> /dev/null || true)"
		if [[ "${current}" == "${KIND_NETWORK_MTU}" ]]; then
			return 0
		fi
		if "${KIND_BIN}" get clusters 2> /dev/null | grep -q .; then
			echo "Kind docker network MTU is '${current:-unset}', want ${KIND_NETWORK_MTU}; leaving it because a Kind cluster still exists."
			return 0
		fi
		echo "Recreating Kind docker network with MTU ${KIND_NETWORK_MTU} (was '${current:-unset}')."
		# Shared CI hosts may have unrelated containers attached, which makes
		# the removal fail. The existing network is usable there, since only
		# Docker Desktop defaults it to an MTU KubeVirt rejects.
		if ! docker network rm kind; then
			echo "Could not remove the kind network; keeping it as it is."
			return 0
		fi
	fi
	docker network create -d bridge \
		-o "com.docker.network.driver.mtu=${KIND_NETWORK_MTU}" \
		kind
}

ensure_kind_network_mtu

# kindnet wires pods with the ptp CNI plugin, which is pure routing: inside an
# ordinary pod it deletes the on-link route for the pod subnet and sends even
# same-subnet traffic to the gateway, and nothing on the node answers ARP for a
# pod address. A KubeVirt bridge-bound guest never gets that route hack. It
# configures itself from KubeVirt's DHCP, which hands out the pod address with
# the subnet's own /24, so it believes every pod is on-link and ARPs for it
# directly. Nobody replies, so the guest's answers are never sent and every
# connection from a pod into the guest hangs, which is what DPF's Redfish
# client sees when it dials a mock BMC. Proxy ARP makes the node answer for the
# pod addresses it routes, which is exactly what the ptp route hack spares an
# ordinary pod from needing. The `all` key is OR'd with the per-interface one,
# so this also covers the veths of virt-launcher pods created later.
enable_pod_proxy_arp() {
	local node
	for node in $("${KIND_BIN}" get nodes --name "${CLUSTER_NAME}"); do
		# Written through /proc rather than sysctl(8), which the node image
		# does not ship.
		docker exec "${node}" sh -c 'echo 1 >/proc/sys/net/ipv4/conf/all/proxy_arp'
		echo "Enabled proxy ARP on ${node}."
	done
}

if "${KIND_BIN}" get clusters | grep -qx "${CLUSTER_NAME}"; then
	echo "Kind cluster '${CLUSTER_NAME}' already exists. Skipping creation."
	# A reused cluster is a fresh container only for the first job on the host.
	enable_pod_proxy_arp
	exit 0
fi

cat > "${KIND_CONFIG_FILE}" << EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ${CLUSTER_NAME}
${KIND_NETWORKING}
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: InitConfiguration
    nodeRegistration:
      kubeletExtraArgs:
        node-labels: "ingress-ready=true"
${EXTRA_PORT_MAPPINGS}
  image: ${KIND_NODE_IMAGE}
${KVM_EXTRA_MOUNTS}
EOF

echo "Creating Kind cluster '${CLUSTER_NAME}'..."
"${KIND_BIN}" create cluster --config="${KIND_CONFIG_FILE}"

enable_pod_proxy_arp

# Kind ships a local-path provisioner. DPF prereqs install their own.
"${KUBECTL_BIN}" delete ns local-path-storage --ignore-not-found
"${KUBECTL_BIN}" delete sc standard --ignore-not-found

# Each CoreDNS replica requests 100m, and the suite has to schedule on a
# 2-vCPU runner.
"${KUBECTL_BIN}" -n kube-system scale deployment coredns --replicas=1

echo "Kind cluster '${CLUSTER_NAME}' is ready."
