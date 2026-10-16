/*
Copyright 2026 NVIDIA.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubevirt

import (
	"net/netip"

	virtv1 "kubevirt.io/api/core/v1"
)

// BridgedNetworkName is the pod network DesiredVirtualMachine attaches, and the
// name KubeVirt reports that interface's address under.
const BridgedNetworkName = "default"

// masqueradeGuestNetwork is KubeVirt's default masquerade vmNetworkCIDR
// (gateway 10.0.2.1, guest 10.0.2.2). Its addresses are only reachable from
// inside the virt-launcher pod, so publishing one would point DPF at an
// address that never reaches the guest BMC.
var masqueradeGuestNetwork = netip.MustParsePrefix("10.0.2.0/24")

// GuestBridgedIPv4 is the address DPF must open Redfish against: the IPv4 the
// guest reports on its bridged pod network. It returns "" whenever the VMI has
// not produced one this fleet is willing to publish, which is the normal state
// for most of a guest's boot.
func GuestBridgedIPv4(vmi *virtv1.VirtualMachineInstance) string {
	if vmi == nil {
		return ""
	}
	for _, iface := range vmi.Status.Interfaces {
		if iface.Name != BridgedNetworkName {
			continue
		}
		address, err := netip.ParseAddr(iface.IP)
		if err != nil || !address.Is4() {
			return ""
		}
		if masqueradeGuestNetwork.Contains(address) {
			return ""
		}
		return iface.IP
	}
	return ""
}
