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

DPF_HELMFILES_REPO="${DPF_HELMFILES_REPO:-https://github.com/NVIDIA/doca-platform.git}"
DPF_HELMFILES_REF="${DPF_HELMFILES_REF:-15f1775b1baa453fa8d7f53082ece17f8f2bf091}"
DPF_HELMFILES_DIR="${DPF_HELMFILES_DIR:?DPF_HELMFILES_DIR is required}"

mkdir -p "$(dirname "${DPF_HELMFILES_DIR}")"

if [[ ! -d "${DPF_HELMFILES_DIR}/.git" ]]; then
	rm -rf "${DPF_HELMFILES_DIR}"
	git clone --filter=blob:none --sparse --no-checkout "${DPF_HELMFILES_REPO}" "${DPF_HELMFILES_DIR}"
	git -C "${DPF_HELMFILES_DIR}" sparse-checkout set deploy/helmfiles
fi

git -C "${DPF_HELMFILES_DIR}" fetch --depth 1 origin "${DPF_HELMFILES_REF}"
git -C "${DPF_HELMFILES_DIR}" checkout --detach FETCH_HEAD

if [[ ! -f "${DPF_HELMFILES_DIR}/deploy/helmfiles/prereqs.yaml" ]]; then
	echo "prereqs.yaml not found under ${DPF_HELMFILES_DIR}/deploy/helmfiles" >&2
	exit 1
fi

echo "DPF helmfiles ready at ${DPF_HELMFILES_DIR}/deploy/helmfiles (ref ${DPF_HELMFILES_REF})"
