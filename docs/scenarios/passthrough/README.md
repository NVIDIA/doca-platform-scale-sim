# Passthrough scale scenario

Installs a passthrough DPUSet and a matching SimFleet on a real DPF cluster,
so DPF provisions simulated DPUs the way it would provision hardware. This is
not the Kind e2e suite; that lives under `hack/e2e` and `test/e2e`.

## Prerequisites

- DPF already running, with a Kamaji `DPUCluster` (default
  `dpu-cplane-tenant1` in namespace `dpu-cplane-tenant1`).
- The `SimFleet` in `dpf-operator-system`, the same namespace DPF issued
  `dpf-ca-trust-bundle` and `dpf-provisioning-issuer` into. DPF's Redfish
  client looks those up in the `DPUDevice`'s namespace, and Kubernetes will
  not let a `SimFleet` own inventory across namespaces.
- `kubectl` and `helm` access to the cluster, from a workstation or a host
  you can reach over SSH.
- A guest image (`VM_IMAGE`): a `simfleet-node` tag or digest that every node
  able to run a VM can pull. There is no default image. Build and push one
  from a Linux amd64 host with Docker:

  ```bash
  make dpuagent-build
  make -C images/simfleet-node node-image-build node-image-push \
    NODE_IMG=<registry>/simfleet-node:<tag>
  ```

- A BFB (`BFB_URL`) that DPF can download from inside the cluster. There is
  no default. The guest mock BMC only reads the bundle's headers and software
  inventory, so any BFB works: a DOCA BlueField bundle, or the trimmed
  fixture `make deploy-e2e-bfb-fixtures` stages in `bin/bf3-trimmed.bfb`,
  served over HTTP.
- Kubernetes 1.33 / 1.34: the KubeVirt installer disables ImageVolume (see
  below). Hardware KVM is preferred; the script enables TCG emulation only
  when no node advertises `devices.kubevirt.io/kvm`.

## Private guest image registries

SimFleet puts the guest image straight into the VirtualMachine as a
`containerDisk` volume and sets nothing else on it — no `imagePullSecret`, no
`imagePullPolicy`. That is deliberate; the SimFleet API is not growing a
registry-credentials field. The consequence is that the virt-launcher pod
pulls `${VM_IMAGE}` **anonymously**. From a private registry, the VMI sits in
`ContainerCreating` with `ImagePullBackOff` on the `container-disk` container
unless the image is already on the node.

Because the tag is not `:latest`, the effective pull policy is
`IfNotPresent`: an image already in the node's containerd store is used as-is
and never fetched. So pull it once per node, as root, with credentials.

Do this on **every** node a VM can land on. Nothing constrains scheduling
here, so in practice that means every worker that is not cordoned. The image
is several GB, so run them in parallel and expect it to take a while.

```bash
# On each node, as root.
crictl pull --creds "${REGISTRY_USER}:${REGISTRY_TOKEN}" "${VM_IMAGE}"
```

If `crictl` is not configured on the node, `ctr` reaches the same containerd
store, but the namespace matters — kubelet only sees `k8s.io`:

```bash
ctr --namespace k8s.io images pull \
  --user "${REGISTRY_USER}:${REGISTRY_TOKEN}" \
  "${VM_IMAGE}"
```

Driving it over SSH, for every schedulable node:

```bash
for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
  ssh "${node}" "sudo crictl pull --creds '${REGISTRY_USER}:${REGISTRY_TOKEN}' \
    '${VM_IMAGE}'" &
done
wait
```

Verify before applying the SimFleet:

```bash
# Should print the image on every node.
for node in $(kubectl get nodes -o jsonpath='{.items[*].metadata.name}'); do
  echo -n "${node}: "
  ssh "${node}" "sudo crictl images | grep -c simfleet-node" || echo 0
done
```

Pin a tag or a digest; a digest is required for cryptographic immutability.

## Running the scripts

Each script runs locally against the current kubeconfig, or streamed to a
host that has cluster access. Streamed, the scripts need nothing else on that
host; the passthrough installer carries an inline copy of
`passthrough-simfleet.yaml`.

```bash
ssh <host> 'sudo bash -s' < docs/scenarios/passthrough/install-kubevirt.sh
```

Fish has no bash heredocs, so keep `sudo bash -s` and redirect the file, as
above.

## Install order

1. **KubeVirt** (`install-kubevirt.sh`) if it is not already Available. The
   script applies operator + CR (v1.9.0 by default), then patches
   `developerConfiguration`:
   - `disabledFeatureGates: [ImageVolume]` — required on Kubernetes 1.33 and
     1.34. KubeVirt 1.9 turns this Beta gate on by default. An empty
     `featureGates` list does **not** turn it off. Without the disable,
     virt-launcher crash-loops with
     `exec: "/container-disk-binary/usr/bin/container-disk": no such file`.
   - `useEmulation: false` when KVM is allocatable, `true` otherwise.
   - Do not set `cpuModel` (for example `qemu64`). Host-model scheduling
     fails if nodes do not advertise that model.
