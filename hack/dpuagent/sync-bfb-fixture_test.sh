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

# Contract test for sync-bfb-fixture.sh. The script talks to git and a remote,
# so the real thing cannot run in the unit-test job; a fake git stands in and
# the assertions are about what the script asked it for and what it installed.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SYNC="${ROOT}/hack/dpuagent/sync-bfb-fixture.sh"
MOCK_REF="${ROOT}/hack/dpuagent/mock.ref"
CHECKSUM="${ROOT}/hack/dpuagent/bfb-fixture.sha256"
SOURCE_REL="test/mock/dpuagent/testdata/bf3-trimmed.bfb"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

MOCK_SHA="$(tr -d '[:space:]' < "$MOCK_REF")"

# The real fixture is not committed, so the happy path copies a stand-in with a
# checksum file recorded for it.
FAKE_FIXTURE="$TMP/fake.bfb"
FAKE_CHECKSUM="$TMP/fake.sha256"

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

sha256_of() {
	if command -v sha256sum > /dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

# setup_fakebin installs a git that records its arguments and materialises the
# doca-platform tree the script expects, with the fixture taken from
# ${FAKE_SOURCE}. No network and no real checkout are involved.
setup_fakebin() {
	mkdir -p "$TMP/fakebin"
	cat > "$TMP/fakebin/git" << 'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${CALLS:?}"
plant() {
  local dest="$1"
  if [[ -n "${FAKE_SOURCE:-}" ]]; then
    mkdir -p "${dest}/test/mock/dpuagent/testdata"
    cp "${FAKE_SOURCE}" "${dest}/test/mock/dpuagent/testdata/bf3-trimmed.bfb"
  fi
}
if [[ "$1" == "clone" ]]; then
  dest="${@: -1}"
  mkdir -p "${dest}/.git"
  plant "${dest}"
  exit 0
fi
if [[ "$1" == "-C" ]]; then
  case "$3" in
  remote)
    if [[ "$4" == "get-url" && "$5" == "origin" ]]; then
      printf '%s\n' "https://github.com/NVIDIA/doca-platform.git"
      exit 0
    fi
    exit 0
    ;;
  checkout)
    plant "$2"
    exit 0
    ;;
  fetch | reset | clean)
    exit 0
    ;;
  esac
fi
exit 0
EOF
	chmod +x "$TMP/fakebin/git"
}

# run_sync runs the script against a throwaway cache and output path and
# prints the file the fake git recorded its calls in.
run_sync() {
	local calls="$TMP/calls-$$"
	local status=0
	: > "$calls"
	(
		export CALLS="$calls"
		export PATH="$TMP/fakebin:$PATH"
		export DPUAGENT_CACHE_DIR="${DPUAGENT_CACHE_DIR:?}"
		export BFB_FIXTURE_OUT="${BFB_FIXTURE_OUT:?}"
		export BFB_FIXTURE_SHA256="${BFB_FIXTURE_SHA256:-$CHECKSUM}"
		export BFB_FIXTURE_RECORD="${BFB_FIXTURE_RECORD:-}"
		export FAKE_SOURCE="${FAKE_SOURCE:-}"
		# The script's own progress goes to stderr so that stdout stays the path
		# of the call log.
		"$SYNC" >&2
	) || status=$?
	printf '%s' "$calls"
	return "$status"
}

test_checksum_names_the_fixture() {
	echo "== test_checksum_names_the_fixture =="
	grep -Eq -- "^[0-9a-f]{64}  bf3-trimmed\.bfb$" "$CHECKSUM" \
		|| fail "${CHECKSUM} does not pin bf3-trimmed.bfb"
}

