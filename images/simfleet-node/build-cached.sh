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

# Reuse a previously built containerDisk only when it carries the source hash
# of the recipe in this checkout. On a host that keeps its Docker images, the
# fixed simfleet-node tag survives between runs, and its mere existence says
# nothing about whether it matches the current recipe. GitHub-hosted runners
# start empty, so there this always builds.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "${ROOT}/lib.sh"

BASE_IMAGE="${BASE_IMAGE:?BASE_IMAGE (repo@digest) is required}"
NODE_IMG="${NODE_IMG:-simfleet-node:local}"
DISK_SIZE="${DISK_SIZE:-${SIMFLEET_DISK_SIZE}}"
CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
KUBE_APT_VERSION="${KUBE_APT_VERSION:-${SIMFLEET_KUBE_APT_VERSION}}"
IMAGE_VERSION="${IMAGE_VERSION:-}"
DPUAGENT_BIN="$(simfleet_dpuagent_bin)"

source_hash="$(
	BASE_IMAGE="${BASE_IMAGE}" DISK_SIZE="${DISK_SIZE}" \
		KUBE_APT_VERSION="${KUBE_APT_VERSION}" IMAGE_VERSION="${IMAGE_VERSION}" \
		DPUAGENT_BIN="${DPUAGENT_BIN}" \
		"${ROOT}/source-hash.sh"
)"

cached=""
if cached="$("${CONTAINER_TOOL}" image inspect \
	--format "{{index .Config.Labels \"${SIMFLEET_SOURCE_HASH_LABEL}\"}}" \
	"${NODE_IMG}" 2> /dev/null)"; then
	cached="$(printf '%s' "${cached}" | tr -d '[:space:]')"
else
	cached=""
fi

if [ -n "${cached}" ] && [ "${cached}" = "${source_hash}" ]; then
	echo "Reusing ${NODE_IMG}: source hash ${source_hash} is unchanged"
	exit 0
fi
if [ -n "${cached}" ]; then
	echo "Rebuilding ${NODE_IMG}: cached source hash ${cached} != ${source_hash}"
else
	echo "Building ${NODE_IMG}: no cached image with a source hash"
fi

BASE_IMAGE="${BASE_IMAGE}" NODE_IMG="${NODE_IMG}" DISK_SIZE="${DISK_SIZE}" \
	CONTAINER_TOOL="${CONTAINER_TOOL}" KUBE_APT_VERSION="${KUBE_APT_VERSION}" \
	IMAGE_VERSION="${IMAGE_VERSION}" DPUAGENT_BIN="${DPUAGENT_BIN}" \
	exec "${ROOT}/build.sh"
