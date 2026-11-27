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

# The container counterpart of verify-guest.sh. Runs the node-in-a-pod image
# with `docker run --privileged`, the way the Pod runs it, and checks what the
# image is supposed to provide.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${ROOT}/lib.sh"

IMAGE="${1:-${NODE_CONTAINER_IMG:-simfleet-node-container:local}}"
CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
# The image is linux/amd64; override only to check the recipe on another
# architecture, where the mock-dpuagent binary would not run.
PLATFORM="${PLATFORM:-linux/amd64}"
KUBE_APT_VERSION="${KUBE_APT_VERSION:-${SIMFLEET_KUBE_APT_VERSION}}"
# How long systemd gets to finish starting. It has no cloud-init to wait for,
# so a healthy boot is well inside this.
BOOT_TIMEOUT="${BOOT_TIMEOUT:-120}"

fail() {
	echo "container verification failed: $*" >&2
	exit 1
}

NAME="simfleet-node-verify-$$"
cleanup() {
	"${CONTAINER_TOOL}" rm -f "${NAME}" > /dev/null 2>&1 || true
}
trap cleanup EXIT

# The Pod mounts memory-backed emptyDirs at /run and /tmp.
if ! "${CONTAINER_TOOL}" run -d --privileged --platform "${PLATFORM}" \
	--name "${NAME}" --tmpfs /run --tmpfs /tmp "${IMAGE}" > /dev/null; then
	fail "could not start ${IMAGE}"
fi

# in_container runs a command in the verified container. Callers treat a
# failure as a verification failure, not as an empty answer.
in_container() {
	"${CONTAINER_TOOL}" exec "${NAME}" "$@"
}

# A container that exits stays visible to `exec` as an error, so say why.
container_running() {
	[ "$("${CONTAINER_TOOL}" inspect -f '{{.State.Running}}' "${NAME}" 2> /dev/null || true)" = true ]
}

state=""
deadline=$((SECONDS + BOOT_TIMEOUT))
while [ "${SECONDS}" -lt "${deadline}" ]; do
	if ! container_running; then
		"${CONTAINER_TOOL}" logs "${NAME}" 2>&1 | tail -40 >&2 || true
		fail "the container exited before systemd finished starting"
	fi
	# is-system-running exits non-zero for every state but "running", so only
	# the printed state matters here.
	state="$(in_container systemctl is-system-running 2> /dev/null || true)"
	case "${state}" in
	running | degraded) break ;;
	esac
	sleep 2
done
case "${state}" in
running | degraded) ;;
*)
	in_container systemctl list-jobs 2>&1 | tail -20 >&2 || true
	fail "systemd is '${state:-unreachable}' after ${BOOT_TIMEOUT}s; expected running or degraded"
	;;
esac
echo "systemd is ${state}"

# PID 1 is systemd, in init.scope at the root of a cgroup namespace the entrypoint
# made. That is what keeps its slices out of the host's hierarchy.
pid1="$(in_container cat /proc/1/comm)"
if [ "${pid1}" != systemd ]; then
	fail "PID 1 is ${pid1}; expected systemd"
fi
pid1_cgroup="$(in_container cat /proc/1/cgroup)"
if [ "${pid1_cgroup}" != "0::/init.scope" ]; then
	fail "PID 1 is in cgroup '${pid1_cgroup}'; expected 0::/init.scope"
fi

# degraded is acceptable, but not because of our own units.
failed="$(in_container systemctl list-units --failed --plain --no-legend --no-pager || true)"
for unit in mock-dpuagent.service wait-kubelet-join.service mock-ovs.service containerd.service; do
	if grep -q "^${unit} " <<< "${failed}"; then
		fail "${unit} failed: ${failed}"
	fi
done

# The units, enabled under multi-user.target: a container has no cloud-init.target.
for unit in mock-dpuagent.service wait-kubelet-join.service mock-ovs.service; do
	in_container test -f "/usr/lib/systemd/system/${unit}" \
		|| fail "/usr/lib/systemd/system/${unit} is missing"
	enabled="$(in_container systemctl is-enabled "${unit}" || true)"
	if [ "${enabled}" != enabled ]; then
		fail "${unit} is '${enabled}'; expected enabled"
	fi
	in_container test -L "/etc/systemd/system/multi-user.target.wants/${unit}" \
		|| fail "${unit} is not wanted by multi-user.target"
