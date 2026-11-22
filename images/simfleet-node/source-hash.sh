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

# Print a digest of everything that determines the contents of the built
# containerDisk: the base image, the target disk size, and the recipe itself.
# build.sh stamps it on the image so CI can reuse a previous build only when
# the recipe that produced it is byte-for-byte the one in the checkout.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${ROOT}/lib.sh"

BASE_IMAGE="${BASE_IMAGE:?BASE_IMAGE (repo@digest) is required}"
DISK_SIZE="${DISK_SIZE:-${SIMFLEET_DISK_SIZE}}"
KUBE_APT_VERSION="${KUBE_APT_VERSION:-${SIMFLEET_KUBE_APT_VERSION}}"
IMAGE_VERSION="${IMAGE_VERSION:-}"

# mock-dpuagent is guest content, so the hash has to cover it. mock.ref states
# which commit it should have come from, and the binary digest states what is
# actually going to be baked in; a locally rebuilt agent must not reuse an
# image built from different bytes.
mock_ref="$(simfleet_mock_ref "${SIMFLEET_MOCK_REF:-${ROOT}/../../hack/dpuagent/mock.ref}")"
dpuagent_bin="$(simfleet_dpuagent_bin)"
if [ ! -s "${dpuagent_bin}" ]; then
	echo "mock-dpuagent binary not found at ${dpuagent_bin}" >&2
	echo "build it with: make dpuagent-build" >&2
	exit 1
fi
dpuagent_digest="$(simfleet_sha256 < "${dpuagent_bin}")"

# mock-ovs is guest content too. It is built from pkg/mock-ovs in this checkout,
# so the binary digest is what says which bytes go into the guest.
mock_ovs_bin="$(simfleet_mock_ovs_bin)"
if [ ! -s "${mock_ovs_bin}" ]; then
	echo "mock-ovs binary not found at ${mock_ovs_bin}" >&2
	echo "build it with: make mock-ovs-build" >&2
	exit 1
fi
mock_ovs_digest="$(simfleet_sha256 < "${mock_ovs_bin}")"

files=(
	Dockerfile
	Dockerfile.builder
	build-cached.sh
	build.sh
	check-host.sh
	customize-disk.sh
	lib.sh
	source-hash.sh
	verify-guest.sh
)
# Everything copied into the guest counts too, and globbing it means a new
# guest file cannot be added without invalidating the cache.
while IFS= read -r path; do
	files+=("guest/${path##*/guest/}")
done < <(find "${ROOT}/guest" -type f | LC_ALL=C sort)

{
	printf 'base-image=%s\n' "${BASE_IMAGE}"
	printf 'disk-size=%s\n' "${DISK_SIZE}"
	printf 'kube-apt-version=%s\n' "${KUBE_APT_VERSION}"
	printf 'image-version=%s\n' "${IMAGE_VERSION}"
	printf 'mock-ref=%s\n' "${mock_ref}"
	printf 'dpuagent-digest=%s\n' "${dpuagent_digest}"
	printf 'mock-ovs-digest=%s\n' "${mock_ovs_digest}"
	for path in "${files[@]}"; do
		printf 'file=%s\n' "${path}"
		cat "${ROOT}/${path}"
	done
} | simfleet_sha256
