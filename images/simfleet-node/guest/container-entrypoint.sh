#!/bin/bash

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

# Boots systemd as PID 1 in a privileged Pod. Adapted from the entrypoint of
# kind's node image (https://github.com/kubernetes-sigs/kind, Apache-2.0),
# reduced to the steps this image needs. THIRD_PARTY_NOTICES.md credits kind.

set -euo pipefail

# containerd's CRI gives a privileged container the host's cgroup namespace and
# a cgroup2 mount of the host's whole hierarchy. Without a private namespace
# and a mount rooted in it, this systemd and kubelet would manage the host's
# slices and fight the host kubelet over kubelet.slice.
if [ -z "${DPF_SIM_CGROUPNS:-}" ]; then
	export DPF_SIM_CGROUPNS=1
	exec unshare --cgroup "$0" "$@"
fi
unset DPF_SIM_CGROUPNS

umount /sys/fs/cgroup
mount -t cgroup2 -o rw,nosuid,nodev,noexec,relatime cgroup2 /sys/fs/cgroup
own_cgroup="$(grep -E '^0::' /proc/self/cgroup || true)"
if [ "${own_cgroup}" != "0::/" ]; then
	echo "cgroup namespace is not rooted at this container (/proc/self/cgroup: ${own_cgroup:-no cgroup v2 entry}); the node needs cgroup v2" >&2
	exit 1
fi

# kubelet volume mounts must propagate into nested containers.
mount --make-rshared /

# cgroup v2 forbids processes in a cgroup that delegates controllers, so move
# everything into a child before enabling controllers for systemd. The child
# must be named init.scope: systemd strips that suffix to find its root, so its
# slices land at the mount root where kubelet looks for them.
mkdir -p /sys/fs/cgroup/init.scope
xargs -rn1 < /sys/fs/cgroup/cgroup.procs > /sys/fs/cgroup/init.scope/cgroup.procs || true
controllers="$(sed -e 's/ / +/g' -e 's/^/+/' < /sys/fs/cgroup/cgroup.controllers)"
if [ "${controllers}" != "+" ]; then
	echo "${controllers}" > /sys/fs/cgroup/cgroup.subtree_control
fi

# systemd generates a machine ID at boot when the file is empty, so every DPU
# gets its own.
: > /etc/machine-id

# kubelet logs to /dev/kmsg.
if [ ! -e /dev/kmsg ]; then
	ln -s /dev/console /dev/kmsg
fi

exec /sbin/init
