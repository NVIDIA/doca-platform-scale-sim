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

# Apply a passthrough BFB/flavor/DPUSet/service-chain and a matching SimFleet.
# Usage, locally or on a host with cluster access:
#   VM_IMAGE=... BFB_URL=... ./install-passthrough-simfleet.sh
#   ssh <host> "sudo VM_IMAGE=... BFB_URL=... bash -s" < install-passthrough-simfleet.sh
#
# Streamed over SSH, the script applies its inline copy of
# passthrough-simfleet.yaml, so the manifest does not need to be on the host.
#
# Override DPU_CLUSTER_NAME if the Kamaji DPUCluster is not dpu-cplane-tenant1.
# Override FLEET_ID to install more than one fleet in the same namespace.

set -euo pipefail

DPU_CLUSTER_NAME="${DPU_CLUSTER_NAME:-dpu-cplane-tenant1}"
DPU_CLUSTER_NS="${DPU_CLUSTER_NS:-dpu-cplane-tenant1}"
REPLICAS="${REPLICAS:-2}"
FLEET_ID="${FLEET_ID:-passthrough}"
: "${VM_IMAGE:?set VM_IMAGE to a full simfleet-node image reference, pinned by tag or digest}"
: "${BFB_URL:?set BFB_URL to a BFB that DPF can download from inside the cluster}"

# Both are written into YAML as-is, and awk -v expands backslash escapes.
unsafe_chars='[[:space:]"\]'
if [[ "${VM_IMAGE}" =~ ${unsafe_chars} ]]; then
	echo "VM_IMAGE must not contain whitespace, quotes, or backslashes: ${VM_IMAGE}" >&2
	exit 1
