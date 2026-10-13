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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	virtv1 "kubevirt.io/api/core/v1"
)

func bmcVMI(name string, interfaces ...virtv1.VirtualMachineInstanceNetworkInterface) *virtv1.VirtualMachineInstance {
	return &virtv1.VirtualMachineInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Status:     virtv1.VirtualMachineInstanceStatus{Interfaces: interfaces},
	}
}

func TestGuestBridgedIPv4(t *testing.T) {
	tests := []struct {
		name string
		vmi  *virtv1.VirtualMachineInstance
		want string
	}{
		{name: "no VMI", vmi: nil},
		{name: "no interfaces", vmi: bmcVMI("vm")},
		{
			name: "the bridged default interface",
			vmi: bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
				Name: "default", IP: "10.1.2.3",
			}),
			want: "10.1.2.3",
		},
		{
			name: "another network is not the BMC",
			vmi: bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
				Name: "secondary", IP: "10.1.2.3",
			}),
		},
		{
			name: "the interface has not reported an address yet",
			vmi: bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
				Name: "default",
			}),
		},
		{
			// KubeVirt reports the SLIRP gateway for a VMI whose guest agent
			// has not answered. DPF would dial the pod's own gateway.
			name: "the masquerade gateway is never a BMC",
			vmi: bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
				Name: "default", IP: "10.0.2.2",
			}),
		},
		{
			name: "an IPv6 address is not a BMC address",
			vmi: bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
				Name: "default", IP: "fd10::3",
			}),
		},
		{
			name: "a malformed address is refused rather than published",
			vmi: bmcVMI("vm", virtv1.VirtualMachineInstanceNetworkInterface{
				Name: "default", IP: "not-an-address",
			}),
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := guestBridgedIPv4(testCase.vmi); got != testCase.want {
				t.Fatalf("guestBridgedIPv4() = %q, want %q", got, testCase.want)
			}
		})
	}
}
