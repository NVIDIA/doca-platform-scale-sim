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

HELM_BIN="${HELM:?HELM is required}"
KUBECTL_BIN="${KUBECTL:?KUBECTL is required}"
# An HTTP chart repository, as NGC publishes DPF.
DPF_HELM_REPO="${DPF_HELM_REPO:?DPF_HELM_REPO is required}"
DPF_HELM_CHART="${DPF_HELM_CHART:?DPF_HELM_CHART is required}"
DPF_HELM_VERSION="${DPF_HELM_VERSION:?DPF_HELM_VERSION is required}"
DPF_OPERATOR_NAMESPACE="${DPF_OPERATOR_NAMESPACE:?DPF_OPERATOR_NAMESPACE is required}"
DPF_OPERATOR_RELEASE="${DPF_OPERATOR_RELEASE:?DPF_OPERATOR_RELEASE is required}"
DPF_OPERATOR_CONFIG="${DPF_OPERATOR_CONFIG:?DPF_OPERATOR_CONFIG is required}"

echo "Installing DPF operator ${DPF_HELM_REPO}/${DPF_HELM_CHART}:${DPF_HELM_VERSION} into ${DPF_OPERATOR_NAMESPACE}..."
"${HELM_BIN}" upgrade --install "${DPF_OPERATOR_RELEASE}" "${DPF_HELM_CHART}" \
	--repo "${DPF_HELM_REPO}" \
	--version "${DPF_HELM_VERSION}" \
	--namespace "${DPF_OPERATOR_NAMESPACE}" \
	--create-namespace \
	--wait \
	--timeout 10m \
	--set kubeStateMetricsCRDMetrics.enabled=false \
	--set grafanaDashboards.enabled=false \
	--set prometheusSecureMetrics.enabled=false

"${KUBECTL_BIN}" rollout status deployment/dpf-operator-controller-manager \
	--namespace "${DPF_OPERATOR_NAMESPACE}" \
	--timeout 5m

dump_dpf_state() {
	echo "=== DPFOperatorConfig conditions ==="
	"${KUBECTL_BIN}" get dpfoperatorconfig dpfoperatorconfig \
		--namespace "${DPF_OPERATOR_NAMESPACE}" \
		-o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{.message}{"\n\n"}{end}' || true
	echo "=== Pods in ${DPF_OPERATOR_NAMESPACE} ==="
	"${KUBECTL_BIN}" get pods --namespace "${DPF_OPERATOR_NAMESPACE}" -o wide || true
	echo "=== DPF operator logs (last 200 lines) ==="
	"${KUBECTL_BIN}" logs deployment/dpf-operator-controller-manager \
		--namespace "${DPF_OPERATOR_NAMESPACE}" --tail 200 || true

	# Any component whose Pods never become ready keeps the config out of Ready,
	# so describe those Pods and drain the logs of every container in them,
	# including the run that already exited.
	local pod
	for pod in $("${KUBECTL_BIN}" get pods --namespace "${DPF_OPERATOR_NAMESPACE}" \
		-o jsonpath='{range .items[?(@.status.phase!="Succeeded")]}{.metadata.name}{"\t"}{range .status.containerStatuses[*]}{.ready}{","}{end}{"\n"}{end}' \
		| awk '$2 == "" || $2 ~ /false/ {print $1}'); do
		echo "=== Describe pod ${pod} ==="
		"${KUBECTL_BIN}" describe pod "${pod}" --namespace "${DPF_OPERATOR_NAMESPACE}" || true
		local container
		for container in $("${KUBECTL_BIN}" get pod "${pod}" --namespace "${DPF_OPERATOR_NAMESPACE}" \
			-o jsonpath='{range .spec.containers[*]}{.name}{"\n"}{end}'); do
			echo "=== Logs ${pod}/${container} (last 100 lines) ==="
			"${KUBECTL_BIN}" logs "${pod}" -c "${container}" \
				--namespace "${DPF_OPERATOR_NAMESPACE}" --tail 100 || true
			echo "=== Previous logs ${pod}/${container} (last 100 lines) ==="
			"${KUBECTL_BIN}" logs "${pod}" -c "${container}" --previous \
				--namespace "${DPF_OPERATOR_NAMESPACE}" --tail 100 || true
		done
	done
}

# The endpoint of the default kubernetes Service is the API server address
# the DPF components have to reach, and it is only known once Kind has
# created the cluster.
api_server_vip="$("${KUBECTL_BIN}" get endpointslice kubernetes --namespace default \
	-o jsonpath='{.endpoints[0].addresses[0]}')"
api_server_port="$("${KUBECTL_BIN}" get endpointslice kubernetes --namespace default \
	-o jsonpath='{.ports[0].port}')"
if [[ -z "${api_server_vip}" || -z "${api_server_port}" ]]; then
	echo "Could not read the kubernetes Service endpoint" >&2
	exit 1
fi

echo "Applying DPFOperatorConfig from ${DPF_OPERATOR_CONFIG} with API server ${api_server_vip}:${api_server_port}..."
sed -e "s/KUBERNETES_API_SERVER_VIP/${api_server_vip}/" \
	-e "s/KUBERNETES_API_SERVER_PORT/${api_server_port}/" \
	"${DPF_OPERATOR_CONFIG}" | "${KUBECTL_BIN}" apply -f -
if ! "${KUBECTL_BIN}" wait --for=condition=Ready \
	dpfoperatorconfig/dpfoperatorconfig \
	--namespace "${DPF_OPERATOR_NAMESPACE}" \
	--timeout 10m; then
	# The cluster is torn down by the job's after_script, so the reason has to
	# be captured here or it is lost.
	dump_dpf_state
	exit 1
fi

echo "DPF operator ${DPF_HELM_VERSION} is installed."
