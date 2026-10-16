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

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${ROOT}/lib.sh"

DISK="${1:?path to disk.img}"
DISK_SIZE="${DISK_SIZE:-${SIMFLEET_DISK_SIZE}}"
KUBE_APT_VERSION="${KUBE_APT_VERSION:-${SIMFLEET_KUBE_APT_VERSION}}"

fail() {
	echo "guest verification failed: $*" >&2
	exit 1
}

# guest runs read-only guestfish commands. Every caller treats a guestfish
# failure as a verification failure rather than as an empty answer.
guest() {
	local output
	if ! output="$(simfleet_guestfish "${DISK}" "$@")"; then
		fail "guestfish could not run: $*"
	fi
	printf '%s\n' "${output}"
}

expected_files=(
	/usr/bin/kubelet
	/usr/bin/kubeadm
	/usr/bin/kubectl
	/usr/bin/containerd
	/etc/containerd/config.toml
	/etc/modules-load.d/simfleet.conf
	/etc/sysctl.d/99-simfleet-k8s.conf
	/opt/cni/bin/bridge
	/opt/cni/bin/host-local
	/opt/cni/bin/loopback
	/etc/cni/net.d/10-flannel.conflist
	/usr/lib/systemd/system/kubelet.service
	/usr/lib/systemd/system/containerd.service
	/usr/lib/systemd/system/qemu-guest-agent.service
	/usr/lib/udev/rules.d/60-qemu-guest-agent.rules
	/opt/dpf/mock-dpuagent
	/opt/dpf/wait-kubelet-join.sh
	# Vendor units, so that a fleet with dpuAgent.enabled:false can mask them.
	# systemd reads /etc before /run before /usr/lib, so a unit file in /etc
	# would leave `systemctl mask` nowhere to put its /dev/null symlink.
	/usr/lib/systemd/system/mock-dpuagent.service
	/usr/lib/systemd/system/wait-kubelet-join.service
)
file_checks=()
for file in "${expected_files[@]}"; do
	file_checks+=("is-file ${file}")
done
present="$(guest "${file_checks[@]}")"
if ! awk -v expected="${#expected_files[@]}" '
  BEGIN { valid = 1 }
  NF != 1 || $1 != "true" { valid = 0 }
  END { exit valid && NR == expected ? 0 : 1 }
' <<< "${present}"; then
	echo "guest verification failed: expected guest files are missing" >&2
	printf 'checked: %s\n' "${expected_files[*]}" >&2
	printf '%s\n' "${present}" >&2
	exit 1
fi

# Debian moves qemu-ga to sbin; upstream keeps it in bin. Either is fine.
agent_binary="$(guest 'is-file /usr/sbin/qemu-ga' 'is-file /usr/bin/qemu-ga')"
if ! grep -qx true <<< "${agent_binary}"; then
	fail "the qemu-guest-agent binary is missing from /usr/sbin and /usr/bin"
fi

# unit_enabled answers whether systemd would start a unit at boot, and aborts
# rather than guessing when guestfish gives an answer it does not understand.
# The target defaults to multi-user.target; a unit that has to run after
# cloud-init is enabled under cloud-init.target instead.
unit_enabled() {
	local unit="$1" target="${2:-multi-user.target}" answer
	if ! answer="$(simfleet_guestfish "${DISK}" \
		"is-symlink /etc/systemd/system/${target}.wants/${unit}")"; then
		fail "guestfish could not check whether ${unit} is enabled"
	fi
	case "${answer}" in
	true) return 0 ;;
	false) return 1 ;;
	esac
	fail "unexpected guestfish answer for ${unit}: ${answer}"
}

# kubelet remains disabled until kubeadm join starts it.
if unit_enabled kubelet.service; then
	fail "kubelet is enabled; expected disabled"
fi
if ! unit_enabled containerd.service; then
	fail "containerd.service is disabled; expected enabled"
fi
# The mock is wanted by cloud-init.target because its configuration arrives
# from cloud-init and its unit is ordered after cloud-final.service.
if ! unit_enabled mock-dpuagent.service cloud-init.target; then
	fail "mock-dpuagent.service is disabled; expected enabled"
fi
if unit_enabled mock-dpuagent.service; then
	fail "mock-dpuagent.service is enabled under multi-user.target, which is reached before cloud-final"
fi
# The join helper needs the same cloud-init inputs, so it is enabled the same
# way: its credential and identity only exist once cloud-final has run.
if ! unit_enabled wait-kubelet-join.service cloud-init.target; then
	fail "wait-kubelet-join.service is disabled; expected enabled"
