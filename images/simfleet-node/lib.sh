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

# Shared helpers for the simfleet-node containerDisk recipe.
# shellcheck shell=bash

# Kept in sync with guest/install.sh by the version consistency test in
# test/consistency.
SIMFLEET_KUBE_APT_VERSION="1.36.4-1.1"
SIMFLEET_DISK_SIZE="10G"
# Stamped onto the built image so CI can tell a cached image apart from a stale
# one without trusting the tag.
SIMFLEET_SOURCE_HASH_LABEL="com.nvidia.simfleet.node-source-hash"
SIMFLEET_KUBERNETES_VERSION_LABEL="com.nvidia.simfleet.kubernetes.version"
SIMFLEET_OCI_VERSION_LABEL="org.opencontainers.image.version"

# Tag for the image that carries libguestfs. Local only: it is a build tool,
# never published.
SIMFLEET_BUILDER_IMAGE="simfleet-node-builder:local"

# Where the mock-dpuagent artifacts live in the guest. The controller renders these
# same paths into cloud-init, and the consistency test in
# test/consistency keeps the two in step.
SIMFLEET_DPUAGENT_GUEST_DIR="/opt/dpf"

# simfleet_dpuagent_bin echoes the linux/amd64 mock-dpuagent binary to bake in.
# hack/dpuagent/build.sh writes the default location.
simfleet_dpuagent_bin() {
	printf '%s\n' "${DPUAGENT_BIN:-${SIMFLEET_RECIPE_DIR:-${ROOT}}/../../bin/mock-dpuagent}"
}

# simfleet_mock_ref echoes the commit mock-dpuagent is built from. This is
# hack/dpuagent/mock.ref and nothing else: that file is what
# hack/dpuagent/build.sh checks out, so it is the only revision that describes
# the binary going into the guest. The controller module's doca-platform pin
# is a separate thing that can move independently in either direction, which
# makes it wrong as provenance. Read directly to keep the recipe shell-only.
simfleet_mock_ref() {
	local ref_file="${1:?path to hack/dpuagent/mock.ref}"
	local ref
	ref="$(tr -d '[:space:]' < "${ref_file}" 2> /dev/null || true)"
	if [ -z "${ref}" ]; then
		echo "does not pin a mock-dpuagent commit: ${ref_file}" >&2
		return 1
	fi
	printf '%s\n' "${ref}"
}

# simfleet_sha256 digests stdin, using whichever tool the host provides.
simfleet_sha256() {
	if command -v sha256sum > /dev/null 2>&1; then
		sha256sum | cut -d' ' -f1
	else
		shasum -a 256 | cut -d' ' -f1
	fi
}

# The disk stages run in the builder image, so the host itself only needs a
# container tool. Everything libguestfs needs is inside that image.
SIMFLEET_REQUIRED_COMMANDS=()

# simfleet_require_host fails unless this is a Linux amd64 host with the tools
# the recipe needs. CI calls it through check-host.sh before starting a build.
simfleet_require_host() {
	local container_tool="${1:-${CONTAINER_TOOL:-docker}}"
	if [ "$(uname -s)" != Linux ] || [ "$(uname -m)" != x86_64 ]; then
		echo "simfleet-node builds require a Linux amd64 host" >&2
		return 1
	fi
	local tool
	for tool in "${container_tool}" ${SIMFLEET_REQUIRED_COMMANDS[@]+"${SIMFLEET_REQUIRED_COMMANDS[@]}"}; do
		if ! command -v "${tool}" > /dev/null 2>&1; then
			echo "required command not found: ${tool}" >&2
			return 1
		fi
	done
}

