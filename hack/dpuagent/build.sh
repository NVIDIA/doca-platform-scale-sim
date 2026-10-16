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
# shellcheck source=hack/dpuagent/checkout.sh
source "$ROOT/hack/dpuagent/checkout.sh"
CACHE="${DPUAGENT_CACHE_DIR:-$ROOT/.cache/dpuagent}"
OUT="${DPUAGENT_OUT:-$ROOT/bin/mock-dpuagent}"

dpuagent_checkout "$ROOT" "$CACHE"

if [[ ! -d "$CACHE/test/mock/dpuagent" ]]; then
	echo "mock-dpuagent package missing; update hack/dpuagent/mock.ref" >&2
	exit 1
fi

mkdir -p "$(dirname "$OUT")"
# Drop any earlier artifact first. The image recipe only checks that this file
# is non-empty, so a survivor from a previous run would be baked into the
# guest as though this build had produced it.
rm -f "$OUT"
(cd "$CACHE" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -mod=readonly -trimpath -o "$OUT" ./test/mock/dpuagent)
