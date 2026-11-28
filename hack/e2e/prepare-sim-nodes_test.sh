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

# Contract test for prepare-sim-nodes.sh. A fake docker and kubectl record
# their arguments, and the assertions are about what the script asked for: the
# conntrack preset runs privileged on the host network, and the node is labelled
# before config/node-prep/ is applied.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="${ROOT}/hack/e2e/prepare-sim-nodes.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

mkdir -p "${TMP}/bin"
cat > "${TMP}/bin/docker" << 'EOF'
#!/usr/bin/env bash
echo "docker $*" >> "${FAKE_CALLS}"
cat > "${FAKE_DOCKER_STDIN}"
EOF
cat > "${TMP}/bin/kubectl" << 'EOF'
#!/usr/bin/env bash
echo "kubectl $*" >> "${FAKE_CALLS}"
EOF
chmod +x "${TMP}/bin/docker" "${TMP}/bin/kubectl"

export FAKE_CALLS="${TMP}/calls"
export FAKE_DOCKER_STDIN="${TMP}/docker-stdin"
: > "${FAKE_CALLS}"

DOCKER="${TMP}/bin/docker" KUBECTL="${TMP}/bin/kubectl" \
	NODE_CONTAINER_IMG=example.com/node:test \
	bash "${SCRIPT}" > "${TMP}/run.log" 2>&1 || {
	cat "${TMP}/run.log" >&2
	fail "prepare-sim-nodes.sh failed"
}

docker_line="$(grep '^docker ' "${FAKE_CALLS}")"
for want in "--privileged" "--net=host" "example.com/node:test"; do
	case "${docker_line}" in
	*"${want}"*) ;;
	*) fail "conntrack preset did not run with ${want}: ${docker_line}" ;;
	esac
done

for want in "32768 * cpus" "131072" "nf_conntrack_max" "(max + 3) / 4"; do
	grep -Fq -- "${want}" "${FAKE_DOCKER_STDIN}" || fail "preset script does not contain: ${want}"
done

label_line="$(grep -n 'label nodes --all --overwrite scale.dpu.nvidia.com/sim-node=true' "${FAKE_CALLS}" | cut -d: -f1)"
apply_line="$(grep -n 'apply -n simfleet-node-prep -k ' "${FAKE_CALLS}" | cut -d: -f1)"
[ -n "${label_line}" ] || fail "nodes were not labelled sim-node=true"
[ -n "${apply_line}" ] || fail "config/node-prep was not applied"
[ "${label_line}" -lt "${apply_line}" ] || fail "nodes must be labelled before node-prep is applied"
grep -Fq 'pod-security.kubernetes.io/enforce=privileged' "${FAKE_CALLS}" || fail "namespace does not allow privileged pods"
grep -Fq 'rollout status daemonset/simfleet-node-prep' "${FAKE_CALLS}" || fail "script does not wait for the DaemonSet"

echo "PASS: prepare-sim-nodes"