# simfleet_build_builder_image builds the image that carries libguestfs. Its
# last layer boots the appliance, so a successful build is also proof that
# libguestfs works.
simfleet_build_builder_image() {
	local container_tool="$1" image="$2" iid_file="$3"
	: > "${iid_file}"
	"${container_tool}" build --platform linux/amd64 \
		--iidfile "${iid_file}" -t "${image}" \
		-f "${SIMFLEET_RECIPE_DIR:-${ROOT}}/Dockerfile.builder" \
		"${SIMFLEET_RECIPE_DIR:-${ROOT}}"

	local image_id
	image_id="$(tr -d '[:space:]' < "${iid_file}")"
	if [[ ! "${image_id}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
		echo "builder returned invalid image ID: ${image_id:-<empty>}" >&2
		return 1
	fi
	printf '%s\n' "${image_id}" > "${iid_file}"
}

# simfleet_kvm_args echoes the flags that give the builder container KVM.
# The docker daemon runs as root, so it can hand over /dev/kvm even where the
# calling user cannot open it, which is the case on the CI runners. Without
# the device libguestfs still works, under TCG, just slowly.
simfleet_kvm_args() {
	case "${SIMFLEET_KVM:-auto}" in
	off) return 0 ;;
	on)
		printf '%s\n' --device /dev/kvm
		return 0
		;;
	esac
	if [ -e /dev/kvm ]; then
		printf '%s\n' --device /dev/kvm
	fi
}

# simfleet_run_in_builder runs a command inside the builder image with the work
# directory mounted at /work and the recipe at /recipe. Arguments up to `--`
# are passed to the container tool, the rest is the command.
simfleet_run_in_builder() {
	local container_tool="$1" image="$2" work="$3"
	shift 3
	local tool_args=() kvm_arg
	while IFS= read -r kvm_arg; do
		[ -n "${kvm_arg}" ] && tool_args+=("${kvm_arg}")
	done < <(simfleet_kvm_args)
	while [ "$#" -gt 0 ] && [ "$1" != -- ]; do
		tool_args+=("$1")
		shift
	done
	[ "${1:-}" = -- ] && shift

	"${container_tool}" run --rm --platform linux/amd64 \
		${tool_args[@]+"${tool_args[@]}"} \
		-v "${work}:/work" \
		-v "${SIMFLEET_RECIPE_DIR:-${ROOT}}:/recipe:ro" \
		-e "HOST_UID=$(id -u)" \
		-e "HOST_GID=$(id -g)" \
		"${image}" \
		"$@"
}

# simfleet_report_libguestfs_host prints the things that actually break a
# libguestfs appliance build on a CI host, so the job log carries the diagnosis
# instead of just the failure.
simfleet_report_libguestfs_host() {
	echo
	echo "Diagnostics:"
	printf '  user: %s\n' "$(id 2> /dev/null || echo unknown)"

	# The appliance needs a readable kernel and its modules. Debian and Ubuntu
	# ship /boot/vmlinuz-* as 0600 root-only, which is the usual cause.
	local kernel found=0
	for kernel in /boot/vmlinuz-*; do
		[ -e "${kernel}" ] || continue
		found=1
		if [ -r "${kernel}" ]; then
			printf '  kernel %s: readable\n' "${kernel}"
		else
			printf '  kernel %s: NOT READABLE by this user\n' "${kernel}"
		fi
	done
	[ "${found}" = 1 ] || echo "  no /boot/vmlinuz-* found"

	if [ -e /dev/kvm ]; then
		if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
			echo "  /dev/kvm: read-write"
		else
			echo "  /dev/kvm: present but not read-write by this user"
		fi
	else
		echo "  /dev/kvm: absent (the appliance would fall back to emulation)"
	fi
	printf '  TMPDIR: %s\n' "${TMPDIR:-/tmp}"

	echo
	echo "Most likely fixes, on the build host:"
	echo "  - make the kernel readable: sudo chmod 0644 /boot/vmlinuz-*"
	echo "  - add this user to the kvm group"
	echo "  - confirm /var/tmp has room for the supermin appliance"

	if command -v libguestfs-test-tool > /dev/null 2>&1; then
		echo
		echo "Last 40 lines of libguestfs-test-tool:"
		libguestfs-test-tool 2>&1 | tail -40
	fi
}

