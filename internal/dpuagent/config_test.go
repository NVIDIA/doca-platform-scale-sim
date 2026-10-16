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

package dpuagent

import (
	mockconfig "github.com/nvidia/doca-platform/test/mock/dpuagent/pkg/config"
	"github.com/nvidia/doca-platform/test/mock/dpuagent/pkg/redfish"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

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

// startMock runs a rendered config through what mock-dpuagent does before it
// serves: the strict parse, defaulting and validation of config.Load, then
// the Redfish server construction that checks delay overrides against the
// route table.
func startMock(agent *scalev1alpha1.DPUAgentSpec) *mockconfig.Config {
	GinkgoHelper()
	rendered, err := RenderConfig(fleetWith(agent), "MT0000000000")
	Expect(err).NotTo(HaveOccurred())
	parsed, err := mockconfig.Parse([]byte(rendered))
	Expect(err).NotTo(HaveOccurred(), "mock refuses the rendered config:\n%s", rendered)
	var opts []redfish.ServerOption
	if d := parsed.BMC.ResponseDelay; d != nil {
		opts = append(opts, redfish.WithResponseDelay(d.Range()))
	}
	if overrides := parsed.BMC.ResponseDelayOverrides; len(overrides) > 0 {
		opts = append(opts, redfish.WithResponseDelayOverrides(overrides))
	}
	_, err = redfish.NewServer(nil, nil, opts...)
	Expect(err).NotTo(HaveOccurred(), "mock Redfish server refuses the rendered config:\n%s", rendered)
	return parsed
}

func renderBMC(bmc *scalev1alpha1.DPUAgentBMCSpec) *mockconfig.Config {
	GinkgoHelper()
	return startMock(&scalev1alpha1.DPUAgentSpec{BMC: bmc})
}

var _ = Describe("Mock DPU agent config", func() {
	Describe("rendering", func() {
		It("renders the defaults when the fleet has no dpuAgent", func() {
			config, err := RenderConfig(fleetWith(nil), "MT0000000000")
			Expect(err).NotTo(HaveOccurred())

			var got renderedMockDPUAgentConfig
			Expect(yaml.Unmarshal([]byte(config), &got)).To(Succeed(), "rendered config:\n%s", config)
			Expect(got.DPUType).To(Equal("bf3"))
			Expect(got.BMC.SerialNumber).To(Equal("MT0000000000"))
			Expect(got.BMC.Firmware.BMC).To(Equal("BF-24.10-17"))
			Expect(got.BMC.Firmware.EROT).To(Equal("mock-0.0"))
			Expect(got.BMC.Firmware.UEFI).To(Equal("mock-0.0"))
			Expect(got.BMC.Firmware.NIC).To(Equal("mock-0.0"))
			Expect(got.Agent.RebootMethod).To(Equal("NoAction"))
			Expect(got.Agent.DPUClusterJoin).To(Equal("kubelet"))
		})

		It("renders an empty config when the agent is disabled", func() {
			disabled := false
			config, err := RenderConfig(
				fleetWith(&scalev1alpha1.DPUAgentSpec{Enabled: &disabled}),
				"MT0000000000",
			)
			Expect(err).NotTo(HaveOccurred())
			Expect(config).To(BeEmpty())
		})

		It("renders dpuType and rebootMethod overrides", func() {
			bf4 := "bf4"
			rebootMethod := "PowerCycle,SLR"
			config, err := RenderConfig(
				fleetWith(&scalev1alpha1.DPUAgentSpec{
					DPUType:      &bf4,
					RebootMethod: &rebootMethod,
				}),
				"MT0000000042",
			)
			Expect(err).NotTo(HaveOccurred())

			var got renderedMockDPUAgentConfig
			Expect(yaml.Unmarshal([]byte(config), &got)).To(Succeed(), "rendered config:\n%s", config)
			Expect(got.DPUType).To(Equal("bf4"))
			Expect(got.BMC.SerialNumber).To(Equal("MT0000000042"))
			Expect(got.Agent.RebootMethod).To(Equal("PowerCycle,SLR"))
		})

		DescribeTable("renders the unchanged pre-bmc bytes when bmc is unset",
			func(agent *scalev1alpha1.DPUAgentSpec) {
				config, err := RenderConfig(fleetWith(agent), "MT0000000000")
				Expect(err).NotTo(HaveOccurred())
				Expect(config).To(Equal(unsetBMCMockDPUAgentConfig))
			},
			Entry("no dpuAgent", nil),
			Entry("empty dpuAgent", &scalev1alpha1.DPUAgentSpec{}),
			Entry("empty bmc", &scalev1alpha1.DPUAgentSpec{BMC: &scalev1alpha1.DPUAgentBMCSpec{}}),
			Entry("empty firmware", &scalev1alpha1.DPUAgentSpec{
				BMC: &scalev1alpha1.DPUAgentBMCSpec{Firmware: &scalev1alpha1.DPUAgentFirmware{}},
			}),
		)

		DescribeTable("renders a config the mock starts with",
			func(agent *scalev1alpha1.DPUAgentSpec) {
				startMock(agent)
			},
			Entry("defaults", nil),
			Entry("bf4", &scalev1alpha1.DPUAgentSpec{DPUType: ptr.To("bf4"), RebootMethod: ptr.To("PowerCycle,SLR")}),
			Entry("every override form", &scalev1alpha1.DPUAgentSpec{BMC: &scalev1alpha1.DPUAgentBMCSpec{
				ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
					{Name: "System", MaxSeconds: 2},
					{Name: "SecureBoot", Methods: []string{"GET"}, MaxSeconds: 1},
					{Name: "SecureBoot", Methods: []string{"PATCH"}, MaxSeconds: 3},
					{Name: "UpdateService.SimpleUpdate", Methods: []string{"*"}, MinSeconds: 20, MaxSeconds: 30},
				},
			}}),
		)

		// An unset firmware field renders the mock's own default, so the guest
		// reports exactly what a mock started without that field would.
		It("renders the mock's own firmware defaults for unset firmware", func() {
			defaults, err := mockconfig.Parse([]byte("dpuType: bf3\nagent:\n  dpuClusterJoin: kubelet\n  rebootMethod: NoAction\n"))
			Expect(err).NotTo(HaveOccurred())
			Expect(startMock(nil).BMC.Firmware).To(Equal(defaults.BMC.Firmware))
		})

		It("keeps the other firmware defaults when only one firmware field is set", func() {
			got := renderBMC(&scalev1alpha1.DPUAgentBMCSpec{
				Firmware: &scalev1alpha1.DPUAgentFirmware{BMC: ptr.To("BF-24.07-10")},
			})
			Expect(got.BMC.Firmware).To(Equal(mockconfig.Firmware{
				BMC: "BF-24.07-10", ERoT: "mock-0.0", UEFI: "mock-0.0", NIC: "mock-0.0",
			}), "want bmc overridden and the rest at mock-0.0")
		})

		It("renders every bmc setting", func() {
			got := renderBMC(&scalev1alpha1.DPUAgentBMCSpec{
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

			Expect(got.BMC.PSID).To(Equal("MT_0000009999"))
			Expect(got.BMC.Firmware).To(Equal(mockconfig.Firmware{BMC: "b", ERoT: "e", UEFI: "u", NIC: "n"}))
			Expect(got.BMC.ResponseDelay).To(Equal(&mockconfig.ResponseDelay{MinSeconds: 0, MaxSeconds: 1}))
			Expect(got.BMC.ResponseDelayOverrides).To(Equal([]mockconfig.ResponseDelayOverride{
				{Name: "System", ResponseDelay: mockconfig.ResponseDelay{MinSeconds: 1, MaxSeconds: 2}},
				{Name: "SecureBoot", Methods: []string{"PATCH"}, ResponseDelay: mockconfig.ResponseDelay{MinSeconds: 5, MaxSeconds: 8}},
			}))
		})

		// A zero range is a real setting ("never delay this"), so it must still be
		// rendered rather than dropped as empty.
		It("renders a zero delay rather than dropping it", func() {
			config, err := RenderConfig(fleetWith(&scalev1alpha1.DPUAgentSpec{
				BMC: &scalev1alpha1.DPUAgentBMCSpec{
					ResponseDelay: &scalev1alpha1.DPUAgentResponseDelay{},
					ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
						{Name: "Task"},
					},
				},
			}), "MT0000000000")
			Expect(err).NotTo(HaveOccurred())
			Expect(config).To(ContainSubstring("  responseDelay:\n    maxSeconds: 0\n    minSeconds: 0\n"))
			Expect(config).To(ContainSubstring("  - maxSeconds: 0\n    minSeconds: 0\n    name: Task\n"))
		})
	})

	Describe("spec resolution", func() {
		It("does not mutate the fleet", func() {
			enabled := false
			dpuType := "bf4"
			rebootMethod := "PowerCycle"
			fleet := fleetWith(&scalev1alpha1.DPUAgentSpec{
				Enabled:      &enabled,
				DPUType:      &dpuType,
				RebootMethod: &rebootMethod,
			})
			before := fleet.DeepCopy()

			_ = resolvedSpec(fleet)

			Expect(equality.Semantic.DeepEqual(before, fleet)).To(BeTrue(), "resolvedSpec mutated the fleet object")
		})

		It("reports the agent enabled unless it is explicitly disabled", func() {
			Expect(Enabled(fleetWith(nil))).To(BeTrue(), "nil dpuAgent defaults to enabled")
			enabled := true
			Expect(Enabled(fleetWith(&scalev1alpha1.DPUAgentSpec{Enabled: &enabled}))).To(BeTrue(), "enabled true")
			disabled := false
			Expect(Enabled(fleetWith(&scalev1alpha1.DPUAgentSpec{Enabled: &disabled}))).To(BeFalse(), "enabled false")
		})
	})
})
