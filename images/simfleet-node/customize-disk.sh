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

set -euo pipefail

# Every stage that needs libguestfs, run inside the builder image from
# Dockerfile.builder. build.sh handles the container-registry work on the host
# and calls this for the disk itself.
#
# Takes the extracted base disk and produces the finished, verified disk.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${ROOT}/lib.sh"

BASE_IMG="${1:?path to the extracted base disk}"
OUT_IMG="${2:?path to write the customized disk to}"
DISK_SIZE="${DISK_SIZE:-${SIMFLEET_DISK_SIZE}}"
KUBE_APT_VERSION="${KUBE_APT_VERSION:-${SIMFLEET_KUBE_APT_VERSION}}"
# build.sh stages the mock into the work mount. Check it before the resize so
# a broken caller fails in a second rather than after growing a 10G disk.
DPUAGENT_BIN="${DPUAGENT_BIN:-}"
if [ ! -s "${DPUAGENT_BIN}" ]; then
	echo "mock-dpuagent binary not found at ${DPUAGENT_BIN:-<unset>}" >&2
	echo "build it with: make dpuagent-build" >&2
	exit 1
fi

disk_bytes="$(simfleet_size_to_bytes "${DISK_SIZE}")"
min_root_bytes="$(simfleet_min_root_bytes "${DISK_SIZE}")"

# Grow a copy in place while the guest is offline. The Ubuntu cloud image's
# partitions are numbered 1,14,15,16 but physically ordered 14,15,16,1.
# virt-resize rewrites them in physical order as 1,2,3,4, which leaves GRUB
# referring to a partition that no longer exists. Moving the GPT backup header
# and the end of partition 1 preserves every partition number and bootloader
# reference.
if ! root_device="$(simfleet_root_device "${BASE_IMG}")"; then
	# The first libguestfs call in the pipeline. If the appliance cannot boot
	# here the cause is the container's environment, so say what it looks like.
	simfleet_report_libguestfs_host >&2
	exit 1
fi
if [[ ! "${root_device}" =~ ^(/dev/[a-z]+)([0-9]+)$ ]]; then
	echo "cannot split root device into disk and partition number: ${root_device}" >&2
	exit 1
fi
root_disk="${BASH_REMATCH[1]}"
root_partition="${BASH_REMATCH[2]}"
if [ "$((disk_bytes % 512))" -ne 0 ]; then
	echo "disk size must be a multiple of 512 bytes: ${DISK_SIZE}" >&2
	exit 1
fi
# GPT reserves its final 33 sectors; sectors are zero-based, so N-34 is the
# final usable LBA.
root_end_sector="$((disk_bytes / 512 - 34))"

echo "Growing ${root_device} into a ${DISK_SIZE} disk"
qemu-img convert -O qcow2 "${BASE_IMG}" "${OUT_IMG}"
qemu-img resize "${OUT_IMG}" "${DISK_SIZE}"
guestfish --rw -a "${OUT_IMG}" << EOF
run
part-expand-gpt ${root_disk}
part-resize ${root_disk} ${root_partition} ${root_end_sector}
resize2fs ${root_device}
EOF
rm -f "${BASE_IMG}"

grown_root_device="$(simfleet_root_device "${OUT_IMG}")"
if [ "${grown_root_device}" != "${root_device}" ]; then
	echo "root partition changed from ${root_device} to ${grown_root_device}" >&2
	exit 1
fi
grown_bytes="$(simfleet_root_fs_bytes "${OUT_IMG}")"
if [ "${grown_bytes}" -lt "${min_root_bytes}" ]; then
	echo "root filesystem is ${grown_bytes} bytes; expected at least ${min_root_bytes}" >&2
	exit 1
fi
echo "Root filesystem grew to ${grown_bytes} bytes"

virt-customize -a "${OUT_IMG}" \
	--copy-in "${ROOT}/guest/install.sh:/tmp" \
	--run-command "chmod +x /tmp/install.sh && KUBE_APT_VERSION='${KUBE_APT_VERSION}' /tmp/install.sh" \
	--run-command 'rm -f /tmp/install.sh' \
	--copy-in "${ROOT}/guest/10-flannel.conflist:/etc/cni/net.d" \
	--run-command 'chmod 0644 /etc/cni/net.d/10-flannel.conflist'

# The mock itself. Its configuration arrives later from cloud-init, so the
# image contains only the binary, the join helper, and the units.
#
# The units go to /usr/lib/systemd/system, alongside kubelet.service and the
# rest of the distribution's own units, rather than to /etc/systemd/system.
# systemd searches /etc, then /run, then /usr/lib, and a controller rendering
# cloud-init for a fleet with dpuAgent.enabled:false stops these two by
# masking them, which means writing /etc/systemd/system/<unit> -> /dev/null.
# A real unit file parked in /etc leaves nowhere higher for that symlink to
# go: `systemctl mask` refuses outright, and `mask --runtime` writes to /run,
# which /etc then shadows, so the units start regardless. As vendor units
# they are maskable the ordinary way, and the mask persists across reboots.
# /lib/systemd/system is the same directory on Ubuntu 24.04, which is fully
# usr-merged; the real path is used here.
virt-customize -a "${OUT_IMG}" \
	--mkdir "${SIMFLEET_DPUAGENT_GUEST_DIR}" \
	--copy-in "${DPUAGENT_BIN}:${SIMFLEET_DPUAGENT_GUEST_DIR}" \
	--copy-in "${ROOT}/guest/wait-kubelet-join.sh:${SIMFLEET_DPUAGENT_GUEST_DIR}" \
	--copy-in "${ROOT}/guest/mock-dpuagent.service:/usr/lib/systemd/system" \
	--copy-in "${ROOT}/guest/wait-kubelet-join.service:/usr/lib/systemd/system" \
	--run-command "chmod 0755 ${SIMFLEET_DPUAGENT_GUEST_DIR}/mock-dpuagent ${SIMFLEET_DPUAGENT_GUEST_DIR}/wait-kubelet-join.sh" \
	--run-command "chmod 0644 /usr/lib/systemd/system/mock-dpuagent.service /usr/lib/systemd/system/wait-kubelet-join.service" \
	--run-command 'systemctl daemon-reload' \
	--run-command 'systemctl enable mock-dpuagent.service' \
	--run-command 'systemctl enable wait-kubelet-join.service'

virt-sysprep -a "${OUT_IMG}" \
	--operations machine-id,ssh-hostkeys,net-hostname,net-hwaddr,dhcp-client-state,logfiles,tmp-files,crash-data

# Reclaim the blocks apt and the resize left behind; a containerDisk layer ships
# the whole disk image, so unsparsified free space is pushed to the registry.
virt-sparsify --in-place "${OUT_IMG}"

DISK_SIZE="${DISK_SIZE}" KUBE_APT_VERSION="${KUBE_APT_VERSION}" \
	"${ROOT}/verify-guest.sh" "${OUT_IMG}"

# This runs as root in the container, so hand the disk back to the user who
# owns the work directory. Otherwise the wrap stage on the host cannot manage
# its own build context.
if [ -n "${HOST_UID:-}" ] && [ -n "${HOST_GID:-}" ]; then
	chown "${HOST_UID}:${HOST_GID}" "${OUT_IMG}"
fi