2. **SimFleet controller** (`install-controller.sh`) from the Helm chart at
   `oci://ghcr.io/nvidia/doca-platform-scale-sim/charts/simfleet-controller`.
   `CHART_VERSION` is required; set `CHART` to install a chart you pushed
   elsewhere with `make helm-push`. From a checkout, `make deploy IMG=...`
   installs the local chart instead. The chart carries the SimFleet CRDs, so
   an upgrade updates them.
3. **Passthrough bundle + SimFleet** (`install-passthrough-simfleet.sh`).
   Uses `passthrough-simfleet.yaml`, renders `VM_IMAGE` and `BFB_URL` into the
   manifest before apply, then patches only replicas and the DPUCluster
   reference. `FLEET_ID` (default `passthrough`) is rendered into the DPUSet
   name, the SimFleet name (`${FLEET_ID}-fleet`), and the pool label so two
   fleets in the same namespace do not collide. The BFB, flavor, service
   chain, and `p0`/`pf0hpf` interfaces stay shared. The bundle includes a
   `bmc-shared-password` Secret in `dpf-operator-system`. SimFleet's
   `DPUDevice`s name no per-device credential, so that is the password DPF
   authenticates to every guest mock BMC with. The mock accepts any password,
   but DPF stops before its first Redfish call without the Secret, and the
   DPUs then wait in `Initializing` with their `DPUDevice` reporting no DPU
   type.

Overrides for step 3:

| Variable                              | Default                                              |
| ------------------------------------- | ---------------------------------------------------- |
| `NS`                                  | `dpf-operator-system`                                |
| `DPU_CLUSTER_NAME` / `DPU_CLUSTER_NS` | `dpu-cplane-tenant1`                                 |
| `REPLICAS`                            | `2`                                                  |
| `FLEET_ID`                            | `passthrough`                                        |
| `VM_IMAGE`                            | required: `simfleet-node` tag or digest              |
| `BFB_URL`                             | required: a BFB DPF can download from in the cluster |

For example:

```bash
VM_IMAGE=<registry>/simfleet-node@sha256:<digest> \
BFB_URL=http://<server>/bf-bundle.bfb \
REPLICAS=10 \
  docs/scenarios/passthrough/install-passthrough-simfleet.sh
```

Applying `passthrough-simfleet.yaml` directly would apply its placeholders;
use the installer.

## Guest mock DPU agent

The `simfleet-node` image bakes `/opt/dpf/mock-dpuagent` (Redfish on **443**
and **80**, plus a fake DPU agent). SimFleet cloud-init writes
`/etc/mock-dpuagent/config.yaml` and a per-ordinal management kubeconfig; a
guest `wait-kubelet-join` helper turns DPF's join Secret into kubelet config
once the DPU exists. The mock service is **not** ordered after the join helper
— DPF cannot create the join Secret until Redfish has already succeeded on the
guest BMC address.

Default `spec.dpuAgent.rebootMethod` is **NoAction**; the DPF reboot controller
is not installed in this scenario (same as Kind e2e). There is no
`provisioning.dpu.nvidia.com/skip-hw-provisioning` label and no hardware
provisioning bypass.

To make every guest BMC answer like a slow real one, set delays when the fleet
is created (`spec.dpuAgent` cannot be changed later):

```yaml
spec:
  dpuAgent:
    bmc:
      responseDelay:
        minSeconds: 0
        maxSeconds: 1
      responseDelayOverrides:
        - name: System
          minSeconds: 1
          maxSeconds: 2
        - name: UpdateService.SimpleUpdate
          minSeconds: 20
          maxSeconds: 30
```

Operation names are those of the pinned mock's Redfish route table; see the
"Redfish response delays" section of the mock-dpuagent README at
`hack/dpuagent/mock.ref`.

## Expected state

```bash
kubectl -n dpf-operator-system get simfleet passthrough-fleet
kubectl -n dpf-operator-system get vm,vmi
kubectl -n dpf-operator-system get dpudevice -o custom-columns=\
NAME:.metadata.name,BMC:.spec.bmcIp,POOL:.metadata.labels.scale\.dpu\.nvidia\.com/pool
```

SimFleet should report `InventoryReady`, `DPUsObserved`, `BootstrapReady`,
and `VirtualMachinesReady` True, with `vmReadyInstances` equal to
`replicas`.

