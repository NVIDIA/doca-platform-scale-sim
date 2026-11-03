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

# Writes the kubelet join files for this guest, standing in for the DPF
# dpu-agent's "Configure Kubelet" step. mock-dpuagent's kubelet cluster-join
# mode only starts the kubelet unit; it does not configure kubelet, so
# something in the guest has to turn DPF's join Secret into kubelet
# configuration first. Cloud-init cannot: the join command is a bearer
# credential that DPF only publishes once the DPU exists, which is long after
# the VirtualMachine booted. It joins only once the mock has finished
# Configure Kubelet in DPU Cluster Config, where the real agent joins.
#
# The credential cloud-init installs can read exactly two things: this guest's
# DPU and that DPU's join Secret. Both reads are used. The Secret's provenance
# is checked against the DPU's live UID before its contents are fetched, and
# the contents are then validated into an argument vector rather than run as a
# shell command.
#
# Ordering against mock-dpuagent.service is deliberately absent. DPF cannot
# create the DPU, and therefore cannot create the join Secret this waits for,
# until the guest is already answering Redfish on 443. Making the mock wait
# for this helper would deadlock provisioning against itself, so the two run
# independently and both steps are idempotent:
#
#   - this script exits 0 immediately once kubelet.conf is in place;
#   - the mock's Start Kubelet step can run before the next poll sees
#     KubeletConfigured; the kubelet it starts without configuration
#     crash-loops until kubeadm writes that configuration and restarts it;
#   - the mock's Start Kubelet step is a no-op on an already running unit.
#
# The mock's simulated reboots do not restart the VM, so after the first join
# the kubelet stays up through any later Rebooting and DPU Cluster Config.
#
# It deliberately leaves kubelet running afterwards rather than stopping it so
# the mock can be the official start. Stopping a joined kubelet after the mock
# had already taken its one Start Kubelet step would leave the tenant Node
# NotReady with nothing left to recover it.

IDENTITY_FILE="${IDENTITY_FILE:-/etc/dpf-sim/identity.env}"
MGMT_KUBECONFIG="${MGMT_KUBECONFIG:-/etc/dpf-sim/mgmt.kubeconfig}"
KUBELET_CONF="${KUBELET_CONF:-/etc/kubernetes/kubelet.conf}"
JOIN_ATTEMPTS="${JOIN_ATTEMPTS:-30}"
JOIN_INTERVAL="${JOIN_INTERVAL:-10}"

log() {
	printf 'wait-kubelet-join: %s\n' "$*"
}

fail() {
	printf 'wait-kubelet-join: %s\n' "$*" >&2
	exit 1
}

# identity_value reads one KEY=value line from the cloud-init identity file.
# The file is parsed rather than sourced so a guest can never execute it.
identity_value() {
	local key="$1" line
	line="$(grep -m1 -E "^${key}=" "${IDENTITY_FILE}" || true)"
	printf '%s' "${line#*=}"
}

# join_argv is the validated argument vector built from the Secret. It is a
# global because validate_join_command fills it in.
join_argv=()

# validate_join_command accepts only the command DPF's bootstrap-token
# generator produces:
#
#   kubeadm join <host:port> --token <id>.<secret> --v=<n> \
#     --discovery-token-ca-cert-hash sha256:<hex> [...]
#
# The Secret sits at a predictable name in the management namespace, so its
# contents are treated as untrusted input: this script never eval's or
# shell-executes the value. Anything outside the grammar above, and any string
# carrying a shell metacharacter or a second line, is rejected outright, and
# what survives is executed as an explicit argument vector.
validate_join_command() {
	local command="$1" token index seen_token=0 seen_hash=0

	if [ "$(printf '%s' "${command}" | wc -l | tr -d '[:space:]')" != "0" ]; then
		return 1
	fi
	if printf '%s' "${command}" | LC_ALL=C grep -q '[^-a-zA-Z0-9_.:=/ ]'; then
		return 1
	fi

	read -r -a join_argv <<< "${command}"
	[ "${#join_argv[@]}" -ge 5 ] || return 1
	[ "${join_argv[0]}" = kubeadm ] || return 1
	[ "${join_argv[1]}" = join ] || return 1
	printf '%s' "${join_argv[2]}" | grep -Eq '^[a-z0-9._-]+:[0-9]{1,5}$' || return 1

	index=3
	while [ "${index}" -lt "${#join_argv[@]}" ]; do
		token="${join_argv[${index}]}"
		case "${token}" in
		--token)
			index=$((index + 1))
			[ "${index}" -lt "${#join_argv[@]}" ] || return 1
			printf '%s' "${join_argv[${index}]}" \
				| grep -Eq '^[a-z0-9]{6}\.[a-z0-9]{16}$' || return 1
			seen_token=1
			;;
		--discovery-token-ca-cert-hash)
			index=$((index + 1))
			[ "${index}" -lt "${#join_argv[@]}" ] || return 1
			printf '%s' "${join_argv[${index}]}" \
				| grep -Eq '^sha256:[0-9a-f]{64}$' || return 1
			seen_hash=1
			;;
		--v=*)
			printf '%s' "${token#--v=}" | grep -Eq '^[0-9]{1,2}$' || return 1
			;;
		*)
			return 1
			;;
		esac
		index=$((index + 1))
	done

	# A join without a token is not a join, and one without a CA hash would
	# trust whatever answers the API server address.
	[ "${seen_token}" -eq 1 ] && [ "${seen_hash}" -eq 1 ]
}

