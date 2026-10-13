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
	"strings"
	"testing"

	mockconfig "github.com/nvidia/doca-platform/test/mock/dpuagent/pkg/config"
	"github.com/nvidia/doca-platform/test/mock/dpuagent/pkg/redfish"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

type renderedMockDPUAgentConfig struct {
	DPUType string `json:"dpuType"`
	BMC     struct {
		SerialNumber string `json:"serialNumber"`
		Firmware     struct {
			BMC  string `json:"bmc"`
			EROT string `json:"erot"`
			UEFI string `json:"uefi"`
			NIC  string `json:"nic"`
		} `json:"firmware"`
	} `json:"bmc"`
	Agent struct {
		RebootMethod   string `json:"rebootMethod"`
		DPUClusterJoin string `json:"dpuClusterJoin"`
	} `json:"agent"`
}

func fleetWith(agent *scalev1alpha1.DPUAgentSpec) *scalev1alpha1.SimFleet {
	fleet := &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "test-ns"},
		Spec: scalev1alpha1.SimFleetSpec{
			Replicas:              1,
			DevicesPerDPUNode:     1,
			DeviceSelectionLabels: map[string]string{"pool": "scale-3000"},
		},
	}
	if agent != nil {
		fleet.Spec.DPUAgent = agent
	}
	return fleet
}

func TestRenderMockDPUAgentConfigDefaults(t *testing.T) {
	config, err := renderMockDPUAgentConfig(fleetWith(nil), "MT0000000000")
	if err != nil {
		t.Fatalf("renderMockDPUAgentConfig: %v", err)
	}

	var got renderedMockDPUAgentConfig
	if err := yaml.Unmarshal([]byte(config), &got); err != nil {
		t.Fatalf("unmarshal rendered config: %v\n%s", err, config)
	}
	if got.DPUType != "bf3" ||
		got.BMC.SerialNumber != "MT0000000000" ||
		got.BMC.Firmware.BMC != "BF-24.10-17" ||
		got.BMC.Firmware.EROT != "mock-0.0" ||
		got.BMC.Firmware.UEFI != "mock-0.0" ||
		got.BMC.Firmware.NIC != "mock-0.0" ||
		got.Agent.RebootMethod != "NoAction" ||
		got.Agent.DPUClusterJoin != "kubelet" {
		t.Fatalf("unexpected rendered config: %#v", got)
	}
}

func TestRenderMockDPUAgentConfigDisabled(t *testing.T) {
	disabled := false
	config, err := renderMockDPUAgentConfig(
		fleetWith(&scalev1alpha1.DPUAgentSpec{Enabled: &disabled}),
		"MT0000000000",
	)
	if err != nil {
		t.Fatalf("renderMockDPUAgentConfig: %v", err)
	}
	if config != "" {
		t.Fatalf("disabled config = %q, want empty", config)
	}
}

func TestRenderMockDPUAgentConfigOverrides(t *testing.T) {
	bf4 := "bf4"
	rebootMethod := "PowerCycle,SLR"
	config, err := renderMockDPUAgentConfig(
		fleetWith(&scalev1alpha1.DPUAgentSpec{
			DPUType:      &bf4,
			RebootMethod: &rebootMethod,
		}),
		"MT0000000042",
	)
	if err != nil {
		t.Fatalf("renderMockDPUAgentConfig: %v", err)
	}

	var got renderedMockDPUAgentConfig
	if err := yaml.Unmarshal([]byte(config), &got); err != nil {
		t.Fatalf("unmarshal rendered config: %v\n%s", err, config)
	}
	if got.DPUType != "bf4" ||
		got.BMC.SerialNumber != "MT0000000042" ||
		got.Agent.RebootMethod != "PowerCycle,SLR" {
		t.Fatalf("overrides not rendered: %#v", got)
	}
}

func TestResolvedDPUAgentSpecDoesNotMutateFleet(t *testing.T) {
	enabled := false
	dpuType := "bf4"
	rebootMethod := "PowerCycle"
	fleet := fleetWith(&scalev1alpha1.DPUAgentSpec{
		Enabled:      &enabled,
		DPUType:      &dpuType,
		RebootMethod: &rebootMethod,
	})
	before := fleet.DeepCopy()

	_ = resolvedDPUAgentSpec(fleet)

	if !equality.Semantic.DeepEqual(before, fleet) {
		t.Fatal("resolvedDPUAgentSpec mutated the fleet object")
	}
}

