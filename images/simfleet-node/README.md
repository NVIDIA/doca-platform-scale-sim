# SimFleet node containerDisk

This recipe customizes the Ubuntu 24.04 KubeVirt containerDisk with
Kubernetes v1.36.4 node packages, Ubuntu's containerd configured for kubeadm,
CNI plugins, and the QEMU guest agent.

## Requirements

Build on a Linux amd64 host with Docker. That is the whole list: libguestfs
runs inside the builder image described below, not on the host.

`make check-host` (or `./check-host.sh`) builds that image and boots its
appliance once, so a host that cannot build the disk says so in under a
minute. The e2e GitHub workflow runs it before `make test-e2e`.

Apple Silicon and other non-Linux-amd64 hosts cannot build this image or run
controller e2e tests that boot it. Use a Linux amd64 machine or the e2e
GitHub workflow (`make test-e2e` fails early elsewhere). That workflow runs on
GitHub-hosted `ubuntu-latest` on every push to `main`, on manual dispatch, and
on pull requests labelled `e2e`. Before building, it frees disk space, because
the containerDisk, Kind, DPF, and KubeVirt do not fit in the runner's default
free space. It also opens `/dev/kvm` to the runner user so KubeVirt can use
hardware virtualization.

### Why libguestfs runs in a container

libguestfs builds a supermin appliance from the host's own kernel, and Ubuntu
ships `/boot/vmlinuz-*` readable only by root. The GitHub Actions runner user
is in the `docker` group but is not root, so a host libguestfs fails when
supermin copies the kernel:

```
cp: cannot open '/boot/vmlinuz-7.0.0-31-generic' for reading: Permission denied
supermin: ... command failed, see earlier errors
```

`Dockerfile.builder` sidesteps that: it installs libguestfs and its own
kernel, bakes a fixed appliance with `libguestfs-make-fixed-appliance`, and
boots it as the last build step, so a broken toolchain fails at image build
time. `customize-disk.sh` holds every stage that needs libguestfs and runs in
there; `build.sh` keeps the registry work, which the host can do.

`/dev/kvm` is passed through when present — the Docker daemon is root, so this
works even where the calling user cannot open the device. Without it
libguestfs falls back to TCG, which is slower but correct. Force either way
with `SIMFLEET_KVM=on|off`.

## Build and push

