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

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
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

func TestDesiredDeviceSpec(t *testing.T) {
	device := desiredDevice(inventoryFleet(), 12)

	if device.Name != "scale-3000-device-000012" || device.Namespace != "ns" {
		t.Fatalf("device identity = %s/%s", device.Namespace, device.Name)
	}
	if device.Spec.SerialNumber != "MT5799000012" {
		t.Fatalf("serialNumber = %q, want MT5799000012", device.Spec.SerialNumber)
	}
	if device.Spec.NumberOfPFs == nil || *device.Spec.NumberOfPFs != 1 {
		t.Fatalf("numberOfPFs = %v, want 1", device.Spec.NumberOfPFs)
	}
	if got := device.Labels[provisioningv1.DPUNodeNameLabel]; got != "scale-3000-host-0001" {
		t.Fatalf("dpunode-name label = %q, want scale-3000-host-0001", got)
	}
}

// The create pass runs long before the guest has an address, so a device it
// creates must not yet match the DPUSet's selector. DPF's first move on a
// device it claims is to open Redfish against spec.bmcIP, and that address is
// only known once the VMI reports one.
func TestDesiredDeviceIsNotSelectableUntilItHasABMCAddress(t *testing.T) {
	fleet := inventoryFleet()
	device := desiredDevice(fleet, 12)

	if device.Spec.BMCIP != nil {
		t.Fatalf("spec.bmcIP = %v, want unset at create", *device.Spec.BMCIP)
	}
	for key := range fleet.Spec.DeviceSelectionLabels {
		if value, published := device.Labels[key]; published {
			t.Fatalf("created device carries selection label %q=%q", key, value)
		}
	}
	// Identity is still published: the fleet has to find and adopt its own
	// devices, and DPF needs the node grouping, before any of this is
	// selectable.
	for key, want := range map[string]string{
		labelFleetName:                  fleet.Name,
		labelFleetUID:                   string(fleet.UID),
		labelOrdinal:                    "000012",
		provisioningv1.DPUNodeNameLabel: "scale-3000-host-0001",
	} {
		if device.Labels[key] != want {
			t.Fatalf("identity label %q = %q, want %q", key, device.Labels[key], want)
		}
	}
}

// Template metadata is the fleet author's own labelling, not the selector DPF
// matches on, so it is published at create like the rest of the identity.
func TestDesiredDeviceKeepsTemplateLabelsAtCreate(t *testing.T) {
	fleet := inventoryFleet()
	fleet.Spec.DPUDeviceTemplate = &scalev1alpha1.DPUDeviceTemplate{
		Metadata: scalev1alpha1.TemplateMetadata{Labels: map[string]string{"team": "scale"}},
	}
	if got := desiredDevice(fleet, 12).Labels["team"]; got != "scale" {
		t.Fatalf("template label team = %q, want scale", got)
	}
}

// The reserved keys have to be refused at the source too. Nothing should be
// able to put controller identity labels back through a selector or a device
// template.
func TestReservedDeviceLabelsAreNeverTakenFromFleetInput(t *testing.T) {
	fleet := inventoryFleet()
	reserved := map[string]string{
		labelFleetName:                  "another-fleet",
		labelFleetUID:                   "another-fleet-uid",
		labelOrdinal:                    "000999",
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

	selection := deviceSelectionLabels(fleet)
	for key := range reserved {
		if value, present := selection[key]; present {
			t.Errorf("selection labels carry reserved key %q=%q", key, value)
		}
	}
	if selection["scale.dpu.nvidia.com/pool"] != "scale-3000" {
		t.Fatalf("stripping reserved keys dropped the real selector: %#v", selection)
	}

	identity := deviceLabels(fleet, 12)
	for key, hostile := range reserved {
		if identity[key] == hostile {
			t.Errorf("device template set reserved key %q to %q", key, hostile)
		}
	}
	if identity["team"] != "scale" {
		t.Fatalf("stripping reserved keys dropped a template label: %#v", identity)
	}
}
