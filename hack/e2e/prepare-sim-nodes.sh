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

# Prepares the Kind cluster's node as a simulation node for the pod-runtime
# e2e: presets nf_conntrack on the Docker host, labels the node, and installs
# config/node-prep/. Run it after the node container image is loaded into Kind.

set -o nounset
set -o pipefail
set -o errexit

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
KUBECTL_BIN="${KUBECTL:-kubectl}"
DOCKER_BIN="${DOCKER:-docker}"
NODE_CONTAINER_IMG="${NODE_CONTAINER_IMG:-simfleet-node-container:local}"
NODE_PREP_DIR="${NODE_PREP_DIR:-${ROOT}/config/node-prep}"
NODE_PREP_NAMESPACE="${NODE_PREP_NAMESPACE:-simfleet-node-prep}"
NODE_PREP_TIMEOUT="${NODE_PREP_TIMEOUT:-5m}"
SIM_NODE_LABEL="scale.dpu.nvidia.com/sim-node=true"

# Kind's hostNetwork is the Kind node container's network namespace, not the
# initial one, and nf_conntrack_max is only writable from the initial one. The
# node-prep DaemonSet would fail there, so the target is applied from a
# container on the Docker host's own network namespace. node-prep.sh leaves the
# sysctl alone when it already holds, so the DaemonSet then succeeds.
#
# The target is max(32768 x CPUs, 131072) with a hash of at least a quarter of
# it, the same formula as images/simfleet-node/guest/node-prep.sh. The node
# container image carries the tools, and /lib/modules is the Docker host's, so
# modprobe works on the host kernel.
preset_conntrack() {
	echo "Presetting nf_conntrack on the Docker host."
	"${DOCKER_BIN}" run --rm --privileged --net=host -i \
		-v /lib/modules:/lib/modules:ro \
		--entrypoint bash "${NODE_CONTAINER_IMG}" -s << 'EOF'
set -euo pipefail
cpus="$(nproc)"
max=$((32768 * cpus))
if [ "${max}" -lt 131072 ]; then
	max=131072
fi
hash=$(((max + 3) / 4))
param=/sys/module/nf_conntrack/parameters/hashsize
if [ -d /sys/module/nf_conntrack ]; then
	modprobe nf_conntrack
else
	modprobe nf_conntrack "hashsize=${hash}"
fi
live="$(tr -d '[:space:]' < "${param}")"
if [ "${live}" -lt "${hash}" ]; then
	echo "${hash}" > "${param}"
fi
sysctl -w "net.netfilter.nf_conntrack_max=${max}"
echo "nf_conntrack_max=$(sysctl -n net.netfilter.nf_conntrack_max) hashsize=$(tr -d '[:space:]' < "${param}")"
EOF
}

preset_conntrack

echo "Labelling the Kind nodes as simulation nodes."
"${KUBECTL_BIN}" label nodes --all --overwrite "${SIM_NODE_LABEL}"

# The DaemonSet is privileged and uses the host network, so its namespace has to
# admit that.
echo "Installing ${NODE_PREP_DIR} into ${NODE_PREP_NAMESPACE}."
"${KUBECTL_BIN}" create namespace "${NODE_PREP_NAMESPACE}" --dry-run=client -o yaml \
	| "${KUBECTL_BIN}" apply -f -
"${KUBECTL_BIN}" label --overwrite namespace "${NODE_PREP_NAMESPACE}" \
	pod-security.kubernetes.io/enforce=privileged
"${KUBECTL_BIN}" apply -n "${NODE_PREP_NAMESPACE}" -k "${NODE_PREP_DIR}"
if ! "${KUBECTL_BIN}" rollout status daemonset/simfleet-node-prep \
	-n "${NODE_PREP_NAMESPACE}" --timeout "${NODE_PREP_TIMEOUT}"; then
	"${KUBECTL_BIN}" get pods -n "${NODE_PREP_NAMESPACE}" -o wide || true
	"${KUBECTL_BIN}" logs -n "${NODE_PREP_NAMESPACE}" -l app.kubernetes.io/name=simfleet-node-prep \
		-c node-prep --tail=-1 || true
	exit 1
fi
echo "Simulation nodes are prepared."
