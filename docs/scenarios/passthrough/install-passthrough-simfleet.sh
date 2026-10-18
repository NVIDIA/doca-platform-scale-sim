#!/usr/bin/env bash
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

NS="${NS:-dpf-operator-system}"
DPU_CLUSTER_NAME="${DPU_CLUSTER_NAME:-dpu-cplane-tenant1}"
DPU_CLUSTER_NS="${DPU_CLUSTER_NS:-dpu-cplane-tenant1}"
REPLICAS="${REPLICAS:-2}"
FLEET_ID="${FLEET_ID:-passthrough}"
FLEET_NAME="${FLEET_ID}-fleet"
: "${VM_IMAGE:?set VM_IMAGE to a simfleet-node release tag or digest}"
: "${BFB_URL:?set BFB_URL to a BFB that DPF can download from inside the cluster}"

if [[ ! "${FLEET_ID}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
	echo "FLEET_ID must be a lowercase DNS-1123 label: ${FLEET_ID}" >&2
	exit 1
fi

render_manifest() {
	awk -v image="${VM_IMAGE}" -v bfb_url="${BFB_URL}" -v fleet_id="${FLEET_ID}" '
		{
			lines[NR] = $0
		}
		$0 == "    image: example.invalid/simfleet-node:set-VM_IMAGE" {
			lines[NR] = "    image: " image
			image_replacements++
		}
		$0 == "  url: https://example.invalid/set-BFB_URL.bfb" {
			lines[NR] = "  url: " bfb_url
			bfb_replacements++
		}
		{
			n = gsub("set-FLEET_ID", fleet_id, lines[NR])
			fleet_replacements += n
		}
		END {
			if (image_replacements != 1 || bfb_replacements != 1 || fleet_replacements != 4) {
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
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-}")" 2>/dev/null && pwd || true)"
if [[ -n "${SCRIPT_DIR}" && -f "${SCRIPT_DIR}/passthrough-simfleet.yaml" ]]; then
	render_manifest "${SCRIPT_DIR}/passthrough-simfleet.yaml" |
		kubectl apply -f -
else
	render_manifest <<'MANIFEST' | kubectl apply -f -
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
  name: passthrough
  namespace: dpf-operator-system
spec:
  bfcfgParameters:
  - UPDATE_ATF_UEFI=yes
  - UPDATE_DPU_OS=yes
  - WITH_NIC_FW_UPDATE=yes
  configFiles:
  - operation: override
    path: /etc/mellanox/mlnx-bf.conf
    permissions: "0644"
    raw: |
      ALLOW_SHARED_RQ="no"
      IPSEC_FULL_OFFLOAD="no"
      ENABLE_ESWITCH_MULTIPORT="yes"
  - operation: override
    path: /etc/mellanox/mlnx-ovs.conf
    permissions: "0644"
    raw: |
      CREATE_OVS_BRIDGES="no"
      OVS_DOCA="yes"
  - operation: override
    path: /etc/mellanox/mlnx-sf.conf
    permissions: "0644"
    raw: ""
  grub:
    kernelParameters:
    - console=hvc0
    - console=ttyAMA0
    - earlycon=pl011,0x13010000
    - fixrttc
    - net.ifnames=0
    - biosdevname=0
    - iommu.passthrough=1
    - cgroup_no_v1=net_prio,net_cls
    - hugepagesz=2048kB
    - hugepages=1024
  nvconfig:
  - device: "*"
    parameters:
    - PF_BAR2_ENABLE=0
    - PER_PF_NUM_SF=1
    - PF_TOTAL_SF=16
    - PF_SF_BAR_SIZE=10
    - INTERNAL_CPU_MODEL=1
    - INTERNAL_CPU_OFFLOAD_ENGINE=0
    - SRIOV_EN=1
    - NUM_OF_VFS=8
    - LAG_RESOURCE_ALLOCATION=1
    - LINK_TYPE_P1=ETH
    - LINK_TYPE_P2=ETH
  ovs:
    rawConfigScript: |
      _ovs-vsctl() {
        ovs-vsctl --no-wait --timeout 15 "$@"
      }

      _ovs-vsctl set Open_vSwitch . other_config:doca-init=true
      _ovs-vsctl set Open_vSwitch . other_config:dpdk-max-memzones=50000
      _ovs-vsctl set Open_vSwitch . other_config:hw-offload=true
      _ovs-vsctl set Open_vSwitch . other_config:pmd-quiet-idle=true
      _ovs-vsctl set Open_vSwitch . other_config:max-idle=20000
      _ovs-vsctl set Open_vSwitch . other_config:max-revalidator=5000
      _ovs-vsctl set Open_vSwitch . other_config:ctl-pipe-size=1024
      _ovs-vsctl --if-exists del-br ovsbr1
      _ovs-vsctl --if-exists del-br ovsbr2
      _ovs-vsctl --may-exist add-br br-sfc
      _ovs-vsctl set bridge br-sfc datapath_type=netdev
      _ovs-vsctl set bridge br-sfc fail_mode=secure
      _ovs-vsctl --may-exist add-port br-sfc p0
      _ovs-vsctl set Interface p0 type=dpdk
      _ovs-vsctl set Port p0 external_ids:dpf-type=physical
      _ovs-vsctl --may-exist add-port br-sfc p1
      _ovs-vsctl set Interface p1 type=dpdk
      _ovs-vsctl set Port p1 external_ids:dpf-type=physical
      echo -e "3tango\n3tango" | passwd root

      oob_mac=$(cat /sys/class/net/oob_net0/address)
      # Do not use EOF as the delimiter. DPF wraps this script in bf.cfg's
      # user-data heredoc (`cat << \EOF`), and mock-dpuagent ends that heredoc
      # at the first line that is exactly "EOF", which would drop dpuagent.conf.
      cat <<'NETPLAN' > /etc/netplan/99-dpf-comm-ch.yaml
      network:
          renderer: networkd
          version: 2
          ethernets:
            pf0vf0: {}
          bridges:
            br-comm-ch:
              macaddress:
              dhcp4: yes
              interfaces:
                - pf0vf0
      NETPLAN
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

kubectl patch simfleet "${FLEET_NAME}" -n "${NS}" --type merge -p "{
  \"spec\": {
    \"replicas\": ${REPLICAS},
    \"bootstrap\": {
      \"dpuClusterRef\": {
        \"name\": \"${DPU_CLUSTER_NAME}\",
        \"namespace\": \"${DPU_CLUSTER_NS}\"
      }
    }
  }
}"

echo "DPUCluster (must already exist):"
kubectl get dpucluster -n "${DPU_CLUSTER_NS}" "${DPU_CLUSTER_NAME}" || true
kubectl -n "${NS}" get bfb,dpuflavor,dpuset,dpuservicechain,dpuserviceinterface,simfleet
