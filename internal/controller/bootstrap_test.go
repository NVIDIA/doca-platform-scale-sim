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
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/utils/ptr"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/kubevirt"
)

// Only a fleet whose guests boot with an enabled agent gets per-DPU
// credentials. Creating a ServiceAccount, Role, and RoleBinding for any other
// fleet grants management API access nothing will ever use.
var _ = Describe("credentialsEligible", func() {
	DescribeTable("is eligible only for a fleet that can issue credentials",
		func(prepare func(*scalev1alpha1.SimFleet), eligible bool) {
			fleet := fleetWith(nil)
			prepare(fleet)
			Expect(credentialsEligible(fleet)).To(Equal(eligible))
		},
		Entry("a VM template and an enabled agent", func(fleet *scalev1alpha1.SimFleet) {
			fleet.Spec.VirtualMachineTemplate = &scalev1alpha1.VirtualMachineTemplate{
				Image: "example.com/node@sha256:test",
			}
			fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: ptr.To(true)}
		}, true),
		Entry("a Pod template and an enabled agent", func(fleet *scalev1alpha1.SimFleet) {
			fleet.Spec.PodTemplate = &scalev1alpha1.PodTemplate{Image: "example.com/node@sha256:test"}
			fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: ptr.To(true)}
		}, true),
		Entry("a Pod template and a disabled agent", func(fleet *scalev1alpha1.SimFleet) {
			fleet.Spec.PodTemplate = &scalev1alpha1.PodTemplate{Image: "example.com/node@sha256:test"}
			fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: ptr.To(false)}
		}, false),
		Entry("a Pod template with no image", func(fleet *scalev1alpha1.SimFleet) {
			fleet.Spec.PodTemplate = &scalev1alpha1.PodTemplate{}
			fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: ptr.To(true)}
		}, false),
		// No VirtualMachine means no guest, and the cloud-init pass that
		// issues credentials returns before it reaches them.
		Entry("no VM template", func(fleet *scalev1alpha1.SimFleet) {
			fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: ptr.To(true)}
		}, false),
		// A guest with the agent off gets a payload with no credential in
		// it, and both units runtime-masked.
		Entry("a disabled agent", func(fleet *scalev1alpha1.SimFleet) {
			fleet.Spec.VirtualMachineTemplate = &scalev1alpha1.VirtualMachineTemplate{
				Image: "example.com/node@sha256:test",
			}
			fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: ptr.To(false)}
		}, false),
		// A template this controller refuses never produces a guest, so
		// the pass fails before issuing anything.
		Entry("a template this controller refuses", func(fleet *scalev1alpha1.SimFleet) {
			fleet.Spec.VirtualMachineTemplate = &scalev1alpha1.VirtualMachineTemplate{
				Image:   "example.com/node@sha256:test",
				Storage: &scalev1alpha1.VirtualMachineStorage{Ephemeral: ptr.To(false)},
			}
			fleet.Spec.DPUAgent = &scalev1alpha1.DPUAgentSpec{Enabled: ptr.To(true)}
		}, false),
	)
})

var _ = Describe("conditionReason", func() {
	DescribeTable("names the configuration a condition is blocked on",
		func(err error, want string) {
			Expect(conditionReason(err)).To(Equal(want))
		},
		Entry("a refused bootstrap state", &bootstrapConditionError{reason: "SomethingRefused", message: "refused"}, "SomethingRefused"),
		Entry("a refused VirtualMachine template", fmt.Errorf("render: %w", kubevirt.ErrUnsupportedStorage), "UnsupportedStorage"),
		Entry("any other error", errors.New("boom"), "ReconcileError"),
	)
})
