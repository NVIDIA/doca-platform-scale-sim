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

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
BUILD="${ROOT}/hack/dpuagent/build.sh"
MOCK_REF="${ROOT}/hack/dpuagent/mock.ref"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

MOCK_SHA="$(tr -d '[:space:]' < "$MOCK_REF")"
GITHUB_REMOTE="https://github.com/NVIDIA/doca-platform.git"

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

assert_contains() {
	local file="$1"
	local expected="$2"
	grep -Fq -- "${expected}" "${file}" \
		|| fail "missing '${expected}' in ${file}"
}

setup_fakebin() {
	mkdir -p "$TMP/fakebin"
	cat > "$TMP/fakebin/go" << 'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${CALLS:?}"
if [[ "$1" == "build" ]]; then
  out=
  while (($#)); do
    [[ "$1" == "-o" ]] && { shift; out="$1"; }
    shift || true
  done
  : >"$out"
  chmod +x "$out"
fi
EOF
	chmod +x "$TMP/fakebin/go"

	cat > "$TMP/fakebin/git" << 'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${CALLS:?}"
if [[ "$1" == "clone" ]]; then
  dest="${@: -1}"
  mkdir -p "${dest}/.git" "${dest}/test/mock/dpuagent"
  exit 0
fi
if [[ "$1" == "-C" ]]; then
  case "$3" in
  remote)
    if [[ "$4" == "get-url" && "$5" == "origin" ]]; then
      printf '%s\n' "${FAKE_ORIGIN_URL:-https://github.com/NVIDIA/doca-platform.git}"
      exit 0
    fi
    if [[ "$4" == "set-url" && "$5" == "origin" ]]; then
      exit 0
    fi
    ;;
  fetch)
    [[ "${FETCH_FAIL:-}" == "1" ]] && exit 1
    exit 0
    ;;
  checkout|reset|clean)
    exit 0
    ;;
  esac
fi
exit 0
EOF
	chmod +x "$TMP/fakebin/git"
}

run_build() {
	local calls="$TMP/calls-$$"
	: > "$calls"
	(
		export CALLS="$calls"
		export PATH="$TMP/fakebin:$PATH"
		export DPUAGENT_CACHE_DIR="${DPUAGENT_CACHE_DIR:-$TMP/cache}"
		export DPUAGENT_OUT="${DPUAGENT_OUT:-$TMP/mock-dpuagent-$$}"
		export DPUAGENT_REPO="${DPUAGENT_REPO:-}"
		export FETCH_FAIL="${FETCH_FAIL:-}"
		export FAKE_ORIGIN_URL="${FAKE_ORIGIN_URL:-}"
		"$BUILD"
	)
	printf '%s' "$calls"
}

test_primary_path() {
	echo "== test_primary_path =="
	local cache="$TMP/cache-primary"
	rm -rf "$cache"
	local out="$TMP/mock-dpuagent-primary"
	local calls
	calls="$(DPUAGENT_CACHE_DIR="$cache" DPUAGENT_OUT="$out" run_build)"

	assert_contains "$calls" "clone --filter=blob:none --no-checkout ${GITHUB_REMOTE}"
	assert_contains "$calls" "-C ${cache} remote get-url origin"
	assert_contains "$calls" "fetch --depth=1 origin ${MOCK_SHA}"
	assert_contains "$calls" "-C ${cache} checkout --force --detach ${MOCK_SHA}"
	assert_contains "$calls" "-C ${cache} reset --hard ${MOCK_SHA}"
	assert_contains "$calls" "-C ${cache} clean -ffdx"
	assert_contains "$calls" "build -mod=readonly -trimpath -o ${out} ./test/mock/dpuagent"
	test -x "$out"
}

test_repo_override() {
	echo "== test_repo_override =="
	local cache="$TMP/cache-override"
	rm -rf "$cache"
	local mirror="https://example.com/doca-platform-mirror.git"
	local calls
	calls="$(DPUAGENT_CACHE_DIR="$cache" DPUAGENT_OUT="$TMP/mock-dpuagent-override" \
		DPUAGENT_REPO="$mirror" FAKE_ORIGIN_URL="$mirror" run_build)"

	assert_contains "$calls" "clone --filter=blob:none --no-checkout ${mirror}"
	assert_contains "$calls" "fetch --depth=1 origin ${MOCK_SHA}"
}

test_fetch_failure() {
	echo "== test_fetch_failure =="
	local cache="$TMP/cache-fetchfail"
	rm -rf "$cache"
	if CALLS="$TMP/calls-fetchfail" PATH="$TMP/fakebin:$PATH" FETCH_FAIL=1 \
		DPUAGENT_CACHE_DIR="$cache" DPUAGENT_OUT="$TMP/mock-dpuagent-fetchfail" \
		"$BUILD" > /dev/null 2>&1; then
		fail "expected build to fail when mock.ref cannot be fetched"
	fi
}