JOIN_ARGS_FILE="${JOIN_ARGS_FILE:-/usr/lib/dpf-sim/kubeadm-join.args}"

# append_image_join_args adds the container image's extra kubeadm arguments.
# The file is root-owned image content, not the join Secret, so its lines are
# not checked against the Secret grammar. A missing file is the VM image.
append_image_join_args() {
	[ -f "${JOIN_ARGS_FILE}" ] || return 0
	local line
	while IFS= read -r line || [ -n "${line}" ]; do
		[ -n "${line}" ] || continue
		join_argv+=("${line}")
	done < "${JOIN_ARGS_FILE}"
}

if [ "${WAIT_KUBELET_JOIN_SOURCE_ONLY:-}" = 1 ]; then
	return 0 2>/dev/null || exit 0
fi

if [ -s "${KUBELET_CONF}" ]; then
	log "kubelet is already configured at ${KUBELET_CONF}"
	exit 0
fi

[ -r "${IDENTITY_FILE}" ] || fail "identity file ${IDENTITY_FILE} is missing"
[ -s "${MGMT_KUBECONFIG}" ] \
	|| fail "management kubeconfig ${MGMT_KUBECONFIG} is missing"

dpu_name="$(identity_value DPU_NAME)"
dpu_namespace="$(identity_value DPU_NAMESPACE)"
for value in "${dpu_name}" "${dpu_namespace}"; do
	printf '%s' "${value}" \
		| grep -Eq '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$' \
		|| fail "identity file does not carry a usable DPU name and namespace"
done
join_secret="${dpu_name}-kubeadm-join"

kube() {
	kubectl --kubeconfig "${MGMT_KUBECONFIG}" --namespace "${dpu_namespace}" "$@"
}

# OWNER_MATCH_TOKEN is the API server's verdict that this Secret really is
# owned by the DPU this guest is becoming.
#
# The join Secret sits at a name derived from the DPU, which anything able to
# write in this namespace could claim first, and the controller does not read
# that Secret at all, so this is the only place provenance can be
# established. Owner reference values are never compared here as text:
# apiVersion, kind, name, and uid are all attacker-chosen strings that
# Kubernetes does not stop from containing a newline, and any scheme that
# printed them back and matched lines could be fed a forged line. Instead the
# go-template below has the API server compare the four real field values and
# emit nothing but this fixed token when they all match, so the only way the
# token can appear is if a genuine owner reference produced it.
readonly OWNER_MATCH_TOKEN="dpu-owner-match"
readonly DPU_API_VERSION="provisioning.dpu.nvidia.com/v1alpha1"

# secretTemplate renders the one request that carries everything: a secret=
# record proving the object exists, the verdict token once per owner
# reference the server matched, and the base64 payload as the final record.
#
# Reading the owner references and then reading the payload would leave a
# window between them in which the Secret that was vouched for stops being
# the Secret that gets executed, so both come out of one response. The
# payload is emitted last and read back as the terminal record, which no
# spliced record can displace.
#
# dpu_name and dpu_uid are validated above before they are interpolated, so
# neither can carry a quote or a brace into the template.
secret_template() {
	printf '%s' \
		'go-template=secret={{.metadata.name}}{{"\n"}}' \
		'{{if .metadata.ownerReferences}}{{range .metadata.ownerReferences}}' \
		"{{if and (eq .apiVersion \"${DPU_API_VERSION}\") (eq .kind \"DPU\")" \
		" (eq .name \"${dpu_name}\") (eq .uid \"$1\")}}${OWNER_MATCH_TOKEN}{{\"\\n\"}}{{end}}" \
		'{{end}}{{end}}' \
		'{{if .data}}{{if .data.join}}join={{.data.join}}{{end}}{{end}}'
}

