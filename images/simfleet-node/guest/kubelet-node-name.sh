#!/bin/bash

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

# Gives every kubelet start the DPU's node name. mock-dpuagent can start the
# kubelet before kubeadm has written its flags, and a client certificate
# issued for the container hostname can never be rotated to the DPU name: the
# node authorizer only signs a CSR for the name the existing certificate
# already carries. The identity file is cloud-init's, so it is in place before
# either unit starts. It is parsed rather than sourced so a guest can never
# execute it.

IDENTITY_FILE="${IDENTITY_FILE:-/etc/dpf-sim/identity.env}"
KUBELET_DEFAULT="${KUBELET_DEFAULT:-/etc/default/kubelet}"

[ -r "${IDENTITY_FILE}" ] || {
	echo "kubelet-node-name: identity file ${IDENTITY_FILE} is missing" >&2
	exit 1
}

line="$(grep -m1 -E '^DPU_NAME=' "${IDENTITY_FILE}" || true)"
dpu_name="${line#DPU_NAME=}"
printf '%s' "${dpu_name}" | grep -Eq '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' || {
	echo "kubelet-node-name: identity file does not carry a usable DPU name" >&2
	exit 1
}

install -d -m 0755 "$(dirname "${KUBELET_DEFAULT}")"
tmp="$(mktemp "$(dirname "${KUBELET_DEFAULT}")/kubelet.XXXXXX")"
printf 'KUBELET_EXTRA_ARGS=--hostname-override=%s\n' "${dpu_name}" > "${tmp}"
chmod 0644 "${tmp}"
mv "${tmp}" "${KUBELET_DEFAULT}"
