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

set -o nounset
set -o pipefail
set -o errexit

HELM="${HELM:?HELM is required}"
DPF_HELM_REPO="${DPF_HELM_REPO:?DPF_HELM_REPO is required}"
DPF_HELM_CHART="${DPF_HELM_CHART:?DPF_HELM_CHART is required}"
DPF_HELM_VERSION="${DPF_HELM_VERSION:?DPF_HELM_VERSION is required}"
DPF_HELMFILES_DIR="${DPF_HELMFILES_DIR:?DPF_HELMFILES_DIR is required}"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

"${HELM}" pull "${DPF_HELM_CHART}" --repo "${DPF_HELM_REPO}" --version "${DPF_HELM_VERSION}" \
	--untar --untardir "${tmp}" > /dev/null

if [[ ! -f "${tmp}/${DPF_HELM_CHART}/dev/helmfiles/prereqs.yaml" ]]; then
	echo "dev/helmfiles/prereqs.yaml not found in chart ${DPF_HELM_CHART} ${DPF_HELM_VERSION}" >&2
	exit 1
fi

rm -rf "${DPF_HELMFILES_DIR}"
mkdir -p "$(dirname "${DPF_HELMFILES_DIR}")"
mv "${tmp}/${DPF_HELM_CHART}/dev/helmfiles" "${DPF_HELMFILES_DIR}"

echo "DPF helmfiles ready at ${DPF_HELMFILES_DIR} (chart ${DPF_HELM_CHART} ${DPF_HELM_VERSION})"
