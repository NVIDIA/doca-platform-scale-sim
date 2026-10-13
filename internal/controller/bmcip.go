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

package controller

import (
	"net/netip"

	virtv1 "kubevirt.io/api/core/v1"
)

// bridgedNetworkName is the pod network virtualmachine.go attaches, and the
// name KubeVirt reports that interface's address under.
const bridgedNetworkName = "default"

// masqueradeGatewayIP is the address KubeVirt reports for a guest whose
// interface has not produced a real address. It is the virt-launcher pod's own
// gateway, so DPF dialling it would reach the pod rather than the guest BMC.
const masqueradeGatewayIP = "10.0.2.2"

// guestBridgedIPv4 is the address DPF must open Redfish against: the IPv4 the
// guest reports on its bridged pod network. It returns "" whenever the VMI has
// not produced one this fleet is willing to publish, which is the normal state
// for most of a guest's boot.
func guestBridgedIPv4(vmi *virtv1.VirtualMachineInstance) string {
	if vmi == nil {
		return ""
	}
	for _, iface := range vmi.Status.Interfaces {
		if iface.Name != bridgedNetworkName {
			continue
		}
		address, err := netip.ParseAddr(iface.IP)
		if err != nil || !address.Is4() {
			return ""
		}
		if iface.IP == masqueradeGatewayIP {
			return ""
		}
		return iface.IP
	}
	return ""
}