Each `DPUDevice` gains `spec.bmcIP` (the guest's delegated pod-network IPv4)
and the DPUSet pool label only after its VM is running and SimFleet has
verified VMI ownership. DPF then claims the device and drives the normal
Redfish short path against the guest mock. DPU provisioning and tenant Node
Ready can take several minutes after `VirtualMachinesReady`.

## Nested Flannel networking

KubeVirt bridge binding delegates each outer-Flannel pod IP to its VM. Tenant
Flannel must use that primary interface and allow UDP 8472 between VM
addresses.

The inner VXLAN MTU is the delegated interface's MTU minus 50 bytes of outer
VXLAN overhead. The outer MTU is environment-specific: read it off the
delegated interface rather than assuming a number. The commands below assume a
1450-byte outer MTU, which gives an inner MTU of 1400 and a 1372-byte
ping payload (1400 minus 28 bytes of IP and ICMP header). Adjust all three if
your outer MTU differs.

Primary pod-network bridge binding does not support KubeVirt live migration.

Check delegated addresses:

```bash
kubectl -n dpf-operator-system get vmi \
  -o custom-columns=NAME:.metadata.name,IP:.status.interfaces[0].ipAddress
```

Set the tenant kubeconfig path, then check Node InternalIPs and PodCIDRs:

```bash
TENANT_KUBECONFIG=/run/dpf-sim/tenant-admin.conf
kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{end}{"\t"}{.spec.podCIDR}{"\n"}{end}'
```

The node image ships `/etc/cni/net.d/10-flannel.conflist`. Older images can
remain NotReady until the tenant Flannel DaemonSet installs that file.

Create one pod on each of two tenant nodes and verify cross-node traffic at the
inner MTU (1400 in the example above):

```bash
mapfile -t TENANT_NODES < <(
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" get nodes \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
)
NODE_A="${TENANT_NODES[0]}"
NODE_B="${TENANT_NODES[1]}"
kubectl --kubeconfig "${TENANT_KUBECONFIG}" run net-a \
  --image=busybox:1.36 --restart=Never \
  --overrides="{\"spec\":{\"nodeName\":\"${NODE_A}\"}}" -- sleep 3600
kubectl --kubeconfig "${TENANT_KUBECONFIG}" run net-b \
  --image=busybox:1.36 --restart=Never \
  --overrides="{\"spec\":{\"nodeName\":\"${NODE_B}\"}}" -- sleep 3600
kubectl --kubeconfig "${TENANT_KUBECONFIG}" wait \
  --for=condition=Ready pod/net-a pod/net-b --timeout=5m
POD_B_IP="$(
  kubectl --kubeconfig "${TENANT_KUBECONFIG}" get pod net-b \
    -o jsonpath='{.status.podIP}'
)"
kubectl --kubeconfig "${TENANT_KUBECONFIG}" exec net-a -- \
  sh -c 'ip link show eth0 | grep "mtu 1400"'
kubectl --kubeconfig "${TENANT_KUBECONFIG}" exec net-a -- \
  ping -c 3 -s 1372 "${POD_B_IP}"
kubectl --kubeconfig "${TENANT_KUBECONFIG}" delete pod net-a net-b
```

## virtctl

Install the client that matches KubeVirt v1.9.0. Use the binary, not
`kubectl virtctl` (kubectl has no virtctl plugin here, and `kubectl -n ... virtctl` also fails because flags cannot precede a plugin name).

```bash
VERSION=v1.9.0
curl -fsSL -o /usr/local/bin/virtctl \
  "https://github.com/kubevirt/kubevirt/releases/download/${VERSION}/virtctl-${VERSION}-linux-amd64"
chmod +x /usr/local/bin/virtctl
virtctl version --client
```

Serial console (exit with Ctrl+] or Ctrl+5):

```bash
virtctl console passthrough-fleet-dpu-000000 -n dpf-operator-system
```

SimFleet cloud-init does not set a guest password or SSH key. The Ubuntu
container disk has no default password; console login as `ubuntu` will fail
until you add credentials to the cloud-init Secret and recreate the VMI.

Stop / start (namespace flag after the subcommand):

```bash
virtctl stop passthrough-fleet-dpu-000000 --force -n dpf-operator-system
virtctl start passthrough-fleet-dpu-000000 -n dpf-operator-system
```

With `runStrategy: Always`, deleting the VMI also forces a new virt-launcher:

```bash
kubectl -n dpf-operator-system delete vmi passthrough-fleet-dpu-000000
```

## Layout

| File                                 | Role                                             |
| ------------------------------------ | ------------------------------------------------ |
| `install-kubevirt.sh`                | KubeVirt 1.9 + ImageVolume / KVM patch           |
| `install-controller.sh`              | Helm install of simfleet-controller and its CRDs |
| `install-passthrough-simfleet.sh`    | Wait for CRDs, apply bundle, patch SimFleet      |
| `passthrough-simfleet.yaml`          | BMC Secret, BFB, flavor, DPUSet, chain, interfaces, SimFleet |
| `test.sh`                            | Contract tests for the installers, run by `make test-scripts` |

