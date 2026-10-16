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

// Package naming derives the deterministic names and identity labels of the
// objects a fleet creates, and maps device ordinals onto DPUNode hosts.
package naming

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
)

// DeviceName is the deterministic DPUDevice name for a fleet ordinal:
// {fleet}-device-{ordinal:06d}.
func DeviceName(fleet string, ordinal int) string {
	return fmt.Sprintf("%s-device-%06d", fleet, ordinal)
}

// InstanceName is the SimInstance for a fleet ordinal: {fleet}-{ordinal:06d}.
func InstanceName(fleet string, ordinal int) string {
	return fmt.Sprintf("%s-%06d", fleet, ordinal)
}

// NodeName is the deterministic DPUNode name for a host index:
// {fleet}-host-{hostIndex:04d}.
func NodeName(fleet string, hostIndex int) string {
	return fmt.Sprintf("%s-host-%04d", fleet, hostIndex)
}

// SerialNumber is the DPUDevice serial for a fleet ordinal. The mock BMC
// accepts only MT plus four digits plus six upper-case letters, digits, or
// '-': twelve characters. The four digits are a hash of the fleet name so
// two fleets do not mint the same serial; the last six are the ordinal.
func SerialNumber(fleet string, ordinal int) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(fleet))
	return fmt.Sprintf("MT%04d%06d", h.Sum32()%10000, ordinal)
}

// VirtualMachineName is the deterministic KubeVirt VirtualMachine name for a fleet ordinal:
// {fleet}-dpu-{ordinal:06d}.
func VirtualMachineName(fleet string, ordinal int) string {
	return fmt.Sprintf("%s-dpu-%06d", fleet, ordinal)
}

// CloudInitSecretName is the cloud-init Secret for a fleet ordinal:
// {fleet}-dpu-{ordinal:06d}-cloud-init.
func CloudInitSecretName(fleet string, ordinal int) string {
	return VirtualMachineName(fleet, ordinal) + "-cloud-init"
}

// KubeadmJoinSecretName is the per-DPU kubeadm join Secret: {dpu}-kubeadm-join.
func KubeadmJoinSecretName(dpuName string) string {
	return dpuName + "-kubeadm-join"
}

// DPUName is the name DPF's DPUSet gives the DPU for a fleet ordinal:
// {node}-{device}. It is derived from names this controller already owns, so a
// guest can be told which DPU it will become, and which join Secret to read,
// before DPF has created that DPU.
func DPUName(fleet string, devicesPerDPUNode, ordinal int) string {
	return DPUNameForHost(fleet, HostIndex(ordinal, devicesPerDPUNode), ordinal)
}

// DPUNameForHost is DPUName for an ordinal whose host index is already known.
func DPUNameForHost(fleet string, hostIndex, ordinal int) string {
	return NodeName(fleet, hostIndex) + "-" + DeviceName(fleet, ordinal)
}

// OrdinalFromDeviceName parses a DPUDevice name back to its ordinal when it belongs to fleet.
func OrdinalFromDeviceName(fleet, deviceName string) (int, bool) {
	prefix := fleet + "-device-"
	if !strings.HasPrefix(deviceName, prefix) {
		return 0, false
	}
	ordinal, err := strconv.Atoi(strings.TrimPrefix(deviceName, prefix))
	if err != nil || ordinal < 0 {
		return 0, false
	}
	if DeviceName(fleet, ordinal) != deviceName {
		return 0, false
	}
	return ordinal, true
}

// HostIndex maps a device ordinal onto its DPUNode: ordinal / devicesPerDPUNode.
// A non-positive devicesPerDPUNode cannot describe a host group, so it maps
// onto host 0.
func HostIndex(ordinal, devicesPerDPUNode int) int {
	if devicesPerDPUNode <= 0 {
		return 0
	}
	return ordinal / devicesPerDPUNode
}

// DesiredNodeCount is ceil(replicas / devicesPerDPUNode), or 0 when replicas is 0.
func DesiredNodeCount(replicas, devicesPerDPUNode int) int {
	if replicas == 0 {
		return 0
	}
	return (replicas + devicesPerDPUNode - 1) / devicesPerDPUNode
}

// HostOrdinalRange is the half-open [start, end) device ordinals owned by hostIndex.
// The last host may have fewer than devicesPerDPUNode devices.
func HostOrdinalRange(hostIndex, devicesPerDPUNode, replicas int) (start, end int) {
	start = hostIndex * devicesPerDPUNode
	end = min(start+devicesPerDPUNode, replicas)
	return start, end
}
