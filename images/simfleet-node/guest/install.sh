#!/bin/bash

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

export DEBIAN_FRONTEND=noninteractive
# Kept in sync with SIMFLEET_KUBE_APT_VERSION in ../lib.sh by the version
# consistency test in test/consistency.
KUBE_APT_VERSION="${KUBE_APT_VERSION:-1.36.4-1.1}"
KUBE_VERSION="v${KUBE_APT_VERSION%%-*}"

# The base image points /etc/resolv.conf at systemd-resolved's 127.0.0.53
# stub, and resolved does not run under virt-customize, so every apt lookup
# fails with "Temporary failure resolving". The libguestfs appliance configures
# QEMU user networking as 169.254.2.0/16, whose resolver is 169.254.2.3. Use it
# for the duration of the install and put the original back afterwards, so the
# shipped guest keeps its own configuration rather than this build-time one.
RESOLV_BACKUP=""
RESOLV_WAS_LINK=""
if [ -L /etc/resolv.conf ]; then
	RESOLV_WAS_LINK="$(readlink /etc/resolv.conf)"
elif [ -e /etc/resolv.conf ]; then
	RESOLV_BACKUP=/etc/resolv.conf.simfleet-build
	cp -a /etc/resolv.conf "${RESOLV_BACKUP}"
fi
restore_resolv() {
	rm -f /etc/resolv.conf
	if [ -n "${RESOLV_WAS_LINK}" ]; then
		ln -s "${RESOLV_WAS_LINK}" /etc/resolv.conf
	elif [ -n "${RESOLV_BACKUP}" ]; then
		mv "${RESOLV_BACKUP}" /etc/resolv.conf
	fi
}
trap restore_resolv EXIT
rm -f /etc/resolv.conf
printf 'nameserver 169.254.2.3\n' > /etc/resolv.conf

apt-get update
apt-get install -y --no-install-recommends \
	apt-transport-https ca-certificates curl gpg \
	containerd \
	qemu-guest-agent

# kubeadm requires the CRI plugin enabled and runc on the systemd cgroup
# driver, which is not Ubuntu's packaged default. Regenerate the whole file so
# the result does not depend on whatever the package happened to ship.
install -d -m 0755 /etc/containerd
containerd config default > /etc/containerd/config.toml
chmod 0644 /etc/containerd/config.toml
sed -i 's/^\([[:space:]]*\)SystemdCgroup[[:space:]]*=[[:space:]]*false/\1SystemdCgroup = true/' \
	/etc/containerd/config.toml
if ! grep -q 'SystemdCgroup = true' /etc/containerd/config.toml; then
	echo "containerd config default did not expose a runc SystemdCgroup option" >&2
	exit 1
fi
if grep -q 'SystemdCgroup = false' /etc/containerd/config.toml; then
	echo "containerd config still leaves a runc runtime on the cgroupfs driver" >&2
	exit 1
fi
if grep -Eq '^[[:space:]]*disabled_plugins[[:space:]]*=.*cri' /etc/containerd/config.toml; then
	echo "containerd config disables the CRI plugin kubeadm needs" >&2
	exit 1
fi

systemctl enable containerd
# qemu-guest-agent.service is intentionally static on Ubuntu. The package's
# udev rule starts it when the org.qemu.guest_agent.0 virtio port appears.

# kubeadm preflight expects both modules loaded and all three sysctls set.
cat > /etc/modules-load.d/simfleet.conf << 'EOF'
overlay
br_netfilter
EOF
chmod 0644 /etc/modules-load.d/simfleet.conf

cat > /etc/sysctl.d/99-simfleet-k8s.conf << 'EOF'
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward = 1
EOF
chmod 0644 /etc/sysctl.d/99-simfleet-k8s.conf

install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.36/deb/Release.key \
	| gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
chmod 0644 /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.36/deb/ /' \
	> /etc/apt/sources.list.d/kubernetes.list

apt-get update
apt-get install -y --no-install-recommends \
	"kubelet=${KUBE_APT_VERSION}" \
	"kubeadm=${KUBE_APT_VERSION}" \
	"kubectl=${KUBE_APT_VERSION}" \
	kubernetes-cni

apt-mark hold kubelet kubeadm kubectl kubernetes-cni

install -d -m 0755 /etc/cni/net.d /opt/cni/bin
# kubernetes-cni ships plugins under /opt/cni/bin.

systemctl disable kubelet.service || true
systemctl stop kubelet.service || true

# The apt pins above are the only thing tying the guest to the tag the image is
# published under, so make the installed binaries prove the version they report.
for binary in kubelet kubeadm kubectl; do
	case "${binary}" in
	kubelet) reported="$(kubelet --version)" ;;
	kubeadm) reported="$(kubeadm version -o short)" ;;
	kubectl) reported="$(kubectl version --client --output=yaml)" ;;
	esac
	printf '%s: %s\n' "${binary}" "${reported}"
	if ! grep -Fq "${KUBE_VERSION}" <<< "${reported}"; then
		echo "${binary} does not report ${KUBE_VERSION}" >&2
		exit 1
	fi
done

# Drop the package indexes and downloaded archives so the wrapped disk does not
# carry ~50MB of apt state that is stale the moment the image is published.
apt-get clean
rm -rf /var/lib/apt/lists
install -d -m 0755 /var/lib/apt/lists/partial