# DPU_GATE_JSONPATH reads the DPU's UID together with the two values that say
# whether the agent has reached this step. The real agent joins in Configure
# Kubelet, inside DPU Cluster Config, so the Node must not exist before then:
# the controller would see the phase as already done and every phase delay
# up to it would gate nothing. The mock pushes KubeletConfigured=True when that
# step ends. The phase is required as well because the agent resets its
# conditions only in memory at the start of a run, so a True left by an earlier
# run can stay on the DPU until the new run's first push, which can come after
# a long OS Installing delay.
readonly DPU_GATE_JSONPATH='jsonpath={.metadata.uid}|{.status.phase}|{.status.agentStatus.conditions[?(@.type=="KubeletConfigured")].status}'
readonly DPU_GATE_OPEN="DPU Cluster Config|True"

# The DPU is read first and separately: without it there is nothing to check
# provenance against, so there is no reason to fetch a payload at all. Neither
# object exists until DPF has finished driving this guest's Redfish BMC.
#
# Only a failed read counts as an attempt. Waiting for the agent to reach
# Configure Kubelet is DPF working normally and can take as long as the fleet's
# phase delays, so it neither spends the budget nor logs on every poll.
dpu_uid=""
secret_response=""
gate=""
last_gate=""
attempt=1
kubectl_err="$(mktemp)"
trap 'rm -f "${kubectl_err}"' EXIT
while [ "${attempt}" -le "${JOIN_ATTEMPTS}" ]; do
	: > "${kubectl_err}"
	if dpu_state="$(kube get dpu "${dpu_name}" \
		--output "${DPU_GATE_JSONPATH}" 2> "${kubectl_err}")" \
		&& dpu_uid="${dpu_state%%|*}" \
		&& printf '%s' "${dpu_uid}" | grep -Eq '^[0-9a-fA-F-]{8,64}$'; then
		gate="${dpu_state#*|}"
		if [ "${gate}" != "${DPU_GATE_OPEN}" ]; then
			if [ "${gate}" != "${last_gate}" ]; then
				# The phase is a controller-written enum; anything else is
				# printed as unknown.
				phase="${gate%|*}"
				printf '%s' "${phase}" | grep -Eq '^[A-Za-z ]{0,32}$' || phase="unknown"
				log "waiting for DPU ${dpu_name} to finish Configure Kubelet (phase ${phase:-unset})"
				last_gate="${gate}"
			fi
			sleep "${JOIN_INTERVAL}"
			continue
		fi
		if secret_response="$(kube get secret "${join_secret}" \
			--output "$(secret_template "${dpu_uid}")" 2> "${kubectl_err}")" \
			&& [ -n "${secret_response}" ]; then
			break
		fi
	fi
	dpu_uid=""
	secret_response=""
	log "DPU ${dpu_name} and its join Secret are not both published yet (attempt ${attempt}/${JOIN_ATTEMPTS})"
	if [ "${attempt}" -eq "${JOIN_ATTEMPTS}" ] && [ -s "${kubectl_err}" ]; then
		# Stderr only, and only on the last try: a 403 or expired token must not
		# look like DPF lag for the rest of the guest's life, and the Secret
		# body must never reach the journal.
		log "last kubectl error: $(cat "${kubectl_err}")"
	fi
	sleep "${JOIN_INTERVAL}"
	attempt=$((attempt + 1))
done
if [ -z "${dpu_uid}" ] || [ -z "${secret_response}" ]; then
	# Exiting non-zero hands the retry back to systemd, which keeps this unit
	# trying for as long as the guest is up.
	fail "DPU ${dpu_name} and Secret ${join_secret} did not appear"
fi

# Whole-line match on a token the server alone can emit. Nothing read from
# the API is printed on any path.
if ! grep -Fxq "${OWNER_MATCH_TOKEN}" <<< "${secret_response}"; then
	fail "Secret ${join_secret} is not owned by DPU ${dpu_name}"
fi

# The payload is the terminal record by construction, so take the last line
# and require it to be one: a record spliced in through an owner reference
# lands before it and can only be ignored.
encoded="$(tail -n 1 <<< "${secret_response}")"
case "${encoded}" in
join=*) encoded="${encoded#join=}" ;;
*) encoded="" ;;
esac
if [ -z "${encoded}" ]; then
	fail "Secret ${join_secret} is missing its join key"
fi

join_command="$(printf '%s' "${encoded}" | base64 -d)"
if ! validate_join_command "${join_command}"; then
	# The command is never echoed: it carries a bootstrap token.
	fail "Secret ${join_secret} does not hold the expected kubeadm join command"
fi

# --node-name is ours, not the Secret's. DPF omits it, which would register
# this node under the guest hostname, while DPF and the e2e contract both
# expect the tenant Node to be named after the DPU.
log "joining the tenant cluster as ${dpu_name}"
append_image_join_args
"${join_argv[@]}" --node-name="${dpu_name}"
log "kubelet is configured; mock-dpuagent owns the official kubelet start"
