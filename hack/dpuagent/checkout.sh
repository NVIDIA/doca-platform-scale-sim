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

# Shared doca-platform checkout for the two things this repository takes from
# the mock: the agent binary the guest image bakes in and the trimmed BFB the
# Kind e2e installs through it. Both read hack/dpuagent/mock.ref and share one
# cache, because a mock BMC that streams a BFB from a different revision than
# its own parser is exactly the drift nothing downstream would explain.
#
# mock.ref is a commit of the public GitHub mirror, the same one go.mod
# requires github.com/nvidia/doca-platform at, so the controller's tests run
# against the code the guest runs. No other remote carries these commits, so there
# is no fallback.
#
# Source this file, then call dpuagent_checkout <repo root> <cache dir>.

DPUAGENT_GITHUB_REMOTE="https://github.com/NVIDIA/doca-platform.git"

dpuagent_checkout() {
	local root="$1"
	local cache="$2"
	local remote="${DPUAGENT_REPO:-$DPUAGENT_GITHUB_REMOTE}"
	local sha
	sha="$(tr -d '[:space:]' < "$root/hack/dpuagent/mock.ref")"

	if [[ ! -d "$cache/.git" ]]; then
		rm -rf "$cache"
		git clone --filter=blob:none --no-checkout "$remote" "$cache"
	fi

	local origin
	origin="$(git -C "$cache" remote get-url origin)"
	if [[ "$origin" != "$remote" ]]; then
		git -C "$cache" remote set-url origin "$remote"
	fi

	if ! git -C "$cache" fetch --depth=1 origin "$sha"; then
		echo "mock-dpuagent checkout: failed to fetch ${sha} from ${remote}" >&2
		return 1
	fi

	git -C "$cache" checkout --force --detach "$sha"
	git -C "$cache" reset --hard "$sha"
	git -C "$cache" clean -ffdx
}
