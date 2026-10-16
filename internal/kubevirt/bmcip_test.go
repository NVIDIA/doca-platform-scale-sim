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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	virtv1 "kubevirt.io/api/core/v1"
)

func bmcVMI(name string, interfaces ...virtv1.VirtualMachineInstanceNetworkInterface) *virtv1.VirtualMachineInstance {
	return &virtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status:     virtv1.VirtualMachineInstanceStatus{Interfaces: interfaces},
	}
}

var _ = Describe("BMC address selection", func() {
	DescribeTable("selects the BMC address a VMI publishes",
		func(vmi *virtv1.VirtualMachineInstance, want string) {
			Expect(GuestBridgedIPv4(vmi)).To(Equal(want))
		},
		Entry("no VMI", nil, ""),
		Entry("no interfaces", bmcVMI("vm"), ""),
		Entry("the bridged default interface", bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
			Name: "default", IP: "10.1.2.3",
		}), "10.1.2.3"),
		Entry("another network is not the BMC", bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
			Name: "secondary", IP: "10.1.2.3",
		}), ""),
		Entry("the interface has not reported an address yet", bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
			Name: "default",
		}), ""),
		// Masquerade guest addresses only exist inside the virt-launcher pod.
		Entry("the default masquerade guest address is never a BMC", bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
			Name: "default", IP: "10.0.2.2",
		}), ""),
		Entry("any address in the masquerade network is never a BMC", bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
			Name: "default", IP: "10.0.2.57",
		}), ""),
		Entry("an IPv6 address is not a BMC address", bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
			Name: "default", IP: "fd10::3",
		}), ""),
		Entry("a malformed address is refused rather than published", bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
			Name: "default", IP: "not-an-address",
		}), ""),
	)
})