func TestDPUAgentEnabled(t *testing.T) {
	if !dpuAgentEnabled(fleetWith(nil)) {
		t.Fatal("nil dpuAgent defaults to enabled")
	}
	enabled := true
	if !dpuAgentEnabled(fleetWith(&scalev1alpha1.DPUAgentSpec{Enabled: &enabled})) {
		t.Fatal("enabled true")
	}
	disabled := false
	if dpuAgentEnabled(fleetWith(&scalev1alpha1.DPUAgentSpec{Enabled: &disabled})) {
		t.Fatal("enabled false")
	}
}

// An existing fleet has no bmc block. New ordinals of it must render the
// bytes its older ordinals were created with.
const unsetBMCMockDPUAgentConfig = `agent:
  dpuClusterJoin: kubelet
  rebootMethod: NoAction
bmc:
  firmware:
    bmc: BF-24.10-17
    erot: mock-0.0
    nic: mock-0.0
    uefi: mock-0.0
  serialNumber: MT0000000000
dpuType: bf3
`

func TestRenderMockDPUAgentConfigUnsetBMCIsUnchanged(t *testing.T) {
	for name, agent := range map[string]*scalev1alpha1.DPUAgentSpec{
		"no dpuAgent":    nil,
		"empty dpuAgent": {},
		"empty bmc":      {BMC: &scalev1alpha1.DPUAgentBMCSpec{}},
		"empty firmware": {BMC: &scalev1alpha1.DPUAgentBMCSpec{Firmware: &scalev1alpha1.DPUAgentFirmware{}}},
	} {
		t.Run(name, func(t *testing.T) {
			config, err := renderMockDPUAgentConfig(fleetWith(agent), "MT0000000000")
			if err != nil {
				t.Fatalf("renderMockDPUAgentConfig: %v", err)
			}
			if config != unsetBMCMockDPUAgentConfig {
				t.Fatalf("rendered config\ngot:\n%s\nwant:\n%s", config, unsetBMCMockDPUAgentConfig)
			}
		})
	}
}

// startMock runs a rendered config through what mock-dpuagent does before it
// serves: the strict parse, defaulting and validation of config.Load, then
// the Redfish server construction that checks delay overrides against the
// route table.
func startMock(t *testing.T, agent *scalev1alpha1.DPUAgentSpec) *mockconfig.Config {
	t.Helper()
	rendered, err := renderMockDPUAgentConfig(fleetWith(agent), "MT0000000000")
	if err != nil {
		t.Fatalf("renderMockDPUAgentConfig: %v", err)
	}
	cfg, err := mockconfig.Parse([]byte(rendered))
	if err != nil {
		t.Fatalf("mock refuses the rendered config: %v\n%s", err, rendered)
	}
	var opts []redfish.ServerOption
	if d := cfg.BMC.ResponseDelay; d != nil {
		opts = append(opts, redfish.WithResponseDelay(d.Range()))
	}
	if overrides := cfg.BMC.ResponseDelayOverrides; len(overrides) > 0 {
		opts = append(opts, redfish.WithResponseDelayOverrides(overrides))
	}
	if _, err := redfish.NewServer(nil, nil, opts...); err != nil {
		t.Fatalf("mock Redfish server refuses the rendered config: %v\n%s", err, rendered)
	}
	return cfg
}

func renderBMC(t *testing.T, bmc *scalev1alpha1.DPUAgentBMCSpec) *mockconfig.Config {
	t.Helper()
	return startMock(t, &scalev1alpha1.DPUAgentSpec{BMC: bmc})
}

func TestRenderedMockDPUAgentConfigStartsTheMock(t *testing.T) {
	for name, agent := range map[string]*scalev1alpha1.DPUAgentSpec{
		"defaults": nil,
		"bf4":      {DPUType: ptr.To("bf4"), RebootMethod: ptr.To("PowerCycle,SLR")},
		"every override form": {BMC: &scalev1alpha1.DPUAgentBMCSpec{
			ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
				{Name: "System", MaxSeconds: 2},
				{Name: "SecureBoot", Methods: []string{"GET"}, MaxSeconds: 1},
				{Name: "SecureBoot", Methods: []string{"PATCH"}, MaxSeconds: 3},
				{Name: "UpdateService.SimpleUpdate", Methods: []string{"*"}, MinSeconds: 20, MaxSeconds: 30},
			},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			startMock(t, agent)
		})
	}
}

