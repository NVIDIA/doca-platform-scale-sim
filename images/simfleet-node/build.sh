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

# Customize quay.io/containerdisks/ubuntu into a KubeVirt containerDisk.
#
# The disk work runs inside the builder image from Dockerfile.builder, because
# the CI hosts cannot run libguestfs directly: their kernels are readable only
# by root and the runner user is not root. Only docker is needed here.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${ROOT}/lib.sh"

REMOVE_WORK=false
if [ -z "${WORK:-}" ]; then
	WORK="$(mktemp -d)"
	REMOVE_WORK=true
fi
cid=""
iid_file=""
cleanup() {
	if [ -n "${cid}" ]; then
		"${CONTAINER_TOOL:-docker}" rm "${cid}" > /dev/null 2>&1 || true
	fi
	if [ -n "${iid_file}" ]; then
		rm -f "${iid_file}"
	fi
	if "${REMOVE_WORK}"; then
		rm -rf "${WORK}"
	fi
}
trap cleanup EXIT

BASE_IMAGE="${BASE_IMAGE:?BASE_IMAGE (repo@digest) is required}"
NODE_IMG="${NODE_IMG:-simfleet-node:local}"
DISK_SIZE="${DISK_SIZE:-${SIMFLEET_DISK_SIZE}}"
CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
KUBE_APT_VERSION="${KUBE_APT_VERSION:-${SIMFLEET_KUBE_APT_VERSION}}"
IMAGE_VERSION="${IMAGE_VERSION:-}"
BUILDER_IMAGE="${BUILDER_IMAGE:-${SIMFLEET_BUILDER_IMAGE}}"
DPUAGENT_BIN="$(simfleet_dpuagent_bin)"

simfleet_require_host "${CONTAINER_TOOL}"

if [ ! -s "${DPUAGENT_BIN}" ]; then
	echo "mock-dpuagent binary not found at ${DPUAGENT_BIN}" >&2
	echo "build it with: make dpuagent-build" >&2
	exit 1
fi

source_hash="$(
	BASE_IMAGE="${BASE_IMAGE}" DISK_SIZE="${DISK_SIZE}" \
		KUBE_APT_VERSION="${KUBE_APT_VERSION}" IMAGE_VERSION="${IMAGE_VERSION}" \
		DPUAGENT_BIN="${DPUAGENT_BIN}" \
		"${ROOT}/source-hash.sh"
)"

mkdir -p "${WORK}"
echo "Extracting disk from ${BASE_IMAGE}"
cid="$("${CONTAINER_TOOL}" create --platform linux/amd64 "${BASE_IMAGE}")"
# Upstream containerDisk layout is /disk/disk.img.
"${CONTAINER_TOOL}" cp "${cid}:/disk/disk.img" "${WORK}/base.img"
"${CONTAINER_TOOL}" rm "${cid}" > /dev/null
cid=""

iid_file="${WORK}/builder.iid"
simfleet_build_builder_image "${CONTAINER_TOOL}" "${BUILDER_IMAGE}" "${iid_file}"
IFS= read -r builder_image_id < "${iid_file}"

echo "Customizing the disk in ${BUILDER_IMAGE}"
# The mock binary is built outside the recipe directory, so stage it into the
# work mount; /recipe is the only other path the builder can see.
cp "${DPUAGENT_BIN}" "${WORK}/mock-dpuagent"
simfleet_run_in_builder "${CONTAINER_TOOL}" "${builder_image_id}" "${WORK}" \
	-e "DISK_SIZE=${DISK_SIZE}" \
	-e "KUBE_APT_VERSION=${KUBE_APT_VERSION}" \
	-e "DPUAGENT_BIN=/work/mock-dpuagent" \
	-- /recipe/customize-disk.sh /work/base.img /work/disk.img

mkdir -p "${WORK}/context"
# The disk is multiple GB. Hard-link it into the build context, falling back to
# a reflink or plain copy, so the wrap stage does not double the disk usage.
rm -f "${WORK}/context/disk.img"
ln "${WORK}/disk.img" "${WORK}/context/disk.img" 2> /dev/null \
	|| cp --reflink=auto "${WORK}/disk.img" "${WORK}/context/disk.img" 2> /dev/null \
	|| cp "${WORK}/disk.img" "${WORK}/context/disk.img"
kube_version="v${KUBE_APT_VERSION%%-*}"
build_labels=(
	--label "${SIMFLEET_SOURCE_HASH_LABEL}=${source_hash}"
	--label "${SIMFLEET_KUBERNETES_VERSION_LABEL}=${kube_version}"
)
if [ -n "${IMAGE_VERSION}" ]; then
	build_labels+=(--label "${SIMFLEET_OCI_VERSION_LABEL}=${IMAGE_VERSION}")
fi
"${CONTAINER_TOOL}" build --platform linux/amd64 -t "${NODE_IMG}" \
	"${build_labels[@]}" \
	-f "${ROOT}/Dockerfile" "${WORK}/context"
echo "Built ${NODE_IMG} (source hash ${source_hash})"
