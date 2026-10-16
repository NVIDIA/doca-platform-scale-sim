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

# Stages bin/bf3-trimmed.bfb, the trimmed BF3 bundle the Kind e2e installs.
# The file is binary and is not committed: doca-platform trims a real BFB down
# to its headers plus the Redfish software inventory, and this copies that
# result out of the revision hack/dpuagent/mock.ref pins.
# hack/dpuagent/bfb-fixture.sha256 is what makes the copy verifiable.
# `make deploy-e2e-bfb-fixtures` runs it before serving the fixture.
#
#   hack/dpuagent/sync-bfb-fixture.sh                    # stage and verify
#   BFB_FIXTURE_RECORD=true hack/dpuagent/sync-bfb-fixture.sh   # after a mock.ref bump
#
# Record mode is the second half of a mock.ref bump and nothing else: it
# rewrites the checksum, so review the new fixture with it.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=hack/dpuagent/checkout.sh
source "$ROOT/hack/dpuagent/checkout.sh"

CACHE="${DPUAGENT_CACHE_DIR:-$ROOT/.cache/dpuagent}"
FIXTURE="bf3-trimmed.bfb"
# Spelled out rather than composed: this path is the provenance, and a reader
# looking for the source of the copy has to be able to grep for it.
SOURCE_REL="test/mock/dpuagent/testdata/bf3-trimmed.bfb"
OUT="${BFB_FIXTURE_OUT:-$ROOT/bin/bf3-trimmed.bfb}"
CHECKSUM="${BFB_FIXTURE_SHA256:-$ROOT/hack/dpuagent/bfb-fixture.sha256}"
STAGED="$(mktemp)"
trap 'rm -f "$STAGED"' EXIT

sha256_of() {
	if command -v sha256sum > /dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

dpuagent_checkout "$ROOT" "$CACHE"

if [[ ! -f "$CACHE/$SOURCE_REL" ]]; then
	echo "${SOURCE_REL} missing from the pinned doca-platform revision; update hack/dpuagent/mock.ref" >&2
	exit 1
fi

# Stage first and install last. A bundle that does not match the pin must not
# reach the tree, because the e2e would then provision from a BFB no committed
# checksum describes.
cp "$CACHE/$SOURCE_REL" "$STAGED"
actual="$(sha256_of "$STAGED")"

if [[ "${BFB_FIXTURE_RECORD:-}" == "true" ]]; then
	printf '%s  %s\n' "$actual" "$FIXTURE" > "$CHECKSUM"
	echo "Recorded ${FIXTURE} checksum ${actual} in ${CHECKSUM}."
else
	expected="$(cut -d' ' -f1 < "$CHECKSUM")"
	if [[ "$actual" != "$expected" ]]; then
		echo "${SOURCE_REL} has checksum ${actual}, but ${CHECKSUM} pins ${expected}." >&2
		echo "Re-run with BFB_FIXTURE_RECORD=true if hack/dpuagent/mock.ref moved on purpose." >&2
		exit 1
	fi
fi

mkdir -p "$(dirname "$OUT")"
install -m 0644 "$STAGED" "$OUT"
echo "Synced ${FIXTURE} from ${SOURCE_REL} into ${OUT}."