test_copies_the_pinned_fixture() {
	echo "== test_copies_the_pinned_fixture =="
	local cache="$TMP/cache-happy"
	local out="$TMP/out-happy.bfb"
	rm -rf "$cache" "$out"
	local calls
	calls="$(DPUAGENT_CACHE_DIR="$cache" BFB_FIXTURE_OUT="$out" FAKE_SOURCE="$FAKE_FIXTURE" \
		BFB_FIXTURE_SHA256="$FAKE_CHECKSUM" run_sync)"

	assert_contains "$calls" "fetch --depth=1 origin ${MOCK_SHA}"
	assert_contains "$calls" "-C ${cache} checkout --force --detach ${MOCK_SHA}"
	assert_contains "$calls" "-C ${cache} reset --hard ${MOCK_SHA}"
	assert_contains "$calls" "-C ${cache} clean -ffdx"
	cmp -s "$out" "$FAKE_FIXTURE" \
		|| fail "synced fixture differs from the pinned source"
}

test_rejects_a_fixture_that_does_not_match_the_pin() {
	echo "== test_rejects_a_fixture_that_does_not_match_the_pin =="
	local cache="$TMP/cache-mismatch"
	local out="$TMP/out-mismatch.bfb"
	local wrong="$TMP/wrong.bfb"
	rm -rf "$cache"
	printf 'not a bfb' > "$wrong"
	printf 'untouched' > "$out"

	if DPUAGENT_CACHE_DIR="$cache" BFB_FIXTURE_OUT="$out" FAKE_SOURCE="$wrong" \
		run_sync > /dev/null 2>&1; then
		fail "expected the sync to fail on a checksum mismatch"
	fi
	# A mismatch must never reach the e2e: the e2e would then install a BFB
	# nothing in this repository describes.
	[[ "$(cat "$out")" == "untouched" ]] \
		|| fail "a mismatching fixture overwrote the destination"
}

test_fails_when_the_mock_testdata_is_gone() {
	echo "== test_fails_when_the_mock_testdata_is_gone =="
	local cache="$TMP/cache-missing"
	local out="$TMP/out-missing.bfb"
	rm -rf "$cache" "$out"

	# FAKE_SOURCE empty means the fake git plants no testdata at all, which is
	# what a mock.ref bump past a renamed fixture looks like.
	if DPUAGENT_CACHE_DIR="$cache" BFB_FIXTURE_OUT="$out" FAKE_SOURCE="" \
		run_sync > /dev/null 2>&1; then
		fail "expected the sync to fail when ${SOURCE_REL} is missing"
	fi
	[[ ! -f "$out" ]] || fail "a missing source still produced ${out}"
}

test_records_a_new_checksum_on_request() {
	echo "== test_records_a_new_checksum_on_request =="
	local cache="$TMP/cache-record"
	local out="$TMP/out-record.bfb"
	local checksum="$TMP/record.sha256"
	local new="$TMP/new.bfb"
	rm -rf "$cache" "$out"
	printf 'a new trimmed bfb' > "$new"
	printf 'stale  bf3-trimmed.bfb\n' > "$checksum"

	DPUAGENT_CACHE_DIR="$cache" BFB_FIXTURE_OUT="$out" FAKE_SOURCE="$new" \
		BFB_FIXTURE_SHA256="$checksum" BFB_FIXTURE_RECORD=true run_sync > /dev/null

	cmp -s "$out" "$new" || fail "record mode did not install the new fixture"
	assert_contains "$checksum" "$(sha256_of "$new")"
	assert_contains "$checksum" "bf3-trimmed.bfb"
}

setup_fakebin
printf "Bf\x02\x13fake" > "$FAKE_FIXTURE"
printf "%s  bf3-trimmed.bfb\n" "$(sha256_of "$FAKE_FIXTURE")" > "$FAKE_CHECKSUM"
test_checksum_names_the_fixture
test_copies_the_pinned_fixture
test_rejects_a_fixture_that_does_not_match_the_pin
test_fails_when_the_mock_testdata_is_gone
test_records_a_new_checksum_on_request
echo "PASS: all BFB fixture sync contract tests"
