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

set -eu -o pipefail

SCENARIO_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
# Keep mocks on the workspace filesystem. Job /tmp can be noexec, which
# makes PATH look like it has helm/kubectl while execve returns EACCES and
# the installer falls through to a leftover binary from the Go module cache.
TMP_DIR="$(mktemp -d "${SCENARIO_DIR}/.test.XXXXXX")"
trap 'rm -rf "${TMP_DIR}"' EXIT
trap 'exit 1' HUP INT TERM

BIN_DIR="${TMP_DIR}/bin"
LOG="${TMP_DIR}/operations.log"
STDIN_LOG="${TMP_DIR}/stdin.log"
mkdir -p "${BIN_DIR}"
: > "${LOG}"
: > "${STDIN_LOG}"
export LOG STDIN_LOG
# Do not inherit a helm/kubectl from the repository bin/ (CI cache)
# or a FAIL_HELM_UPGRADE leftover. The mocks must be the only tools.
BASH_BIN="$(command -v bash)"
MOCK_PATH="${BIN_DIR}:$(dirname "${BASH_BIN}"):/usr/bin:/bin"
unset FAIL_HELM_UPGRADE MOCK_LOG MOCK_HELM_SOURCE HELM_BIN_DIR LOCALBIN

cat > "${BIN_DIR}/helm" << 'EOF'
#!/usr/bin/env bash
{
	printf 'helm'
	printf ' %s' "$@"
	printf '\n'
} >>"${LOG}"

case "${1:-}" in
version)
	printf 'v3.18.3\n'
	;;
upgrade)
	if test "${FAIL_HELM_UPGRADE:-0}" = 1; then
		exit 1
	fi
	;;
esac
EOF

cat > "${BIN_DIR}/kubectl" << 'EOF'
#!/usr/bin/env bash
# Keep what the installers pipe into `kubectl apply -f -`; the assertions read
# the rendered manifest from there.
if test "${1:-}" = apply && test "${2:-}" = -f && test "${3:-}" = -; then
	cat >>"${STDIN_LOG}"
fi

{
	printf 'kubectl'
	printf ' %s' "$@"
	printf '\n'
} >>"${LOG}"
EOF

chmod +x "${BIN_DIR}/helm" "${BIN_DIR}/kubectl"

fail() {
	echo "$*" >&2
	if test -s "${LOG}"; then
		echo "--- commands ---" >&2
		cat "${LOG}" >&2
	fi
	if test -s "${STDIN_LOG}"; then
		echo "--- kubectl stdin ---" >&2
		cat "${STDIN_LOG}" >&2
	fi
	exit 1
}

logged_line() {
	local needle="$1"
	local line
	line="$(awk -v expected="${needle}" '$0 == expected { print NR; exit }' "${LOG}")"
	if test -z "${line}"; then
		fail "missing logged command: ${needle}"
	fi
	printf '%s\n' "${line}"
}

assert_before() {
	local first_line="$1"
	local second_line="$2"
	local first_desc="$3"
	local second_desc="$4"
	if test "${first_line}" -ge "${second_line}"; then
		fail "${first_desc} (line ${first_line}) did not precede ${second_desc} (line ${second_line})"
	fi
}

run_installer() {
	env -i \
		PATH="${MOCK_PATH}" \
		HOME="${TMP_DIR}" \
		LOG="${LOG}" \
		STDIN_LOG="${STDIN_LOG}" \
		"$@"
}

run_installer \
	CHART="example.invalid/simfleet-controller" \
	CHART_VERSION="1.2.3" \
	"${BASH_BIN}" "${SCENARIO_DIR}/install-controller.sh" > /dev/null

upgrade_line="$(logged_line \
	'helm upgrade --install simfleet-controller example.invalid/simfleet-controller --version 1.2.3 --namespace simfleet-controller-system --create-namespace --wait --timeout 5m')"
wait_line="$(logged_line \
	'kubectl wait --for=condition=Established crd/simfleets.scale.dpu.nvidia.com crd/siminstances.scale.dpu.nvidia.com --timeout=2m')"
assert_before "${upgrade_line}" "${wait_line}" "helm upgrade" "kubectl wait"
# The chart carries the CRDs as templates; `helm show crds` prints nothing
# for it, so applying that output would fail the install.
if grep -q '^helm show crds' "${LOG}"; then
	fail "install-controller.sh applies helm show crds output"
fi

: > "${LOG}"
if run_installer \
	CHART="example.invalid/simfleet-controller" \
	CHART_VERSION="1.2.3" \
	FAIL_HELM_UPGRADE=1 \
	"${BASH_BIN}" "${SCENARIO_DIR}/install-controller.sh" > /dev/null 2>&1; then
	fail "install-controller.sh succeeded when helm upgrade failed"
fi
if grep -q '^kubectl wait ' "${LOG}"; then
	fail "kubectl wait ran after helm upgrade failed"
