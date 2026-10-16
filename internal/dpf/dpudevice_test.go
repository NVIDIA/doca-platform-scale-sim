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

package dpf

import (
	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

func inventoryFleet() *scalev1alpha1.SimFleet {
	return &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scale-3000",
			Namespace: "ns",
			UID:       types.UID("uid"),
		},
		Spec: scalev1alpha1.SimFleetSpec{
			Replicas:              20,
			DevicesPerDPUNode:     10,
			DeviceSelectionLabels: map[string]string{"scale.dpu.nvidia.com/pool": "scale-3000"},
		},
	}
}

var _ = Describe("DesiredDevice", func() {
	It("derives identity, serial number, PF count and node grouping from the ordinal", func() {
		device := DesiredDevice(inventoryFleet(), 12)

		Expect(device.Name).To(Equal("scale-3000-device-000012"))
		Expect(device.Namespace).To(Equal("ns"))
		Expect(device.Spec.SerialNumber).To(Equal("MT5799000012"))
		Expect(device.Spec.NumberOfPFs).NotTo(BeNil())
		Expect(*device.Spec.NumberOfPFs).To(Equal(1))
		Expect(device.Labels).To(HaveKeyWithValue(provisioningv1.DPUNodeNameLabel, "scale-3000-host-0001"))
	})

	// The create pass runs long before the guest has an address, so a device it
	// creates must not yet match the DPUSet's selector. DPF's first move on a
	// device it claims is to open Redfish against spec.bmcIP, and that address is
	// only known once the VMI reports one.
	It("is not selectable until it has a BMC address", func() {
		fleet := inventoryFleet()
		device := DesiredDevice(fleet, 12)

		Expect(device.Spec.BMCIP).To(BeNil(), "spec.bmcIP must be unset at create")
		for key := range fleet.Spec.DeviceSelectionLabels {
			Expect(device.Labels).NotTo(HaveKey(key), "created device carries selection label %q", key)
		}
		// Identity is still published: the fleet has to find and adopt its own
		// devices, and DPF needs the node grouping, before any of this is
		// selectable.
		for key, want := range map[string]string{
			naming.LabelFleetName:           fleet.Name,
			naming.LabelFleetUID:            string(fleet.UID),
			naming.LabelOrdinal:             "000012",
			provisioningv1.DPUNodeNameLabel: "scale-3000-host-0001",
		} {
			Expect(device.Labels).To(HaveKeyWithValue(key, want), "identity label %q", key)
		}
	})

	// Template metadata is the fleet author's own labelling, not the selector DPF
	// matches on, so it is published at create like the rest of the identity.
	It("keeps template labels at create", func() {
		fleet := inventoryFleet()
		fleet.Spec.DPUDeviceTemplate = &scalev1alpha1.DPUDeviceTemplate{
			Metadata: scalev1alpha1.TemplateMetadata{Labels: map[string]string{"team": "scale"}},
		}
		Expect(DesiredDevice(fleet, 12).Labels).To(HaveKeyWithValue("team", "scale"))
	})

	// The reserved keys have to be refused at the source too. Nothing should be
	// able to put controller identity labels back through a selector or a device
	// template.
	It("never takes reserved device labels from fleet input", func() {
		fleet := inventoryFleet()
		reserved := map[string]string{
			naming.LabelFleetName:           "another-fleet",
			naming.LabelFleetUID:            "another-fleet-uid",
			naming.LabelOrdinal:             "000999",
			provisioningv1.DPUNodeNameLabel: "another-host",
		}
		fleet.Spec.DeviceSelectionLabels = map[string]string{"scale.dpu.nvidia.com/pool": "scale-3000"}
		for key, value := range reserved {
			fleet.Spec.DeviceSelectionLabels[key] = value
		}
		fleet.Spec.DPUDeviceTemplate = &scalev1alpha1.DPUDeviceTemplate{
			Metadata: scalev1alpha1.TemplateMetadata{Labels: map[string]string{"team": "scale"}},
		}
		for key, value := range reserved {
			fleet.Spec.DPUDeviceTemplate.Metadata.Labels[key] = value
		}

		selection := DeviceSelectionLabels(fleet)
		for key := range reserved {
			Expect(selection).NotTo(HaveKey(key), "selection labels carry reserved key %q", key)
		}
		Expect(selection).To(HaveKeyWithValue("scale.dpu.nvidia.com/pool", "scale-3000"),
			"stripping reserved keys dropped the real selector")

		identity := DeviceLabels(fleet, 12)
		for key, hostile := range reserved {
			Expect(identity[key]).NotTo(Equal(hostile), "device template set reserved key %q", key)
		}
		Expect(identity).To(HaveKeyWithValue("team", "scale"), "stripping reserved keys dropped a template label")
	})
})