test_missing_mock_package() {
	echo "== test_missing_mock_package =="
	local cache="$TMP/cache-missing"
	rm -rf "$cache"
	mkdir -p "$TMP/fakebin-missing"
	cat > "$TMP/fakebin-missing/git" << 'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${CALLS:?}"
if [[ "$1" == "clone" ]]; then
  dest="${@: -1}"
  mkdir -p "${dest}/.git"
  exit 0
fi
if [[ "$1" == "-C" ]]; then
  case "$3" in
  remote)
    if [[ "$4" == "get-url" && "$5" == "origin" ]]; then
      printf '%s\n' "https://github.com/NVIDIA/doca-platform.git"
      exit 0
    fi
    if [[ "$4" == "set-url" && "$5" == "origin" ]]; then
      exit 0
    fi
    ;;
  fetch|checkout|reset|clean)
    exit 0
    ;;
  esac
fi
exit 0
EOF
	chmod +x "$TMP/fakebin-missing/git"

	if CALLS="$TMP/calls-missing" PATH="$TMP/fakebin-missing:$TMP/fakebin:$PATH" \
		DPUAGENT_CACHE_DIR="$cache" DPUAGENT_OUT="$TMP/mock-dpuagent-missing" \
		"$BUILD" > /dev/null 2>&1; then
		fail "expected build to fail when test/mock/dpuagent is missing"
	fi
}

test_dirty_cache_cleanup() {
	echo "== test_dirty_cache_cleanup =="
	local cache="$TMP/cache-dirty"
	rm -rf "$cache"
	mkdir -p "$cache/.git" "$cache/test/mock/dpuagent"
	echo dirty > "$cache/stale-untracked.txt"
	echo modified > "$cache/test/mock/dpuagent/main.go"

	local out="$TMP/mock-dpuagent-dirty"
	local calls
	calls="$(DPUAGENT_CACHE_DIR="$cache" DPUAGENT_OUT="$out" run_build)"

	assert_contains "$calls" "-C ${cache} reset --hard ${MOCK_SHA}"
	assert_contains "$calls" "-C ${cache} clean -ffdx"
	test -x "$out"
}

test_origin_mismatch() {
	echo "== test_origin_mismatch =="
	local cache="$TMP/cache-origin"
	rm -rf "$cache"
	mkdir -p "$cache/.git" "$cache/test/mock/dpuagent"
	local out="$TMP/mock-dpuagent-origin"
	local calls
	FAKE_ORIGIN_URL="https://example.com/wrong.git"
	calls="$(DPUAGENT_CACHE_DIR="$cache" DPUAGENT_OUT="$out" run_build)"
	unset FAKE_ORIGIN_URL

	assert_contains "$calls" "-C ${cache} remote get-url origin"
	assert_contains "$calls" "-C ${cache} remote set-url origin ${GITHUB_REMOTE}"
	test -x "$out"
}

# A build that fails must not leave the previous artifact sitting at the
# output path. The image recipe only checks that the file is non-empty, so a
# survivor from an earlier run is baked into the guest as if it were current.
test_failed_build_removes_the_stale_artifact() {
	echo "== test_failed_build_removes_the_stale_artifact =="
	local cache="$TMP/cache-stale"
	rm -rf "$cache"
	mkdir -p "$cache/.git" "$cache/test/mock/dpuagent"
	local out="$TMP/mock-dpuagent-stale"
	printf 'yesterday\n' > "$out"
	chmod +x "$out"

	mkdir -p "$TMP/fakebin-buildfail"
	cp "$TMP/fakebin/git" "$TMP/fakebin-buildfail/git"
	cat > "$TMP/fakebin-buildfail/go" << 'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${CALLS:?}"
echo "compile error" >&2
exit 1
EOF
	chmod +x "$TMP/fakebin-buildfail/go"

	if CALLS="$TMP/calls-stale" PATH="$TMP/fakebin-buildfail:$TMP/fakebin:$PATH" \
		DPUAGENT_CACHE_DIR="$cache" DPUAGENT_OUT="$out" \
		"$BUILD" > /dev/null 2>&1; then
		fail "expected build to fail when go build fails"
	fi
	if [ -e "$out" ]; then
		fail "a failed build left the stale artifact at ${out}"
	fi
}

setup_fakebin
test_primary_path
test_repo_override
test_fetch_failure
test_missing_mock_package
test_dirty_cache_cleanup
test_origin_mismatch
test_failed_build_removes_the_stale_artifact
echo "PASS: all mock-dpuagent build contract tests"
