# Node preparation

A simulated DPU is a pod on the host kernel. systemd, containerd, kubelet,
flannel, and kube-proxy share that kernel's modules and non-namespaced
sysctls, and the host defaults are sized for one node. This DaemonSet
prepares each simulation node once, before any DPU pod starts.

`node-prep.sh` runs as a privileged init container from the
`simfleet-node-container` image. It requires cgroup v2, loads `overlay`,
`br_netfilter`, `vxlan`, and `nf_conntrack`, and raises the sysctls below.
The pause container only keeps the Pod alive after the init container exits,
so a Ready Pod means preparation succeeded.

cgroup v2 is selected at boot
(`systemd.unified_cgroup_hierarchy=1`). The script checks for it and exits
if the host is still on cgroup v1.

## What the ConfigMap sets

`configmap.yaml` is injected into the init container. Edit it before applying
to change how many DPUs a host is sized for. The script never lowers a live
inotify or `pid_max` value.

| Key | Default | Host setting |
| --- | --- | --- |
| `INOTIFY_MAX_USER_INSTANCES` | `8192` | `fs.inotify.max_user_instances`. One simulated DPU holds about 30 instances; the kernel default of 128 covers about five. |
| `INOTIFY_MAX_USER_WATCHES` | `1048576` | `fs.inotify.max_user_watches` |
| `DPU_SLOTS` | `270` | `kernel.pid_max` is raised to at least 45 PIDs per slot. 270 follows the inotify budget above. |

`net.netfilter.nf_conntrack_max` is computed on the node as
`max(32768 × CPUs, 131072)`, and the `nf_conntrack` hash is at least a
quarter of that. kube-proxy writes this sysctl whenever the live value
differs and exits when the write fails. The write is refused from inside a
DPU pod, so the host has to already hold kube-proxy's target. The script
leaves the sysctl alone when it already matches.

## Install

Build the node container image. The script is copied into that image.

```bash
make dpuagent-build
make -C images/simfleet-node node-container-image-build \
  NODE_CONTAINER_IMG=<registry>/simfleet-node-container:<tag>
docker push <registry>/simfleet-node-container:<tag>
```

Point the DaemonSet at the image the cluster can pull. In
`kustomization.yaml`:

```yaml
images:
  - name: simfleet-node-container
    newName: <registry>/simfleet-node-container
    newTag: <tag>
```

Label every node a DPU pod can land on, and apply the DaemonSet into a
namespace that allows privileged pods:

```bash
kubectl label node "${NODE}" scale.dpu.nvidia.com/sim-node=true
kubectl create namespace simfleet-node-prep
kubectl label namespace simfleet-node-prep \
  pod-security.kubernetes.io/enforce=privileged
kubectl apply -n simfleet-node-prep -k config/node-prep
kubectl -n simfleet-node-prep rollout status daemonset/simfleet-node-prep
```

The DaemonSet selects `scale.dpu.nvidia.com/sim-node=true` and tolerates a
`NoSchedule` taint with the same key and value. Put that label on the
fleet's `podTemplate.nodeSelector` so DPU pods land only on prepared nodes.
The fleet namespace has to allow privileged pods as well.

On Kind, the DaemonSet's host network is the Kind node container, so it
cannot write `nf_conntrack_max`. Set that sysctl, and the conntrack hash,
from the Docker host (`docker run --privileged --net=host`) before the
DaemonSet runs. When the value already matches, the init container leaves it
and still becomes Ready.