done
for file in /opt/dpf/mock-dpuagent /opt/dpf/wait-kubelet-join.sh \
	/etc/cni/net.d/10-flannel.conflist \
	/usr/lib/dpf-sim/kubeadm-join.args \
	"/usr/lib/dpf-sim/kubeadm-patches/kubeletconfiguration+strategic.yaml" \
	/usr/local/bin/container-entrypoint.sh; do
	in_container test -f "${file}" || fail "${file} is missing"
done
for file in /opt/dpf/mock-dpuagent /opt/dpf/wait-kubelet-join.sh /usr/local/bin/mock-ovs; do
	in_container test -x "${file}" || fail "${file} is not executable"
done
for file in /usr/bin/ovs-vsctl /usr/bin/ovs-ofctl; do
	in_container test -x "${file}" || fail "${file} is missing"
done

# Without the Secret neither unit has anything to do, so neither should run.
for unit in mock-dpuagent.service wait-kubelet-join.service; do
	active="$(in_container systemctl is-active "${unit}" || true)"
	if [ "${active}" = active ] || [ "${active}" = activating ] || [ "${active}" = failed ]; then
		fail "${unit} is ${active} without its configuration"
	fi
done

# mock-ovs needs no Secret, so it runs as soon as systemd does. Its unit creates
# br-sfc, br-hbn and br-ovn, and the OVS clients must see them through the socket.
active="$(in_container systemctl is-active mock-ovs.service || true)"
if [ "${active}" != active ]; then
	fail "mock-ovs.service is ${active}; expected active"
fi
bridges="$(in_container ovs-vsctl list-br || true)"
for bridge in br-sfc br-hbn br-ovn; do
	if ! grep -qx "${bridge}" <<< "${bridges}"; then
		fail "ovs-vsctl list-br does not show ${bridge}: ${bridges}"
	fi
done

# kubelet is installed but stays off until kubeadm join starts it.
in_container test -x /usr/bin/kubelet || fail "/usr/bin/kubelet is missing"
kubelet_enabled="$(in_container systemctl is-enabled kubelet.service || true)"
if [ "${kubelet_enabled}" = enabled ]; then
	fail "kubelet is enabled; expected disabled"
fi
if [ "$(in_container systemctl is-active kubelet.service || true)" = active ]; then
	fail "kubelet is running; expected stopped"
fi
if [ "$(in_container systemctl is-enabled containerd.service || true)" != enabled ]; then
	fail "containerd.service is not enabled"
fi

for package in kubelet kubeadm kubectl; do
	installed="$(in_container dpkg-query -W -f='${Version}' "${package}" || true)"
	if [ "${installed}" != "${KUBE_APT_VERSION}" ]; then
		fail "dpkg reports ${package} ${installed:-<absent>}; expected ${KUBE_APT_VERSION}"
	fi
done

# Units a container must not run stay masked. The OVS daemon units would serve
# the sockets mock-ovs serves, so they are masked as well.
for unit in systemd-modules-load.service systemd-resolved.service \
	systemd-udevd.service 'getty@.service' systemd-remount-fs.service \
	openvswitch-switch.service ovs-record-hostname.service \
	ovsdb-server.service ovs-vswitchd.service; do
	masked="$(in_container systemctl is-enabled "${unit}" 2> /dev/null || true)"
	if [ "${masked}" != masked ]; then
		fail "${unit} is '${masked}'; expected masked"
	fi
done

# VM-only content must not leak into the container image.
if in_container test -e /etc/modules-load.d/simfleet.conf; then
	fail "/etc/modules-load.d/simfleet.conf exists; module loading is host preparation"
fi
if in_container dpkg-query -W qemu-guest-agent > /dev/null 2>&1; then
	fail "qemu-guest-agent is installed"
fi

join_args="$(in_container cat /usr/lib/dpf-sim/kubeadm-join.args)"
if [ "${join_args}" != "--patches=/usr/lib/dpf-sim/kubeadm-patches" ]; then
	fail "kubeadm-join.args is '${join_args}'"
fi

echo "container verification passed"
