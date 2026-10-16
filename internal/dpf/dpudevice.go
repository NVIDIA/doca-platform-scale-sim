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

// Package dpf builds the DPF inventory objects, DPUDevices and DPUNodes, that
// stand in for a fleet's simulated hardware.
package dpf

import (
	"fmt"
	"maps"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

const numberOfPFsDefault = 1

// DesiredDevice is the DPUDevice for ordinal, without spec.bmcIP and without
// the selection labels; see DeviceLabels.
func DesiredDevice(fleet *scalev1alpha1.SimFleet, ordinal int) *provisioningv1.DPUDevice {
	numberOfPFs := numberOfPFsDefault
	if fleet.Spec.DPUDeviceTemplate != nil && fleet.Spec.DPUDeviceTemplate.Spec.NumberOfPFs != nil {
		numberOfPFs = *fleet.Spec.DPUDeviceTemplate.Spec.NumberOfPFs
	}
	return &provisioningv1.DPUDevice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      naming.DeviceName(fleet.Name, ordinal),
			Namespace: fleet.Namespace,
			Labels:    DeviceLabels(fleet, ordinal),
		},
		Spec: provisioningv1.DPUDeviceSpec{
			SerialNumber: naming.SerialNumber(fleet.Name, ordinal),
			NumberOfPFs:  &numberOfPFs,
		},
	}
}

// withoutReservedDeviceLabels copies labels, dropping the keys only the
// controller may set. It never mutates its argument, which is fleet spec.
// The CRD rejects a selector that uses one, so filtering here is what covers
// the DPUDevice template, which CEL does not reach, and any object admitted
// before that rule existed.
func withoutReservedDeviceLabels(labels map[string]string) map[string]string {
	allowed := make(map[string]string, len(labels))
	maps.Copy(allowed, labels)
	for _, reserved := range scalev1alpha1.ReservedDeviceLabelKeys {
		delete(allowed, reserved)
	}
	return allowed
}

// DeviceLabels is everything a SimInstance puts on its DPUDevice when it
// creates or repairs it: who owns it, which ordinal it is, and which DPUNode
// it belongs to. It deliberately excludes spec.deviceSelectionLabels. Those
// are what DPF's DPUSet matches on, and a device DPF claims is a device it
// immediately opens Redfish against, so the controller publishes them only
// together with the address that makes Redfish reachable.
func DeviceLabels(fleet *scalev1alpha1.SimFleet, ordinal int) map[string]string {
	labels := map[string]string{}
	if fleet.Spec.DPUDeviceTemplate != nil {
		maps.Copy(labels, withoutReservedDeviceLabels(fleet.Spec.DPUDeviceTemplate.Metadata.Labels))
	}
	maps.Copy(labels, naming.InventoryLabels(fleet, fmt.Sprintf("%06d", ordinal)))
	labels[provisioningv1.DPUNodeNameLabel] = naming.NodeName(
		fleet.Name,
		naming.HostIndex(ordinal, fleet.Spec.DevicesPerDPUNode),
	)
	return labels
}

// DeviceSelectionLabels is the selector DPF's DPUSet matches. It must only be
// written alongside spec.bmcIP. A selector is fleet-author input, so it cannot
// carry a reserved identity key.
func DeviceSelectionLabels(fleet *scalev1alpha1.SimFleet) map[string]string {
	return withoutReservedDeviceLabels(fleet.Spec.DeviceSelectionLabels)
}
