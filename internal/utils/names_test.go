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

package utils

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Names", func() {
	It("names a device by fleet and zero-padded ordinal", func() {
		Expect(DeviceName("scale-3000", 0)).To(Equal("scale-3000-device-000000"))
	})

	DescribeTable("names an instance by fleet and zero-padded ordinal",
		func(fleet string, ordinal int, want string) {
			Expect(InstanceName(fleet, ordinal)).To(Equal(want))
		},
		Entry("fleet-a-000000", "fleet-a", 0, "fleet-a-000000"),
		Entry("fleet-a-000123", "fleet-a", 123, "fleet-a-000123"),
	)

	It("names a node by fleet and zero-padded host index", func() {
		Expect(NodeName("scale-3000", 0)).To(Equal("scale-3000-host-0000"))
	})

	It("mints a per-fleet serial number in the mock BMC format", func() {
		Expect(SerialNumber("scale-3000", 0)).To(Equal("MT5799000000"))
		Expect(SerialNumber("scale-3000", 2999)).To(Equal("MT5799002999"))
		Expect(SerialNumber("scale-3000", 0)).NotTo(Equal(SerialNumber("other-fleet", 0)),
			"two fleets minted the same serial for ordinal 0")
		for _, serial := range []string{
			SerialNumber("scale-3000", 0),
			SerialNumber("e2e-fleet", 1),
			SerialNumber("passthrough-fleet", 12),
		} {
			Expect(serial).To(MatchRegexp(`^MT[0-9]{4}[A-Z0-9-]{6}$`), "%q is not a 12-character mock BMC serial", serial)
		}
	})

	It("names a virtual machine by fleet and zero-padded ordinal", func() {
		Expect(VirtualMachineName("scale-3000", 0)).To(Equal("scale-3000-dpu-000000"))
	})

	It("names the cloud-init Secret after the virtual machine", func() {
		Expect(CloudInitSecretName("scale-3000", 0)).To(Equal("scale-3000-dpu-000000-cloud-init"))
	})

	It("names the kubeadm join Secret after the virtual machine", func() {
		Expect(KubeadmJoinSecretName("scale-3000-dpu-000000")).To(Equal("scale-3000-dpu-000000-kubeadm-join"))
	})

	DescribeTable("names a DPU by its host group and device",
		func(fleet string, devicesPerDPUNode, ordinal int, want string) {
			Expect(DPUName(fleet, devicesPerDPUNode, ordinal)).To(Equal(want))
		},
		Entry("first ordinal of the first host", "e2e-fleet", 1, 0, "e2e-fleet-host-0000-e2e-fleet-device-000000"),
		Entry("second host group", "scale-3000", 10, 12, "scale-3000-host-0001-scale-3000-device-000012"),
		Entry("unusable host grouping still names a DPU", "scale-3000", 0, 12, "scale-3000-host-0000-scale-3000-device-000012"),
	)

	It("names a DPU for an explicit host index", func() {
		Expect(DPUNameForHost("scale-3000", 1, 12)).To(Equal("scale-3000-host-0001-scale-3000-device-000012"))
		// The host index is taken as given, so a name recorded under an
		// earlier devicesPerDPUNode still resolves to the DPU DPF created.
		Expect(DPUNameForHost("scale-3000", 3, 12)).To(Equal("scale-3000-host-0003-scale-3000-device-000012"))
		Expect(DPUName("scale-3000", 10, 12)).To(Equal(DPUNameForHost("scale-3000", 1, 12)),
			"DPUName should equal DPUNameForHost of host 1")
	})

	// A DPU name is a Kubernetes object name and the tenant Node name kubelet
	// registers, so the widest fleet this controller supports must still fit in
	// the 63-character DNS label limit.
	It("fits a DPU name in a DNS label", func() {
		name := DPUName("scale-3000", 10, 2999)
		Expect(len(name)).To(BeNumerically("<=", 63), "DPUName() = %q", name)
	})

	It("parses the ordinal from a device name of the fleet only", func() {
		ord, ok := OrdinalFromDeviceName("scale-3000", "scale-3000-device-000012")
		Expect(ok).To(BeTrue())
		Expect(ord).To(Equal(12))
		_, ok = OrdinalFromDeviceName("scale-3000", "other-device-000000")
		Expect(ok).To(BeFalse())
		_, ok = OrdinalFromDeviceName("scale-3000", "scale-3000-device-abc")
		Expect(ok).To(BeFalse())
	})

	It("groups ordinals into hosts and counts the nodes needed", func() {
		Expect(HostIndex(0, 10)).To(Equal(0))
		Expect(HostIndex(9, 10)).To(Equal(0))
		Expect(HostIndex(10, 10)).To(Equal(1))
		Expect(DesiredNodeCount(0, 10)).To(Equal(0))
		Expect(DesiredNodeCount(20, 10)).To(Equal(2))
		Expect(DesiredNodeCount(21, 10)).To(Equal(3))
	})

	// A CRD minimum keeps devicesPerDPUNode >= 1, but callers reach HostIndex with
	// values read from unvalidated objects, so it must not divide by zero.
	It("returns host 0 for a non-positive devicesPerDPUNode", func() {
		Expect(HostIndex(1234, 0)).To(Equal(0))
		Expect(HostIndex(1234, -5)).To(Equal(0))
	})

	It("returns each host's ordinal range, clipped to the replica count", func() {
		start, end := HostOrdinalRange(0, 10, 20)
		Expect([]int{start, end}).To(Equal([]int{0, 10}), "host 0 range")
		start, end = HostOrdinalRange(1, 10, 15)
		Expect([]int{start, end}).To(Equal([]int{10, 15}), "partial last host range")
	})
})