// An unset firmware field renders the mock's own default, so the guest
// reports exactly what a mock started without that field would.
func TestRenderMockDPUAgentConfigFirmwareDefaultsAreTheMocks(t *testing.T) {
	defaults, err := mockconfig.Parse([]byte("dpuType: bf3\nagent:\n  dpuClusterJoin: kubelet\n  rebootMethod: NoAction\n"))
	if err != nil {
		t.Fatalf("mockconfig.Parse: %v", err)
	}
	if got := startMock(t, nil).BMC.Firmware; got != defaults.BMC.Firmware {
		t.Fatalf("rendered firmware = %+v, mock defaults = %+v", got, defaults.BMC.Firmware)
	}
}

func TestRenderMockDPUAgentConfigPartialFirmwareKeepsTheRest(t *testing.T) {
	got := renderBMC(t, &scalev1alpha1.DPUAgentBMCSpec{
		Firmware: &scalev1alpha1.DPUAgentFirmware{BMC: ptr.To("BF-24.07-10")},
	})
	fw := got.BMC.Firmware
	if fw.BMC != "BF-24.07-10" || fw.ERoT != "mock-0.0" || fw.UEFI != "mock-0.0" || fw.NIC != "mock-0.0" {
		t.Fatalf("firmware = %#v, want bmc overridden and the rest at mock-0.0", fw)
	}
}

func TestRenderMockDPUAgentConfigBMCSettings(t *testing.T) {
	got := renderBMC(t, &scalev1alpha1.DPUAgentBMCSpec{
		PSID: ptr.To("MT_0000009999"),
		Firmware: &scalev1alpha1.DPUAgentFirmware{
			BMC: ptr.To("b"), ERoT: ptr.To("e"), UEFI: ptr.To("u"), NIC: ptr.To("n"),
		},
		ResponseDelay: &scalev1alpha1.DPUAgentResponseDelay{MinSeconds: 0, MaxSeconds: 1},
		ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
			{Name: "System", MinSeconds: 1, MaxSeconds: 2},
			{Name: "SecureBoot", Methods: []string{"PATCH"}, MinSeconds: 5, MaxSeconds: 8},
		},
	})

	if got.BMC.PSID != "MT_0000009999" {
		t.Errorf("psid = %q", got.BMC.PSID)
	}
	if fw := got.BMC.Firmware; fw.BMC != "b" || fw.ERoT != "e" || fw.UEFI != "u" || fw.NIC != "n" {
		t.Errorf("firmware = %#v", fw)
	}
	if d := got.BMC.ResponseDelay; d == nil || d.MinSeconds != 0 || d.MaxSeconds != 1 {
		t.Errorf("responseDelay = %#v, want 0..1", d)
	}
	overrides := got.BMC.ResponseDelayOverrides
	if len(overrides) != 2 {
		t.Fatalf("responseDelayOverrides = %#v, want 2 items", overrides)
	}
	if o := overrides[0]; o.Name != "System" || o.Methods != nil || o.MinSeconds != 1 || o.MaxSeconds != 2 {
		t.Errorf("override[0] = %#v, want System with no methods, 1..2", o)
	}
	if o := overrides[1]; o.Name != "SecureBoot" || len(o.Methods) != 1 || o.Methods[0] != "PATCH" ||
		o.MinSeconds != 5 || o.MaxSeconds != 8 {
		t.Errorf("override[1] = %#v, want SecureBoot PATCH 5..8", o)
	}
}

// A zero range is a real setting ("never delay this"), so it must still be
// rendered rather than dropped as empty.
func TestRenderMockDPUAgentConfigRendersZeroDelay(t *testing.T) {
	config, err := renderMockDPUAgentConfig(fleetWith(&scalev1alpha1.DPUAgentSpec{
		BMC: &scalev1alpha1.DPUAgentBMCSpec{
			ResponseDelay: &scalev1alpha1.DPUAgentResponseDelay{},
			ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
				{Name: "Task"},
			},
		},
	}), "MT0000000000")
	if err != nil {
		t.Fatalf("renderMockDPUAgentConfig: %v", err)
	}
	for _, want := range []string{
		"  responseDelay:\n    maxSeconds: 0\n    minSeconds: 0\n",
		"  - maxSeconds: 0\n    minSeconds: 0\n    name: Task\n",
	} {
		if !strings.Contains(config, want) {
			t.Errorf("rendered config missing %q:\n%s", want, config)
		}
	}
}
