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
	mockconfig "github.com/nvidia/doca-platform/test/mock/dpuagent/pkg/config"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

const (
	dpuAgentConfPath       = "/etc/mock-dpuagent/config.yaml"
	dpuAgentKubeconfigPath = "/etc/dpf-sim/mgmt.kubeconfig"
	// The units the guest image bakes in and enables. Cloud-init only ever
	// masks them; it never starts kubelet or runs a join command itself.
	mockDPUAgentUnit    = "mock-dpuagent.service"
	waitKubeletJoinUnit = "wait-kubelet-join.service"
)

type mockDPUAgentConfig struct {
	DPUType string            `json:"dpuType"`
	BMC     mockDPUAgentBMC   `json:"bmc"`
	Agent   mockDPUAgentAgent `json:"agent"`
}

type mockDPUAgentBMC struct {
	SerialNumber           string                              `json:"serialNumber"`
	PSID                   string                              `json:"psid,omitempty"`
	Firmware               mockDPUAgentFirmware                `json:"firmware"`
	ResponseDelay          *mockDPUAgentResponseDelay          `json:"responseDelay,omitempty"`
	ResponseDelayOverrides []mockDPUAgentResponseDelayOverride `json:"responseDelayOverrides,omitempty"`
}

type mockDPUAgentResponseDelay struct {
	MinSeconds int `json:"minSeconds"`
	MaxSeconds int `json:"maxSeconds"`
}

type mockDPUAgentResponseDelayOverride struct {
	Name       string   `json:"name"`
	Methods    []string `json:"methods,omitempty"`
	MinSeconds int      `json:"minSeconds"`
	MaxSeconds int      `json:"maxSeconds"`
}

type mockDPUAgentFirmware struct {
	BMC  string `json:"bmc"`
	EROT string `json:"erot"`
	UEFI string `json:"uefi"`
	NIC  string `json:"nic"`
}

type mockDPUAgentAgent struct {
	RebootMethod   string `json:"rebootMethod"`
	DPUClusterJoin string `json:"dpuClusterJoin"`
}

func resolvedDPUAgentSpec(fleet *scalev1alpha1.SimFleet) scalev1alpha1.DPUAgentSpec {
	spec := scalev1alpha1.DPUAgentSpec{}
	if fleet.Spec.DPUAgent != nil {
		spec = *fleet.Spec.DPUAgent.DeepCopy()
	}

	spec.Enabled = new(ptr.Deref(spec.Enabled, true))
	spec.DPUType = new(ptr.Deref(spec.DPUType, "bf3"))
	spec.RebootMethod = new(ptr.Deref(spec.RebootMethod, "NoAction"))

	return spec
}

func dpuAgentEnabled(fleet *scalev1alpha1.SimFleet) bool {
	return *resolvedDPUAgentSpec(fleet).Enabled
}

func renderMockDPUAgentConfig(fleet *scalev1alpha1.SimFleet, serial string) (string, error) {
	spec := resolvedDPUAgentSpec(fleet)
	if !*spec.Enabled {
		return "", nil
	}

	config := mockDPUAgentConfig{
		DPUType: *spec.DPUType,
		BMC:     renderMockDPUAgentBMC(spec.BMC, serial),
		Agent: mockDPUAgentAgent{
			RebootMethod:   *spec.RebootMethod,
			DPUClusterJoin: "kubelet",
		},
	}

	rendered, err := yaml.Marshal(config)
	return string(rendered), err
}

func renderMockDPUAgentBMC(bmc *scalev1alpha1.DPUAgentBMCSpec, serial string) mockDPUAgentBMC {
	rendered := mockDPUAgentBMC{
		SerialNumber: serial,
		Firmware: mockDPUAgentFirmware{
			BMC:  mockconfig.BMCMinSupportedVersion,
			EROT: mockconfig.PlaceholderVersion,
			UEFI: mockconfig.PlaceholderVersion,
			NIC:  mockconfig.PlaceholderVersion,
		},
	}
	if bmc == nil {
		return rendered
	}

	rendered.PSID = ptr.Deref(bmc.PSID, "")
	if fw := bmc.Firmware; fw != nil {
		rendered.Firmware.BMC = ptr.Deref(fw.BMC, rendered.Firmware.BMC)
		rendered.Firmware.EROT = ptr.Deref(fw.ERoT, rendered.Firmware.EROT)
		rendered.Firmware.UEFI = ptr.Deref(fw.UEFI, rendered.Firmware.UEFI)
		rendered.Firmware.NIC = ptr.Deref(fw.NIC, rendered.Firmware.NIC)
	}
	if d := bmc.ResponseDelay; d != nil {
		rendered.ResponseDelay = &mockDPUAgentResponseDelay{MinSeconds: d.MinSeconds, MaxSeconds: d.MaxSeconds}
	}
	for _, o := range bmc.ResponseDelayOverrides {
		rendered.ResponseDelayOverrides = append(rendered.ResponseDelayOverrides, mockDPUAgentResponseDelayOverride{
			Name:       o.Name,
			Methods:    o.Methods,
			MinSeconds: o.MinSeconds,
			MaxSeconds: o.MaxSeconds,
		})
	}
	return rendered
}