fi

: > "${LOG}"
if run_installer \
	CHART="example.invalid/simfleet-controller" \
	"${BASH_BIN}" "${SCENARIO_DIR}/install-controller.sh" > /dev/null 2>&1; then
	fail "install-controller.sh ran without CHART_VERSION"
fi
if grep -q '^helm upgrade ' "${LOG}"; then
	fail "helm upgrade ran without CHART_VERSION"
fi

TEST_VM_IMAGE='registry.example.com:5000/nvidia/doca/simfleet-node@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef'
TEST_BFB_URL='http://bfb.example.com/bf-bundle.bfb'

assert_rendered_manifest() {
	local fleet_id="${1:-passthrough}"
	local replicas="${2:-2}"
	grep -Fq "    image: ${TEST_VM_IMAGE}" "${STDIN_LOG}" \
		|| fail "rendered VM image missing from kubectl apply stdin"
	if grep -Fq 'example.invalid/simfleet-node:set-VM_IMAGE' "${STDIN_LOG}"; then
		fail "placeholder reached kubectl apply"
	fi
	grep -Fxq "  url: \"${TEST_BFB_URL}\"" "${STDIN_LOG}" \
		|| fail "rendered BFB URL missing from kubectl apply stdin"
	if grep -Fq 'set-BFB_URL' "${STDIN_LOG}"; then
		fail "BFB_URL placeholder reached kubectl apply"
	fi
	if grep -Fq 'set-FLEET_ID' "${STDIN_LOG}"; then
		fail "FLEET_ID placeholder reached kubectl apply"
	fi
	grep -Fq "  name: ${fleet_id}" "${STDIN_LOG}" \
		|| fail "rendered DPUSet name missing from kubectl apply stdin"
	grep -Fq "  name: ${fleet_id}-fleet" "${STDIN_LOG}" \
		|| fail "rendered SimFleet name missing from kubectl apply stdin"
	grep -Fq "      scale.dpu.nvidia.com/pool: ${fleet_id}" "${STDIN_LOG}" \
		|| fail "rendered pool label missing from kubectl apply stdin"
	grep -Fxq "  replicas: ${replicas}" "${STDIN_LOG}" \
		|| fail "rendered replicas ${replicas} missing from kubectl apply stdin"
	# The SimFleet must be created with its final spec, so the controller
	# never reconciles the manifest defaults.
	if grep -q '^kubectl patch ' "${LOG}"; then
		fail "installer patches the SimFleet after apply"
	fi
	grep -Fq 'name: bmc-shared-password' "${STDIN_LOG}" \
		|| fail "bmc-shared-password Secret missing from kubectl apply stdin"
	grep -Eq '^  password: .+' "${STDIN_LOG}" \
		|| fail "bmc-shared-password has no non-empty password key"
	# DPF wraps DPUFlavor.rawConfigScript in bf.cfg's `cat << \EOF`. A
	# nested terminator that is exactly EOF closes that heredoc and drops
	# later user-data, including dpuagent.conf. The SSH-streamed installer
	# inlines this manifest, so both apply paths must stay safe.
	if grep -Eq '^[[:space:]]*EOF[[:space:]]*$' "${STDIN_LOG}"; then
		fail "nested EOF would terminate DPF's bf.cfg user-data heredoc"
	fi
}

run_passthrough_installer() {
	: > "${LOG}"
	: > "${STDIN_LOG}"
	run_installer \
		VM_IMAGE="${TEST_VM_IMAGE}" \
		BFB_URL="${TEST_BFB_URL}" \
		"${BASH_BIN}" "$@" > /dev/null \
		|| fail "install-passthrough-simfleet.sh failed"
}

# Exercise the normal checked-in manifest branch.
run_passthrough_installer \
	"${SCENARIO_DIR}/install-passthrough-simfleet.sh"
assert_rendered_manifest

# Exercise the inline fallback used when the script is streamed over SSH. A
# manifest in the working directory is not the script's and must be ignored.
: > "${LOG}"
: > "${STDIN_LOG}"
printf 'decoy: manifest-from-working-directory\n' > "${TMP_DIR}/passthrough-simfleet.yaml"
(
	cd "${TMP_DIR}"
	run_installer \
		VM_IMAGE="${TEST_VM_IMAGE}" \
		BFB_URL="${TEST_BFB_URL}" \
		PWD="${TMP_DIR}" \
		"${BASH_BIN}" -s < "${SCENARIO_DIR}/install-passthrough-simfleet.sh" \
		> /dev/null
) || fail "install-passthrough-simfleet.sh failed when streamed on stdin"
rm -f "${TMP_DIR}/passthrough-simfleet.yaml"
assert_rendered_manifest