# simfleet_size_to_bytes converts a qemu-img style size such as 10G to bytes.
simfleet_size_to_bytes() {
	local size="$1"
	local number="${size%[KkMmGgTt]}"
	local suffix="${size#"${number}"}"
	case "${number}" in
	'' | *[!0-9]*)
		echo "invalid size: ${size}" >&2
		return 1
		;;
	esac
	case "${suffix}" in
	'') echo "${number}" ;;
	K | k) echo "$((number * 1024))" ;;
	M | m) echo "$((number * 1024 * 1024))" ;;
	G | g) echo "$((number * 1024 * 1024 * 1024))" ;;
	T | t) echo "$((number * 1024 * 1024 * 1024 * 1024))" ;;
	*)
		echo "invalid size suffix in: ${size}" >&2
		return 1
		;;
	esac
}

# simfleet_guestfish runs read-only guestfish commands, one per argument, with
# the guest filesystems mounted. Commands go over stdin so a failing guestfish
# is a failing pipeline rather than empty output.
simfleet_guestfish() {
	local disk="$1"
	shift
	printf '%s\n' "$@" | guestfish --ro -a "${disk}" -i
}

# simfleet_root_device asks libguestfs which partition holds the root
# filesystem instead of assuming the base image's layout.
simfleet_root_device() {
	local disk="$1"
	local roots
	if ! roots="$(printf 'run\ninspect-os\n' | guestfish --ro -a "${disk}")"; then
		echo "failed to inspect ${disk} for a root filesystem" >&2
		return 1
	fi
	roots="$(printf '%s\n' "${roots}" | sed '/^[[:space:]]*$/d')"
	if [ -z "${roots}" ] || [ "$(printf '%s\n' "${roots}" | wc -l | tr -d ' ')" != 1 ]; then
		echo "expected exactly one root filesystem in ${disk}, got: ${roots}" >&2
		return 1
	fi
	printf '%s\n' "${roots}"
}

# simfleet_root_fs_bytes reports the total capacity of the guest root
# filesystem, which is what the 10Gi growth is supposed to produce.
simfleet_root_fs_bytes() {
	local disk="$1"
	local stats block_size blocks
	if ! stats="$(simfleet_guestfish "${disk}" 'statvfs /')"; then
		echo "failed to read root filesystem statistics from ${disk}" >&2
		return 1
	fi
	block_size="$(simfleet_statvfs_field "${stats}" frsize)"
	if [ -z "${block_size}" ] || [ "${block_size}" = 0 ]; then
		block_size="$(simfleet_statvfs_field "${stats}" bsize)"
	fi
	blocks="$(simfleet_statvfs_field "${stats}" blocks)"
	if [ -z "${block_size}" ] || [ -z "${blocks}" ]; then
		echo "could not parse statvfs output from ${disk}: ${stats}" >&2
		return 1
	fi
	echo "$((block_size * blocks))"
}

# simfleet_min_root_bytes reports the smallest root filesystem that counts as
# grown for a given disk size. It is well under the disk size on purpose:
# virt-resize preserves the base image's BIOS and ESP partitions, and statvfs
# counts only data blocks, so ext4's inode tables and journal do not show up.
# A real 10G build measures about 86% of the disk; the base image it grew from
# is 25%, so this still catches a resize that silently did nothing.
simfleet_min_root_bytes() {
	local disk_bytes
	disk_bytes="$(simfleet_size_to_bytes "$1")" || return 1
	echo "$((disk_bytes / 10 * 8))"
}

simfleet_statvfs_field() {
	awk -v key="$2" '
    $0 ~ "^[[:space:]]*" key ":" { gsub(/[^0-9]/, "", $0); print; exit }
  ' <<< "$1"
}
