# Zero-trust passthrough scale scenario

Installs a passthrough DPUSet and a matching SimFleet on DPF enabled kubernetes cluster, so DPF provisions simulated DPUs the way it would provision hardware.

Read the [scenarios guide](../README.md) first. It covers what every scenario
shares: building the guest image, private registries, KubeVirt settings, the
guest mock DPU agent, nested Flannel networking and virtctl.

## Prerequisites

- The [common prerequisites](../README.md#common-prerequisites): DPF,
`kubectl` and `helm` access, a guest image (`VM_IMAGE`) and a BFB
(`BFB_URL`).
- A Kamaji `DPUCluster` (default `dpu-cplane-tenant1` in namespace
`dpu-cplane-tenant1`).

## Running the scripts

Each script runs locally against the current kubeconfig, or streamed to a
host that has cluster access. Streamed, the scripts need nothing else on that
host; the passthrough installer carries an inline copy of
`passthrough-simfleet.yaml`.

```bash
ssh <host> 'sudo bash -s' < docs/scenarios/zero-trust-passthrough/install-kubevirt.sh
```

## Install order

1. **KubeVirt** (`install-kubevirt.sh`) if it is not already Available. The
  script applies operator + CR (v1.9.0 by default), then patches
   `developerConfiguration` as the
   [KubeVirt configuration](../README.md#kubevirt-configuration) section
   describes, choosing hardware KVM or emulation from what the nodes
   advertise.
2. **SimFleet controller** (`install-controller.sh`) from the Helm chart at
  `oci://ghcr.io/nvidia/doca-platform-scale-sim/charts/simfleet-controller`.
   `CHART_VERSION` is required; set `CHART` to install a chart you pushed
   elsewhere with `make helm-push`. From a checkout, `make deploy IMG=...`
   installs the local chart instead. Without `helm` on the path, the script
   installs a checksum-verified helm v3.18.3 on Linux amd64 and stops on
   other platforms.
3. **Passthrough bundle + SimFleet** (`install-passthrough-simfleet.sh`).
  Uses `passthrough-simfleet.yaml` and renders `VM_IMAGE`, `BFB_URL`,
   and `REPLICAS` into the manifest before a single
   apply, so the controller never sees the manifest defaults. `FLEET_ID` (default `passthrough`) is rendered into the DPUSet
   name, the SimFleet name (`${FLEET_ID}-fleet`), and the pool label so two
   fleets in the same namespace do not collide. The BFB, flavor, service
   chain, and `p0`/`pf0hpf` interfaces stay shared. The bundle includes a
   `bmc-shared-password` Secret in `dpf-operator-system`. SimFleet's
   `DPUDevice`s name no per-device credential, so that is the password DPF
   authenticates to every guest mock BMC with. The mock accepts any password,
   but DPF stops before its first Redfish call without the Secret, and the
   DPUs then wait in `Initializing` with their `DPUDevice` reporting no DPU
   type.

   The SimFleet goes in `dpf-operator-system` because that is where DPF
   issued `dpf-ca-trust-bundle` and `dpf-provisioning-issuer`. DPF's Redfish
   client looks both up in the `DPUDevice`'s namespace, and Kubernetes will
   not let a SimFleet own `DPUDevice`s and `DPUNode`s in another namespace.

Overrides for step 3:


| Variable                              | Default                                                   |
| ------------------------------------- | --------------------------------------------------------- |
| `DPU_CLUSTER_NAME` / `DPU_CLUSTER_NS` | `dpu-cplane-tenant1` (only the DPUCluster the script prints) |
| `REPLICAS`                            | `2`                                                       |
| `FLEET_ID`                            | `passthrough`                                             |
| `VM_IMAGE`                            | required: full `simfleet-node` image reference            |
| `BFB_URL`                             | required: an http(s) BFB URL DPF can reach in the cluster |


For example:

```bash
VM_IMAGE=<registry>/simfleet-node@sha256:<digest> \
BFB_URL=http://<server>/bf-bundle.bfb \
REPLICAS=10 \
  docs/scenarios/zero-trust-passthrough/install-passthrough-simfleet.sh
```

Applying `passthrough-simfleet.yaml` directly would apply its placeholders. Use the installer.

## Expected state

```bash
kubectl -n dpf-operator-system get simfleet passthrough-fleet
kubectl -n dpf-operator-system get vm,vmi
kubectl -n dpf-operator-system get dpudevice -o custom-columns=\
NAME:.metadata.name,BMC:.spec.bmcIp,POOL:.metadata.labels.scale\.dpu\.nvidia\.com/pool
```

The fleet should then reach the conditions in
[SimFleet readiness](../README.md#simfleet-readiness). For the networking
and virtctl checks in the guide, use `FLEET_NS=dpf-operator-system` and VM
names such as `passthrough-fleet-dpu-000000`.

## Layout


| File                              | Role                                                          |
| --------------------------------- | ------------------------------------------------------------- |
| `install-kubevirt.sh`             | KubeVirt 1.9 + ImageVolume / KVM patch                        |
| `install-controller.sh`           | Helm install of simfleet-controller and its CRDs              |
| `install-passthrough-simfleet.sh` | Wait for CRDs, render and apply the bundle and SimFleet       |
| `passthrough-simfleet.yaml`       | BMC Secret, BFB, flavor, DPUSet, chain, interfaces, SimFleet  |
| `test.sh`                         | Contract tests for the installers, run by `make test-scripts` |