fi
if unit_enabled wait-kubelet-join.service; then
	fail "wait-kubelet-join.service is enabled under multi-user.target, which is reached before cloud-final"
fi

# Ubuntu intentionally ships qemu-guest-agent.service as a static unit. It is
# started by udev when KubeVirt exposes the virtio guest-agent port, so an
# enablement symlink is neither expected nor useful.
agent_rule="$(guest 'cat /usr/lib/udev/rules.d/60-qemu-guest-agent.rules')"
for fragment in \
	'SUBSYSTEM=="virtio-ports"' \
	'ATTR{name}=="org.qemu.guest_agent.0"' \
	'ENV{SYSTEMD_WANTS}="qemu-guest-agent.service"'; do
	if ! grep -Fq "${fragment}" <<< "${agent_rule}"; then
		fail "qemu-guest-agent udev rule lacks ${fragment}"
	fi
done

containerd_config="$(guest 'cat /etc/containerd/config.toml')"
if ! grep -q 'SystemdCgroup = true' <<< "${containerd_config}"; then
	fail "containerd does not put runc on the systemd cgroup driver"
fi
if grep -q 'SystemdCgroup = false' <<< "${containerd_config}"; then
	fail "containerd still leaves a runc runtime on the cgroupfs driver"
fi
if grep -Eq '^[[:space:]]*disabled_plugins[[:space:]]*=.*cri' <<< "${containerd_config}"; then
	fail "containerd disables the CRI plugin kubeadm needs"
fi

modules="$(guest 'cat /etc/modules-load.d/simfleet.conf')"
for module in overlay br_netfilter; do
	if ! grep -qx "${module}" <<< "${modules}"; then
		fail "/etc/modules-load.d/simfleet.conf does not load ${module}"
	fi
done

sysctls="$(guest 'cat /etc/sysctl.d/99-simfleet-k8s.conf')"
for setting in \
	'net.bridge.bridge-nf-call-iptables = 1' \
	'net.bridge.bridge-nf-call-ip6tables = 1' \
	'net.ipv4.ip_forward = 1'; do
	if ! grep -qx -- "${setting}" <<< "${sysctls}"; then
		fail "/etc/sysctl.d/99-simfleet-k8s.conf does not set '${setting}'"
	fi
done

# guestfish cannot execute guest binaries, so the version claim is checked
# against dpkg's own record of what apt installed.
dpkg_status="$(guest 'cat /var/lib/dpkg/status')"
for package in kubelet kubeadm kubectl; do
	installed="$(awk -v package="${package}" '
    BEGIN { RS = ""; FS = "\n" }
    {
      name = ""; version = ""
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^Package: /) { name = substr($i, 10) }
        else if ($i ~ /^Version: /) { version = substr($i, 10) }
      }
      if (name == package) { print version; exit }
    }
  ' <<< "${dpkg_status}")"
	if [ "${installed}" != "${KUBE_APT_VERSION}" ]; then
		fail "dpkg reports ${package} ${installed:-<absent>}; expected ${KUBE_APT_VERSION}"
	fi
done

# The apt indexes are ~50MB of state that is stale as soon as the image ships.
apt_lists="$(guest 'ls /var/lib/apt/lists')"
if stale="$(grep -E '_(Packages|Release|InRelease|Sources)$' <<< "${apt_lists}")"; then
	fail "apt package indexes were not cleaned: ${stale//$'\n'/ }"
fi
apt_archives="$(guest 'ls /var/cache/apt/archives')"
if stale="$(grep -E '\.deb$' <<< "${apt_archives}")"; then
	fail "downloaded .deb archives were not cleaned: ${stale//$'\n'/ }"
fi

# install.sh swaps in the QEMU user-mode resolver so apt works under
# virt-customize. That address means nothing to a booted guest, so the
# original configuration has to be back in place.
if [ "$(guest 'is-file /etc/resolv.conf.simfleet-build')" = true ]; then
	fail "the build-time resolv.conf backup was left in the guest"
fi
if [ "$(guest 'is-symlink /etc/resolv.conf')" != true ]; then
	resolv="$(guest 'cat /etc/resolv.conf')"
	if grep -q '169\.254\.2\.3' <<< "${resolv}"; then
		fail "the guest still resolves through the build-time QEMU nameserver"
	fi
fi

min_root_bytes="$(simfleet_min_root_bytes "${DISK_SIZE}")"
root_bytes="$(simfleet_root_fs_bytes "${DISK}")"
if [ "${root_bytes}" -lt "${min_root_bytes}" ]; then
	fail "root filesystem is ${root_bytes} bytes; expected at least ${min_root_bytes} for a ${DISK_SIZE} disk"
fi

echo "guest verification passed"