# A second fleet in the same namespace must not reuse the default names.
: > "${LOG}"
: > "${STDIN_LOG}"
run_installer \
	VM_IMAGE="${TEST_VM_IMAGE}" \
	BFB_URL="${TEST_BFB_URL}" \
	FLEET_ID=lab2 \
	"${BASH_BIN}" "${SCENARIO_DIR}/install-passthrough-simfleet.sh" \
	> /dev/null \
	|| fail "install-passthrough-simfleet.sh failed with FLEET_ID=lab2"
assert_rendered_manifest lab2
if grep -Fq 'name: passthrough-fleet' "${STDIN_LOG}"; then
	fail "default fleet name leaked into a FLEET_ID=lab2 apply"
fi

: > "${LOG}"
: > "${STDIN_LOG}"
run_installer \
	VM_IMAGE="${TEST_VM_IMAGE}" \
	BFB_URL="${TEST_BFB_URL}" \
	REPLICAS=10 \
	"${BASH_BIN}" "${SCENARIO_DIR}/install-passthrough-simfleet.sh" \
	> /dev/null \
	|| fail "install-passthrough-simfleet.sh failed with a REPLICAS override"
assert_rendered_manifest passthrough 10

for invalid in "FLEET_ID=Not a label" "REPLICAS=-1" "REPLICAS=2}" \
	"DPU_CLUSTER_NAME=a\"b" "DPU_CLUSTER_NS=Tenant" \
	"VM_IMAGE=registry.example.com/simfleet-node:1 #x" 'VM_IMAGE=a\nb' \
	"BFB_URL=ftp://bfb.example.com/b.bfb" 'BFB_URL=http://bfb.example.com/a"b.bfb'; do
	: > "${LOG}"
	: > "${STDIN_LOG}"
	if run_installer \
		VM_IMAGE="${TEST_VM_IMAGE}" \
		BFB_URL="${TEST_BFB_URL}" \
		"${invalid}" \
		"${BASH_BIN}" "${SCENARIO_DIR}/install-passthrough-simfleet.sh" \
		> /dev/null 2>&1; then
		fail "installer accepted ${invalid}"
	fi
	if grep -q '^kubectl apply ' "${LOG}"; then
		fail "kubectl apply ran with ${invalid}"
	fi
done

# Rendering must reject manifests with anything other than one placeholder.
for placeholder_count in zero two; do
	case_dir="${TMP_DIR}/${placeholder_count}"
	mkdir -p "${case_dir}"
	cp "${SCENARIO_DIR}/install-passthrough-simfleet.sh" \
		"${case_dir}/install-passthrough-simfleet.sh"
	case "${placeholder_count}" in
	zero)
		awk '
			$0 == "    image: example.invalid/simfleet-node:set-VM_IMAGE" {
				sub("set-VM_IMAGE", "missing-placeholder")
			}
			{ print }
		' "${SCENARIO_DIR}/passthrough-simfleet.yaml" \
			> "${case_dir}/passthrough-simfleet.yaml"
		;;
	two)
		awk '
			{ print }
			$0 == "    image: example.invalid/simfleet-node:set-VM_IMAGE" {
				print
			}
		' "${SCENARIO_DIR}/passthrough-simfleet.yaml" \
			> "${case_dir}/passthrough-simfleet.yaml"
		;;
	esac

	: > "${LOG}"
	: > "${STDIN_LOG}"
	if run_installer \
		VM_IMAGE="${TEST_VM_IMAGE}" \
		BFB_URL="${TEST_BFB_URL}" \
		"${BASH_BIN}" "${case_dir}/install-passthrough-simfleet.sh" > /dev/null 2>&1; then
		fail "installer accepted a manifest with ${placeholder_count} placeholders"
	fi
	if grep -Fq 'example.invalid/simfleet-node:set-VM_IMAGE' "${STDIN_LOG}"; then
		fail "placeholder reached kubectl apply from a rejected manifest"
	fi
done

: > "${LOG}"
: > "${STDIN_LOG}"
if run_installer \
	VM_IMAGE="${TEST_VM_IMAGE}" \
	"${BASH_BIN}" "${SCENARIO_DIR}/install-passthrough-simfleet.sh" > /dev/null 2>&1; then
	fail "installer ran without BFB_URL"
fi
if grep -q '^kubectl apply ' "${LOG}"; then
	fail "kubectl apply ran without BFB_URL"
fi

# The SSH-streamed path applies the installer's inline copy, so it has to be
# the checked-in manifest byte for byte.
if ! awk "/<< ?'MANIFEST'/ { inline = 1; next } /^MANIFEST\$/ { inline = 0 } inline" \
	"${SCENARIO_DIR}/install-passthrough-simfleet.sh" \
	| cmp -s - "${SCENARIO_DIR}/passthrough-simfleet.yaml"; then
	fail "install-passthrough-simfleet.sh inlines a manifest that differs from passthrough-simfleet.yaml"
fi

echo "PASS: zero-trust passthrough scenario installers render the manifest and install the chart"
