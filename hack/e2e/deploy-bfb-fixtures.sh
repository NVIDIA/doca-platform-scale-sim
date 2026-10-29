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

KUBECTL_BIN="${KUBECTL:-kubectl}"
KIND_BIN="${KIND:-kind}"
KIND_CLUSTER="${KIND_CLUSTER:-simfleet-controller-test-e2e}"
# Not configurable, and deliberately so: the BFB object's spec.url, the
# manifest applied below and the suite's failure diagnostics all spell out this
# Service. An override here would only move the server somewhere nothing looks.
readonly NAMESPACE="dpf-operator-system"
readonly NAME="bfb-fixtures"
FIXTURE_FILE="${BFB_FIXTURE_FILE:?BFB_FIXTURE_FILE must point at the trimmed BFB}"
MANIFEST="${BFB_FIXTURES_MANIFEST:?BFB_FIXTURES_MANIFEST must point at bfb-fixtures.yaml}"
# Pinned by digest in the Makefile. The Kind node never resolves it: the host
# daemon pulls it once and `kind load` puts it in the node's image store.
SOURCE_IMAGE="${BFB_FIXTURES_SOURCE_IMAGE:?BFB_FIXTURES_SOURCE_IMAGE must be set}"
IMAGE="${BFB_FIXTURES_IMAGE:-simfleet-e2e/bfb-fixtures:busybox}"

if [[ ! -f "${FIXTURE_FILE}" ]]; then
	echo "BFB fixture ${FIXTURE_FILE} does not exist; run make deploy-e2e-bfb-fixtures, which stages it with hack/dpuagent/sync-bfb-fixture.sh" >&2
	exit 1
fi

echo "Loading the BFB fixture server image ${SOURCE_IMAGE} as ${IMAGE}..."
docker pull "${SOURCE_IMAGE}"
docker tag "${SOURCE_IMAGE}" "${IMAGE}"
# SOURCE_IMAGE is a multi-platform index. With the containerd image store, the
# daemon keeps the whole index but only the layers of its own platform, and
# `kind load docker-image` imports with --all-platforms, which fails on the
# first missing blob. Save just the platform the Kind node runs instead.
PLATFORM="$(docker version --format '{{.Server.Os}}/{{.Server.Arch}}')"
ARCHIVE_DIR="$(mktemp -d)"
trap 'rm -rf "${ARCHIVE_DIR}"' EXIT
docker save --platform "${PLATFORM}" --output "${ARCHIVE_DIR}/image.tar" "${IMAGE}"
"${KIND_BIN}" load image-archive "${ARCHIVE_DIR}/image.tar" --name "${KIND_CLUSTER}"

# The suite applies testdata/ into dpf-operator-system, which DPF already
# created. The BFB object it creates there is downloaded within seconds, so
# the server has to exist first. Creating the namespace here is a no-op when
# DPF has already installed, and keeps a local re-run from depending on
# apply order.
"${KUBECTL_BIN}" create namespace "${NAMESPACE}" --dry-run=client -o yaml \
	| "${KUBECTL_BIN}" apply -f -

# kubectl notices the file is not UTF-8 and writes it to binaryData, keyed by
# its base name, which is the path the BFB URL asks for.
echo "Publishing $(basename "${FIXTURE_FILE}") from ${FIXTURE_FILE}..."
"${KUBECTL_BIN}" create configmap "${NAME}" \
	--namespace "${NAMESPACE}" \
	--from-file="${FIXTURE_FILE}" \
	--dry-run=client -o yaml \
	| "${KUBECTL_BIN}" apply -f -

sed -e "s|BFB_FIXTURES_IMAGE|${IMAGE}|" "${MANIFEST}" | "${KUBECTL_BIN}" apply -f -

# A ConfigMap update reaches a running pod only after the kubelet sync period,
# so restart rather than wait out a stale mount on a re-run.
"${KUBECTL_BIN}" rollout restart "deployment/${NAME}" --namespace "${NAMESPACE}"
"${KUBECTL_BIN}" rollout status "deployment/${NAME}" --namespace "${NAMESPACE}" --timeout 5m

echo "BFB fixtures are served at http://${NAME}.${NAMESPACE}.svc.cluster.local/$(basename "${FIXTURE_FILE}")."