Run these from this directory. The first command builds the mock-dpuagent
binary the image bakes in (see [Mock DPU agent](#mock-dpu-agent-is-baked-but-not-configured)).

```sh
make -C ../.. dpuagent-build
make node-image-build
make node-image-push NODE_IMG=registry.example.com/simfleet-node:v0.2.0
```

The default local tag is `simfleet-node:local`. Controller e2e uses
`simfleet-node:e2e`, which is never published. The Kubernetes version inside
the guest is recorded as the OCI label
`com.nvidia.simfleet.kubernetes.version`; a release also stamps
`org.opencontainers.image.version` with the project release ref. Publish only
that release tag. The base image is pinned to
the linux/amd64 manifest digest of `quay.io/containerdisks/ubuntu:24.04`.

### Cached builds

`make node-image-build-cached` stamps and then checks a
`com.nvidia.simfleet.node-source-hash` label covering the base image digest,
the disk size, every recipe and guest file, the commit in
`hack/dpuagent/mock.ref`, and the digest of the `mock-dpuagent` binary being
baked in. It reuses an existing image
only when that hash matches, so any recipe change rebuilds and the mere
existence of the fixed tag never counts as a cache hit. Print the current hash
with `make node-image-source-hash`.

The cache lives in the local Docker image store, so it only pays off on a
host that keeps its images between runs, such as a workstation or a
self-hosted runner. GitHub-hosted runners start every job with an empty store,
and the e2e workflow prunes images to free disk, so CI always takes the build
path.

## What the guest gets

| Area | Contents |
| --- | --- |
| Kubernetes | `kubelet`, `kubeadm`, `kubectl` pinned to `1.36.4-1.1` and held; kubelet **disabled** until `kubeadm join` starts it |
| Runtime | Ubuntu `containerd`, `/etc/containerd/config.toml` regenerated from `containerd config default` with CRI enabled and runc on `SystemdCgroup = true`; enabled at boot |
| kubeadm prerequisites | `/etc/modules-load.d/simfleet.conf` (`overlay`, `br_netfilter`) and `/etc/sysctl.d/99-simfleet-k8s.conf` (bridge nf-call for IPv4/IPv6, `net.ipv4.ip_forward`) |
| CNI | `kubernetes-cni` plugins in `/opt/cni/bin` and kube-flannel's default `/etc/cni/net.d/10-flannel.conflist` |
| Mock DPU agent | `/opt/dpf/mock-dpuagent` built from the doca-platform commit pinned by `hack/dpuagent/mock.ref`, plus `/opt/dpf/wait-kubelet-join.sh`, and enabled `mock-dpuagent.service` and `wait-kubelet-join.service` |
| Boot signal | `qemu-guest-agent`, activated by its virtio-port udev rule, so KubeVirt reports `AgentConnected` once the guest is actually up |
| Disk | Root filesystem grown offline to 10G while preserving the cloud image's GPT partition numbers, then sysprepped and sparsified |

### Mock DPU agent is baked, but not configured

The image carries the mock binary, the join helper, and both systemd units. It
carries no DPU identity, no management credential, and no mock configuration.
The SimFleet controller writes those into each VM's cloud-init Secret:

- `/etc/dpf-sim/identity.env` — deterministic DPU name, namespace, DPUNode, and
  device name for this ordinal (before the DPU object exists; no flavor field);
- `/etc/mock-dpuagent/config.yaml` — serial number, firmware versions,
  `dpuType`, `rebootMethod`, `dpuClusterJoin: kubelet`, and any
  `spec.dpuAgent.bmc` PSID or Redfish delay settings;
- `/etc/dpf-sim/mgmt.kubeconfig` — per-ordinal management token scoped to read
  this guest's future DPU and its join Secret.

`mock-dpuagent.service` refuses to start until cloud-init writes a non-empty
config file (`ExecStartPre=/usr/bin/test -s /etc/mock-dpuagent/config.yaml`).
The mock serves Redfish on **443** and **80** and impersonates the DPU agent
lifecycle DPF expects.

Both guest units are enabled under **`cloud-init.target`**, not
`multi-user.target`, because their inputs only exist after `cloud-final`
finishes. They are deliberately **not ordered against each other**: DPF cannot
create the join Secret until Redfish has already succeeded on the guest BMC, so
making the mock wait on the join helper would deadlock provisioning. The helper
waits for DPF to publish the DPU and its join Secret, runs `kubeadm join` as a
validated argument vector, and **leaves kubelet running** afterwards so the
mock's one-shot Start Kubelet step cannot race an already-stopped unit.

### Both units are vendor units, so that they can be masked

`customize-disk.sh` installs `mock-dpuagent.service` and
`wait-kubelet-join.service` into **`/usr/lib/systemd/system`**, alongside
`kubelet.service` and the rest of the distribution's units, and then enables
them. That path is a contract, not a convention.

A SimFleet with `spec.dpuAgent.enabled: false` stops both units by having
cloud-init run `systemctl mask --now` on them, so one image serves both
cases. A mask is a symlink to `/dev/null` at `/etc/systemd/system/<unit>`, and
systemd resolves a unit name by searching `/etc/systemd/system`, then
`/run/systemd/system`, then `/usr/lib/systemd/system`, taking the first hit.
Installing a real unit file into `/etc` therefore breaks masking in both
available forms: `systemctl mask` refuses, because the path it needs is
already a regular file, and `systemctl mask --runtime` writes to `/run`,
which the file in `/etc` then shadows. In both cases the `runcmd` achieves
nothing and the units start anyway — the mock with no config, the join helper
with no credential, retrying forever.

As vendor units they mask the ordinary way, and the mask persists across
reboots. Enabling still works: the enablement symlink is
`/etc/systemd/system/cloud-init.target.wants/<unit>`, a different path from
the mask symlink, so a masked unit stays enabled on disk and systemd simply
declines to start it. `verify-guest.sh` asserts the vendor placement and the
`cloud-init.target` enablement; a consistency test in
`test/consistency` ties that placement to the mask form
the controller renders, because neither suite can see the other half.

Build the mock binary before the image, as in [Build and push](#build-and-push),
or the build fails with the command to run. `dpuagent-build` invokes `hack/dpuagent/build.sh`, which checks out the commit
in `hack/dpuagent/mock.ref` and writes `bin/mock-dpuagent`. That commit and
the binary's digest are both part of the source hash, so a rebuilt mock never
reuses a cached image. `mock.ref` is a commit of the public GitHub mirror, and
the controller module requires `github.com/nvidia/doca-platform` at the same
commit (`TestMockRefIsTheRequiredDPFModule` fails otherwise), so the
controller's tests exercise the code the guest runs. Bump both together.

### Flannel owns pod networking

The image contains standard CNI plugin binaries and kube-flannel's default
`/etc/cni/net.d/10-flannel.conflist`. Tenant Flannel still owns PodCIDR
allocation and may overwrite that conflist. The baked file keeps kubelet
Ready when the Flannel DaemonSet copies `/opt/cni/bin/flannel` but skips
installing the conflist.

## Tests

```sh
make test
```

`test.sh` runs static contract checks and drives `build.sh`, `build-cached.sh`,
`customize-disk.sh`, and `verify-guest.sh` against stand-in `docker`,
`qemu-img`, `virt-*`, and `guestfish` commands. The `docker` stand-in runs
what a real `docker run` would, with the mounts translated back to host paths,
so the disk stages are still exercised rather than merely requested. It needs
nothing but bash and coreutils, so it also runs on macOS. The unit-test GitHub
workflow runs it on every pull request through `make test-scripts` in the
repository root. The `v1.36.4` guest pin shared with CI and the
scale scenario is checked by `test/consistency`, which
`make test` in the controller runs.

The output is a KubeVirt containerDisk containing `/disk/disk.img`; it is not
a runnable Ubuntu application container. Guest paths and units are checked by
`verify-guest.sh` at build time and by `test/consistency`
(`TestNodeImageShipsAgentGuestPaths`), which keeps the image and controller
cloud-init in step.