fi
if [[ ! "${BFB_URL}" =~ ^https?:// || "${BFB_URL}" =~ ${unsafe_chars} ]]; then
	echo "BFB_URL must be an http(s) URL without whitespace, quotes, or backslashes: ${BFB_URL}" >&2
	exit 1
fi

if [[ ! "${FLEET_ID}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
	echo "FLEET_ID must be a lowercase DNS-1123 label: ${FLEET_ID}" >&2
	exit 1
fi
if [[ ! "${REPLICAS}" =~ ^[0-9]+$ ]]; then
	echo "REPLICAS must be a non-negative integer: ${REPLICAS}" >&2
	exit 1
fi
if [[ ! "${DPU_CLUSTER_NAME}" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]]; then
	echo "DPU_CLUSTER_NAME must be a lowercase DNS-1123 subdomain: ${DPU_CLUSTER_NAME}" >&2
	exit 1
fi
if [[ ! "${DPU_CLUSTER_NS}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
	echo "DPU_CLUSTER_NS must be a lowercase DNS-1123 label: ${DPU_CLUSTER_NS}" >&2
	exit 1
fi

render_manifest() {
	awk -v image="${VM_IMAGE}" -v bfb_url="${BFB_URL}" -v fleet_id="${FLEET_ID}" \
		-v replicas="${REPLICAS}" \
		-v cluster_name="${DPU_CLUSTER_NAME}" -v cluster_ns="${DPU_CLUSTER_NS}" '
		{
			lines[NR] = $0
		}
		$0 == "    image: example.invalid/simfleet-node:set-VM_IMAGE" {
			lines[NR] = "    image: " image
			image_replacements++
		}
		$0 == "  url: https://example.invalid/set-BFB_URL.bfb" {
			lines[NR] = "  url: \"" bfb_url "\""
			bfb_replacements++
		}
		$0 == "  replicas: 2" {
			lines[NR] = "  replicas: " replicas
			replicas_replacements++
		}
		$0 == "      name: dpu-cplane-tenant1" {
			lines[NR] = "      name: " cluster_name
			cluster_name_replacements++
		}
		$0 == "      namespace: dpu-cplane-tenant1" {
			lines[NR] = "      namespace: " cluster_ns
			cluster_ns_replacements++
		}
		{
			n = gsub("set-FLEET_ID", fleet_id, lines[NR])
			fleet_replacements += n
		}
		END {
			if (image_replacements != 1 || bfb_replacements != 1 || fleet_replacements != 4 ||
				replicas_replacements != 1 || cluster_name_replacements != 1 ||
				cluster_ns_replacements != 1) {
				exit 1
			}
			for (line = 1; line <= NR; line++) {
				print lines[line]
			}
		}
	' "$@"
}

echo "Waiting for DPF and SimFleet CRDs..."
kubectl wait --for=condition=Established \
	crd/bfbs.provisioning.dpu.nvidia.com \
	crd/dpuflavors.provisioning.dpu.nvidia.com \
	crd/dpusets.provisioning.dpu.nvidia.com \
	crd/dpuservicechains.svc.dpu.nvidia.com \
	crd/dpuserviceinterfaces.svc.dpu.nvidia.com \
	crd/simfleets.scale.dpu.nvidia.com \
	--timeout=5m

echo "Applying passthrough DPUSet + SimFleet..."
# Streamed on stdin there is no script path, and dirname "" would resolve to
# the working directory, so only a script run from a file looks next to itself.
SCRIPT_DIR=""
if [[ -n "${BASH_SOURCE[0]:-}" && -f "${BASH_SOURCE[0]}" ]]; then
	SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fi
if [[ -n "${SCRIPT_DIR}" && -f "${SCRIPT_DIR}/passthrough-simfleet.yaml" ]]; then
	render_manifest "${SCRIPT_DIR}/passthrough-simfleet.yaml" \
		| kubectl apply -f -
else
	render_manifest << 'MANIFEST' | kubectl apply -f -
---
# The BMC password DPF authenticates to every mock BMC with. A DPUDevice that
# names no bmcCredentialSecretName, as this fleet's do, sends DPF to the shared
# Secret "bmc-shared-password" in the device's own namespace. Without it DPF
# stops before the first Redfish call and the DPUs wait in Initializing.
#
# The value is arbitrary. The mock accepts every basic-auth login and keeps no
# password state, so this only has to be present and non-empty; it is a
# non-default password so the flow takes the ordinary path rather than DPF's
# factory-default handling.
apiVersion: v1
kind: Secret
metadata:
  name: bmc-shared-password
  namespace: dpf-operator-system
type: Opaque
stringData:
  password: simfleet-lab-bmc
---
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: BFB
metadata:
  name: passthrough
  namespace: dpf-operator-system
spec:
  # DPF downloads this from inside the cluster. The guest mock BMC only reads
  # the bundle's headers and software inventory, so any BFB DPF can fetch
  # works; install-passthrough-simfleet.sh renders BFB_URL here.
  url: https://example.invalid/set-BFB_URL.bfb
---
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUFlavor
metadata:
  # Keep spec identical to DPF's zero-trust passthrough use case flavor
  # (docs/public/user-guides/zero-trust/use-cases/passthrough in doca-platform).
  name: passthrough
  namespace: dpf-operator-system
spec:
  grub:
    kernelParameters:
    - console=hvc0
    - console=ttyAMA0
    - earlycon=pl011,0x13010000
    - net.ifnames=0
    - biosdevname=0
    - iommu.passthrough=1
    - cgroup_no_v1=net_prio,net_cls
    - hugepagesz=2048kB
    - hugepages=250
  nvconfig:
  - device: "*"
    parameters:
    - PF_BAR2_ENABLE=0
    - PER_PF_NUM_SF=1
    - PF_TOTAL_SF=20
    - PF_SF_BAR_SIZE=10
    - NUM_PF_MSIX_VALID=0
    - PF_NUM_PF_MSIX_VALID=1
    - PF_NUM_PF_MSIX=228
    - INTERNAL_CPU_MODEL=1
    - INTERNAL_CPU_OFFLOAD_ENGINE=0
    - SRIOV_EN=1
    - NUM_OF_VFS=46
    - LAG_RESOURCE_ALLOCATION=1
    - LINK_TYPE_P1=ETH
    - LINK_TYPE_P2=ETH
  scalableFunctions:
  - count: 20
    device: "*"  # alternatively device: p0
    poolName: bf_sf
  ovs:
    rawConfigScript: |
      _ovs-vsctl() {
        ovs-vsctl --timeout 15 "$@"
      }

      # Remove default OVS configuration on the DPU and ensure no leftovers on the OVS kernel side
      _ovs-vsctl --if-exists del-br ovsbr1
      _ovs-vsctl --if-exists del-br ovsbr2
      ovs-appctl --timeout 15 dpctl/del-dp system@ovs-system || true

      _ovs-vsctl set Open_vSwitch . other_config:doca-init=true
      _ovs-vsctl set Open_vSwitch . other_config:dpdk-max-memzones=50000
      _ovs-vsctl set Open_vSwitch . other_config:hw-offload=true
      _ovs-vsctl set Open_vSwitch . other_config:pmd-quiet-idle=true
      _ovs-vsctl set Open_vSwitch . other_config:max-idle=20000
      _ovs-vsctl set Open_vSwitch . other_config:max-revalidator=5000
      _ovs-vsctl set Open_vSwitch . other_config:doca-congestion-threshold=60
      _ovs-vsctl set Open_vSwitch . other_config:flow-limit=500000
      _ovs-vsctl set Open_vSwitch . other_config:hw-offload-ct-unidir-udp-enabled=true
      _ovs-vsctl remove Open_vSwitch . other_config default-datapath-type || true

      if systemctl list-unit-files openvswitch-switch.service &>/dev/null; then
        systemctl restart openvswitch-switch
      elif systemctl list-unit-files openvswitch.service &>/dev/null; then
        systemctl restart openvswitch
      fi
      _ovs-vsctl --may-exist add-br br-sfc
      _ovs-vsctl set bridge br-sfc datapath_type=netdev
      _ovs-vsctl set bridge br-sfc fail_mode=secure
      _ovs-vsctl --may-exist add-port br-sfc p0
      _ovs-vsctl set Interface p0 type=dpdk
      _ovs-vsctl set Interface p0 mtu_request=9216
      _ovs-vsctl set Port p0 external_ids:dpf-type=physical

  bfcfgParameters:
  - UPDATE_ATF_UEFI=yes
  - UPDATE_DPU_OS=yes
  - WITH_NIC_FW_UPDATE=yes

  configFiles:
  - path: /etc/mellanox/mlnx-bf.conf
    operation: override
    raw: |
        ALLOW_SHARED_RQ="no"
        IPSEC_FULL_OFFLOAD="no"
        ENABLE_ESWITCH_MULTIPORT="yes"
    permissions: "0644"
  - path: /etc/mellanox/mlnx-ovs.conf
    operation: override
    raw: |
        CREATE_OVS_BRIDGES="no"
        OVS_DOCA="yes"
    permissions: "0644"
  - path: /etc/mellanox/mlnx-sf.conf
    operation: override
    raw: ""
    permissions: "0644"
---
apiVersion: svc.dpu.nvidia.com/v1alpha1
kind: DPUServiceChain
metadata:
  name: passthrough
  namespace: dpf-operator-system
spec:
  template:
    spec:
      template:
        spec:
          switches:
            - ports:
              - serviceInterface:
                  matchLabels:
                    interface: p0
              - serviceInterface:
                  matchLabels:
                    interface: pf0hpf
---
apiVersion: provisioning.dpu.nvidia.com/v1alpha1
kind: DPUSet
metadata:
  name: set-FLEET_ID
  namespace: dpf-operator-system
spec:
  strategy:
    type: OnDelete
  dpuNodeSelector:
    matchLabels:
      feature.node.kubernetes.io/dpu-enabled: "true"
  dpuDeviceSelector:
    matchLabels:
      scale.dpu.nvidia.com/pool: set-FLEET_ID
  dpuTemplate:
    spec:
      dpuFlavor: passthrough
      bfb:
        name: passthrough
      nodeEffect:
        applyOnLabelChange: false
        noEffect: true
        force: false
---
apiVersion: svc.dpu.nvidia.com/v1alpha1
kind: DPUServiceInterface
metadata:
  name: p0
  namespace: dpf-operator-system
spec:
  template:
    spec:
      template:
        metadata:
          labels:
            interface: "p0"
        spec:
          interfaceType: physical
          physical:
            interfaceName: p0
---
apiVersion: svc.dpu.nvidia.com/v1alpha1
kind: DPUServiceInterface
metadata:
  name: pf0hpf
  namespace: dpf-operator-system
spec:
  template:
    spec:
      template:
        metadata:
          labels:
            interface: "pf0hpf"
        spec:
          interfaceType: pf
          pf:
            pfID: 0
---
apiVersion: scale.dpu.nvidia.com/v1alpha1
kind: SimFleet
metadata:
  name: set-FLEET_ID-fleet
  namespace: dpf-operator-system
spec:
  replicas: 2
  devicesPerDPUNode: 1
  deviceSelectionLabels:
    scale.dpu.nvidia.com/pool: set-FLEET_ID
  bootstrap:
    # DPF's Kamaji cluster manager keeps the DPUCluster and its admin kubeconfig
    # Secret in the tenant namespace, not in dpf-operator-system.
    # install-passthrough-simfleet.sh renders REPLICAS, DPU_CLUSTER_NAME and
    # DPU_CLUSTER_NS over replicas and this reference.
    dpuClusterRef:
      name: dpu-cplane-tenant1
      namespace: dpu-cplane-tenant1
  virtualMachineTemplate:
    image: example.invalid/simfleet-node:set-VM_IMAGE
    runStrategy: Always
    resources:
      requests:
        cpu: "2"
        memory: 4Gi
    storage:
      ephemeral: true
    podNetwork:
      bridge: true
  dpuNodeTemplate:
    metadata:
      labels:
        feature.node.kubernetes.io/dpu-enabled: "true"
    spec:
      nodeRebootMethod:
        external: {}
  dpuDeviceTemplate:
    spec:
      numberOfPFs: 1
MANIFEST
fi

echo "DPUCluster (must already exist):"
kubectl get dpucluster -n "${DPU_CLUSTER_NS}" "${DPU_CLUSTER_NAME}" || true
kubectl -n dpf-operator-system get bfb,dpuflavor,dpuset,dpuservicechain,dpuserviceinterface,simfleet
