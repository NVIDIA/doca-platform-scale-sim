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

# Fail early when a host cannot build the simfleet-node containerDisk. CI runs
# this before the build so a problem shows up before the job spends an hour
# reaching the same conclusion.
#
# The disk work happens inside the builder image, so this builds that image
# and boots its appliance once. Both are needed by the build itself, so
# nothing here is wasted work.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${ROOT}/lib.sh"

CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
BUILDER_IMAGE="${BUILDER_IMAGE:-${SIMFLEET_BUILDER_IMAGE}}"

simfleet_require_host "${CONTAINER_TOOL}"

# The image build proves libguestfs works without /dev/kvm. This proves it
# works with the flags a real build uses, which may add the KVM device.
probe="$(mktemp -d)"
trap 'rm -rf "${probe}"' EXIT
iid_file="${probe}/builder.iid"
simfleet_build_builder_image "${CONTAINER_TOOL}" "${BUILDER_IMAGE}" "${iid_file}"
IFS= read -r builder_image_id < "${iid_file}"
if ! simfleet_run_in_builder "${CONTAINER_TOOL}" "${builder_image_id}" "${probe}" \
	-- bash -c 'qemu-img create -f qcow2 /work/probe.qcow2 16M >/dev/null &&
    printf "run\n" | guestfish -a /work/probe.qcow2'; then
	echo "libguestfs cannot start its appliance in ${BUILDER_IMAGE}" >&2
	echo "Retry with SIMFLEET_KVM=off to rule out /dev/kvm passthrough." >&2
	exit 1
fi

echo "simfleet-node build host looks usable"
