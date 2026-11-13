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

# Prepares a host kernel to run many node-in-a-pod Pods. It is the program of
# the node-prep DaemonSet's init container, which runs this image privileged on
# the host network. It is not part of the VM disk.
#
# The floors come from the environment (the DaemonSet fills it from a
# ConfigMap). Sysctls that are floors are only ever raised; nf_conntrack_max is
# the exception and is set to exactly the computed target.

set -euo pipefail

INOTIFY_MAX_USER_INSTANCES="${INOTIFY_MAX_USER_INSTANCES:-8192}"
INOTIFY_MAX_USER_WATCHES="${INOTIFY_MAX_USER_WATCHES:-1048576}"
# 8192 inotify instances at about 30 per DPU.
DPU_SLOTS="${DPU_SLOTS:-270}"
PIDS_PER_DPU=45
CONNTRACK_PER_CPU=32768
CONNTRACK_MIN=131072

# Where the checks look. Tests point these at a scratch directory.
CGROUP_ROOT="${NODE_PREP_CGROUP_ROOT:-/sys/fs/cgroup}"
SYS_MODULE="${NODE_PREP_SYS_MODULE:-/sys/module}"

die() {
	echo "node-prep: $*" >&2
	exit 1
}

# Every number the script uses must be a plain non-negative integer; sysctl and
# the arithmetic below would otherwise fail in confusing ways.
require_integer() {
	local name="$1" value="$2"
	case "${value}" in
	'' | *[!0-9]*) die "${name} must be a non-negative integer, got '${value}'" ;;
	esac
}

# sysctl_value prints the live value of a sysctl or fails the script.
sysctl_value() {
	local key="$1" value
	value="$(sysctl -n "${key}")" || die "cannot read sysctl ${key}"
	value="${value//[[:space:]]/}"
	require_integer "sysctl ${key}" "${value}"
	echo "${value}"
}

# set_sysctl writes a value and checks that it stuck.
set_sysctl() {
	local key="$1" want="$2" got
	sysctl -w "${key}=${want}" > /dev/null || die "cannot write sysctl ${key}=${want}"
	got="$(sysctl_value "${key}")"
	[ "${got}" = "${want}" ] || die "sysctl ${key} is ${got} after writing ${want}; the write did not stick"
}

# raise_sysctl brings a sysctl up to a floor and never lowers it.
raise_sysctl() {
	local key="$1" floor="$2" live
	live="$(sysctl_value "${key}")"
	if [ "${live}" -ge "${floor}" ]; then
		echo "node-prep: ${key} is ${live}, at or above ${floor}"
		return
	fi
	echo "node-prep: raising ${key} from ${live} to ${floor}"
	set_sysctl "${key}" "${floor}"
}

# The node runs systemd and kubelet in a cgroup v2 hierarchy. A v1 host cannot
# be fixed at runtime, so this comes before anything is changed.
if [ ! -f "${CGROUP_ROOT}/cgroup.controllers" ]; then
	die "cgroup v2 is required: ${CGROUP_ROOT} is not a cgroup v2 mount. Boot the host with the unified cgroup hierarchy (systemd.unified_cgroup_hierarchy=1); this cannot be fixed at runtime."
fi

require_integer INOTIFY_MAX_USER_INSTANCES "${INOTIFY_MAX_USER_INSTANCES}"
require_integer INOTIFY_MAX_USER_WATCHES "${INOTIFY_MAX_USER_WATCHES}"
require_integer DPU_SLOTS "${DPU_SLOTS}"

cpus="$(nproc)" || die "cannot count the host CPUs"
require_integer "nproc" "${cpus}"
[ "${cpus}" -ge 1 ] || die "nproc reported ${cpus} CPUs"

# nf_conntrack_max is max(32768 x CPUs, 131072); the hash is at least a
# quarter of it.
conntrack_max=$((CONNTRACK_PER_CPU * cpus))
if [ "${conntrack_max}" -lt "${CONNTRACK_MIN}" ]; then
	conntrack_max="${CONNTRACK_MIN}"
fi
conntrack_hash=$(((conntrack_max + 3) / 4))
hashsize_param="${SYS_MODULE}/nf_conntrack/parameters/hashsize"

# ensure_conntrack_hash raises the hash of a loaded nf_conntrack to at least
# conntrack_hash, through the module parameter.
ensure_conntrack_hash() {
	local live
	[ -e "${hashsize_param}" ] || die "nf_conntrack is loaded but ${hashsize_param} does not exist; cannot size the conntrack hash"
	live="$(tr -d '[:space:]' < "${hashsize_param}")" || die "cannot read ${hashsize_param}"
	require_integer "${hashsize_param}" "${live}"
	if [ "${live}" -ge "${conntrack_hash}" ]; then
		echo "node-prep: nf_conntrack hashsize is ${live}, at or above ${conntrack_hash}"
		return
	fi
	echo "node-prep: raising nf_conntrack hashsize from ${live} to ${conntrack_hash}"
	echo "${conntrack_hash}" > "${hashsize_param}" || die "cannot write nf_conntrack hashsize (${hashsize_param})"
	live="$(tr -d '[:space:]' < "${hashsize_param}")"
	[ "${live}" -ge "${conntrack_hash}" ] || die "nf_conntrack hashsize is ${live} after writing ${conntrack_hash}; the write did not stick"
}

load_module() {
	local module="$1"
	shift
	echo "node-prep: loading ${module}"
	modprobe "${module}" "$@" || die "cannot load kernel module ${module}"
}

# nf_conntrack goes first: when it is not loaded yet, its hash size can only be
# chosen at load time, and br_netfilter would otherwise pull it in at the
# default size.
if [ -d "${SYS_MODULE}/nf_conntrack" ]; then
	load_module nf_conntrack
else
	load_module nf_conntrack "hashsize=${conntrack_hash}"
fi
ensure_conntrack_hash
for module in overlay br_netfilter vxlan; do
	load_module "${module}"
done

raise_sysctl fs.inotify.max_user_instances "${INOTIFY_MAX_USER_INSTANCES}"
raise_sysctl fs.inotify.max_user_watches "${INOTIFY_MAX_USER_WATCHES}"
raise_sysctl kernel.pid_max $((PIDS_PER_DPU * DPU_SLOTS))

# nf_conntrack_max is a target, not a floor. When it already holds, leave it:
# the sysctl is not writable from a host-network Pod whose network namespace is
# not the initial one (kind), where the Docker host applies it instead.
live_conntrack="$(sysctl_value net.netfilter.nf_conntrack_max)"
if [ "${live_conntrack}" = "${conntrack_max}" ]; then
	echo "node-prep: net.netfilter.nf_conntrack_max is already ${conntrack_max}"
else
	echo "node-prep: setting net.netfilter.nf_conntrack_max from ${live_conntrack} to ${conntrack_max} (${cpus} CPUs)"
	set_sysctl net.netfilter.nf_conntrack_max "${conntrack_max}"
fi

echo "node-prep: done"
