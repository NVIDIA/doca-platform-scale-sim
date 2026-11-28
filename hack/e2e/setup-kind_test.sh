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

# Contract test for the kind config setup-kind.sh generates. It never creates a
# cluster: a fake kind records the config it is asked to create and a fake
# docker and kubectl accept everything else.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SETUP="${ROOT}/hack/e2e/setup-kind.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

assert_contains() {
	local file="$1" expected="$2"
	grep -Fq -- "${expected}" "${file}" || {
		cat "${file}" >&2
		fail "missing '${expected}' in ${file}"
	}
}

assert_absent() {
	local file="$1" unexpected="$2"
	if grep -Fq -- "${unexpected}" "${file}"; then
		cat "${file}" >&2
		fail "unexpected '${unexpected}' in ${file}"
	fi
}

mkdir -p "${TMP}/bin"

cat > "${TMP}/bin/kind" << 'EOF'
#!/usr/bin/env bash
case "$1 $2" in
"get clusters") exit 0 ;;
"get nodes") exit 0 ;;
"create cluster")
	for arg in "$@"; do
		case "${arg}" in
		--config=*) cp "${arg#--config=}" "${FAKE_KIND_CONFIG_OUT}" ;;
		esac
	done
	;;
esac
EOF
cat > "${TMP}/bin/docker" << 'EOF'
#!/usr/bin/env bash
# "network inspect" fails so the script takes the create path.
[ "$1 $2" = "network inspect" ] && exit 1
exit 0
EOF
cat > "${TMP}/bin/kubectl" << 'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "${TMP}/bin/kind" "${TMP}/bin/docker" "${TMP}/bin/kubectl"

# render_config runs the script with the given environment and prints the path
# of the config it handed to kind.
render_config() {
	local out="${TMP}/config-$1.yaml"
	shift
	rm -f "${out}"
	env -u DOCKER_HOST -u KIND_POD_SUBNET -u KIND_SERVICE_SUBNET -u KIND_API_SERVER_ADDRESS \
		PATH="${TMP}/bin:${PATH}" KIND="${TMP}/bin/kind" KUBECTL="${TMP}/bin/kubectl" \
		FAKE_KIND_CONFIG_OUT="${out}" "$@" "${SETUP}" > "${TMP}/run.log" 2>&1 \
		|| {
			cat "${TMP}/run.log" >&2
			fail "setup-kind.sh failed"
		}
	[ -s "${out}" ] || fail "setup-kind.sh did not hand a config to kind"
	echo "${out}"
}

# Unset: kind keeps its default pod and service CIDRs, and the config carries
# no networking section at all.
config="$(render_config default)"
assert_absent "${config}" "podSubnet"
assert_absent "${config}" "serviceSubnet"
assert_absent "${config}" "networking:"

# Both set: the pod-runtime run moves the host side off the DPU cluster's CIDRs.
config="$(render_config both KIND_POD_SUBNET=10.200.0.0/16 KIND_SERVICE_SUBNET=10.112.0.0/16)"
assert_contains "${config}" "networking:"
assert_contains "${config}" 'podSubnet: "10.200.0.0/16"'
assert_contains "${config}" 'serviceSubnet: "10.112.0.0/16"'

# Each is independent of the other.
config="$(render_config pod-only KIND_POD_SUBNET=10.200.0.0/16)"
assert_contains "${config}" 'podSubnet: "10.200.0.0/16"'
assert_absent "${config}" "serviceSubnet"
config="$(render_config svc-only KIND_SERVICE_SUBNET=10.112.0.0/16)"
assert_contains "${config}" 'serviceSubnet: "10.112.0.0/16"'
assert_absent "${config}" "podSubnet"

# Empty variables behave as unset: the Makefile always passes them through.
config="$(render_config empty KIND_POD_SUBNET= KIND_SERVICE_SUBNET=)"
assert_absent "${config}" "networking:"

# The API server address shares the one networking section with the subnets.
config="$(render_config address KIND_API_SERVER_ADDRESS=192.0.2.10 \
	KIND_POD_SUBNET=10.200.0.0/16 KIND_SERVICE_SUBNET=10.112.0.0/16)"
assert_contains "${config}" 'apiServerAddress: "192.0.2.10"'
assert_contains "${config}" 'podSubnet: "10.200.0.0/16"'
assert_contains "${config}" 'serviceSubnet: "10.112.0.0/16"'
[ "$(grep -c '^networking:' "${config}")" = 1 ] || fail "networking: appears more than once"

# The API server address alone still works as before.
config="$(render_config address-only KIND_API_SERVER_ADDRESS=192.0.2.10)"
assert_contains "${config}" 'apiServerAddress: "192.0.2.10"'
assert_absent "${config}" "podSubnet"

echo "PASS: setup-kind kind config"
