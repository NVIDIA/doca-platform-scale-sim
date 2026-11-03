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

package apivalidation_test

import (
	"regexp"
	"slices"

	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

const dpuAgentImmutableRule = "!has(oldSelf.dpuAgent) ? !has(self.dpuAgent) : self.dpuAgent == oldSelf.dpuAgent"

func assertDefaultJSON(props map[string]apiextensionsv1.JSONSchemaProps, field, want string) {
	GinkgoHelper()
	raw, ok := propertyDefaultRaw(props, field)
	Expect(ok).To(BeTrue(), "dpuAgent.%s missing default", field)
	Expect(string(raw)).To(Equal(want), "dpuAgent.%s default", field)
}

var _ = Describe("SimFleet API types", func() {
	It("names the inventory conditions", func() {
		Expect(scalev1alpha1.ConditionInventoryReconciled).To(Equal("InventoryReconciled"))
		Expect(scalev1alpha1.ConditionDPUsReconciled).To(Equal("DPUsReconciled"))
		Expect(scalev1alpha1.ConditionGuestsReady).To(Equal("GuestsReady"))
		Expect(scalev1alpha1.ConditionBootstrapReconciled).To(Equal("BootstrapReconciled"))
		Expect(scalev1alpha1.SimInstanceGuestReady).To(Equal("GuestReady"))
	})

	Describe("DeepCopy", func() {
		It("preserves and deep-copies the inventory spec", func() {
			pfs := 2
			spec := scalev1alpha1.SimFleetSpec{
				Replicas:              20,
				DevicesPerDPUNode:     10,
				DeviceSelectionLabels: map[string]string{"simfleet": "scale-3000"},
				DPUNodeTemplate: &scalev1alpha1.DPUNodeTemplate{
					Metadata: scalev1alpha1.TemplateMetadata{Labels: map[string]string{"role": "dpu-node"}},
					Spec: scalev1alpha1.DPUNodeTemplateSpec{
						NodeRebootMethod: &provisioningv1.NodeRebootMethod{
							External: &provisioningv1.External{},
						},
					},
				},
				DPUDeviceTemplate: &scalev1alpha1.DPUDeviceTemplate{
					Spec: scalev1alpha1.DPUDeviceTemplateSpec{NumberOfPFs: &pfs},
				},
			}

			copied := spec.DeepCopy()
			Expect(copied.Replicas).To(Equal(spec.Replicas))
			Expect(copied.DevicesPerDPUNode).To(Equal(spec.DevicesPerDPUNode))
			Expect(copied.DeviceSelectionLabels).To(HaveKeyWithValue("simfleet", "scale-3000"))
			Expect(copied.DPUNodeTemplate).NotTo(BeNil())
			Expect(copied.DPUDeviceTemplate).NotTo(BeNil())
			Expect(copied.DPUDeviceTemplate.Spec.NumberOfPFs).To(HaveValue(Equal(pfs)))

			copied.DeviceSelectionLabels["simfleet"] = "mutated"
			Expect(spec.DeviceSelectionLabels).To(HaveKeyWithValue("simfleet", "scale-3000"),
				"DeepCopy should deep-copy DeviceSelectionLabels")
		})

		It("preserves and deep-copies the inventory status", func() {
			status := scalev1alpha1.SimFleetStatus{
				ObservedGeneration:   3,
				AdmittedReplicas:     20,
				CreatedInstances:     19,
				GuestReadyInstances:  18,
				ProvisionedInstances: 17,
				FailedInstances:      1,
				DPUNodes:             2,
				Conditions: []metav1.Condition{
					{Type: scalev1alpha1.ConditionInventoryReconciled, Status: metav1.ConditionTrue},
					{Type: scalev1alpha1.ConditionDPUsReconciled, Status: metav1.ConditionFalse},
				},
			}

			copied := status.DeepCopy()
			Expect(copied.ObservedGeneration).To(BeEquivalentTo(3))
			Expect(copied.AdmittedReplicas).To(BeEquivalentTo(20))
			Expect(copied.CreatedInstances).To(BeEquivalentTo(19))
			Expect(copied.GuestReadyInstances).To(BeEquivalentTo(18))
			Expect(copied.ProvisionedInstances).To(BeEquivalentTo(17))
			Expect(copied.FailedInstances).To(BeEquivalentTo(1))
			Expect(copied.DPUNodes).To(BeEquivalentTo(2))
			Expect(copied.Conditions).To(HaveLen(2))

			copied.Conditions[0].Status = metav1.ConditionUnknown
			Expect(status.Conditions[0].Status).To(Equal(metav1.ConditionTrue),
				"DeepCopy should deep-copy Conditions")
		})

		It("preserves and deep-copies the bootstrap spec", func() {
			spec := scalev1alpha1.SimFleetSpec{
				Replicas:              1,
				DevicesPerDPUNode:     1,
				DeviceSelectionLabels: map[string]string{"simfleet": "scale-3000"},
				Bootstrap: &scalev1alpha1.BootstrapSpec{
					DPUClusterRef: scalev1alpha1.DPUClusterReference{
						Name:      "tenant1",
						Namespace: "dpf-operator-system",
					},
				},
			}

			copied := spec.DeepCopy()
			Expect(copied.Bootstrap).NotTo(BeNil())
			Expect(copied.Bootstrap.DPUClusterRef.Name).To(Equal("tenant1"))
			Expect(copied.Bootstrap.DPUClusterRef.Namespace).To(Equal("dpf-operator-system"))

			copied.Bootstrap.DPUClusterRef.Name = "mutated"
			Expect(spec.Bootstrap.DPUClusterRef.Name).To(Equal("tenant1"), "DeepCopy should deep-copy Bootstrap")
		})

		It("preserves and deep-copies the dpuAgent spec pointers", func() {
			enabled := true
			dpuType := "bf4"
			rebootMethod := "NoAction"
			spec := scalev1alpha1.SimFleetSpec{
				Replicas:              1,
				DevicesPerDPUNode:     1,
				DeviceSelectionLabels: map[string]string{"simfleet": "scale-3000"},
				DPUAgent: &scalev1alpha1.DPUAgentSpec{
					Enabled:      &enabled,
					DPUType:      &dpuType,
					RebootMethod: &rebootMethod,
				},
			}

			copied := spec.DeepCopy()
			Expect(copied.DPUAgent).NotTo(BeNil())
			Expect(copied.DPUAgent.Enabled).NotTo(BeNil())
			Expect(copied.DPUAgent.DPUType).NotTo(BeNil())
			Expect(copied.DPUAgent.RebootMethod).NotTo(BeNil())

			*copied.DPUAgent.Enabled = false
			*copied.DPUAgent.DPUType = "bf3"
			*copied.DPUAgent.RebootMethod = "External"

			Expect(*spec.DPUAgent.Enabled).To(BeTrue(), "DeepCopy should deep-copy Enabled pointer")
			Expect(*spec.DPUAgent.DPUType).To(Equal("bf4"), "DeepCopy should deep-copy DPUType pointer")
			Expect(*spec.DPUAgent.RebootMethod).To(Equal("NoAction"), "DeepCopy should deep-copy RebootMethod pointer")
		})
	})

	// The rollout index survives a restart only if the generated CRD carries it.
	// A Go field the schema prunes would look correct in memory and be lost on
	// every write.
	It("carries the rollout counts in the status schema", func() {
		status := simFleetStatusSchema()
		got := make([]string, 0, len(status.Properties))
		for name, property := range status.Properties {
			if name != "conditions" {
				Expect(property.Type).To(Equal("integer"), "status.%s type", name)
			}
			got = append(got, name)
		}
		slices.Sort(got)
		Expect(got).To(Equal([]string{
			"admittedReplicas", "conditions", "createdInstances", "dpuNodes", "failedInstances",
			"guestReadyInstances", "observedGeneration", "provisionedInstances",
		}))
	})

	It("prints the replica, rollout and readiness columns", func() {
		crd := loadSimFleetCRD()
		Expect(crd.Spec.Versions).NotTo(BeEmpty(), "SimFleet CRD has no versions")
		Expect(crd.Spec.Versions[0].AdditionalPrinterColumns).To(Equal([]apiextensionsv1.CustomResourceColumnDefinition{
			{Name: "Replicas", Type: "integer", JSONPath: ".spec.replicas"},
			{Name: "Admitted", Type: "integer", JSONPath: ".status.admittedReplicas"},
			{Name: "Provisioned", Type: "integer", JSONPath: ".status.provisionedInstances"},
			{Name: "Failed", Type: "integer", JSONPath: ".status.failedInstances"},
			{Name: "Reconciled", Type: "string", JSONPath: ".status.conditions[?(@.type=='InventoryReconciled')].status"},
			{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
		}))
	})

	// The controller filters reserved keys out of a selector, but filtering is
	// silent: a fleet author who uses one gets a device that never matches their
	// DPUSet and nothing to explain it. Rejecting the selector at admission is
	// where the author actually finds out.
	It("rejects reserved keys in deviceSelectionLabels", func() {
		spec := simFleetSpecSchema()
		Expect(spec.Properties).To(HaveKey("deviceSelectionLabels"), "spec schema is missing deviceSelectionLabels")
		selector := spec.Properties["deviceSelectionLabels"]
		Expect(selector.XValidations).NotTo(BeEmpty(), "deviceSelectionLabels has no CEL validation for reserved keys")

		rule := selector.XValidations[0]
		for _, reserved := range scalev1alpha1.ReservedDeviceLabelKeys {
			Expect(rule.Rule).To(ContainSubstring("'"+reserved+"'"),
				"deviceSelectionLabels rule does not reject reserved key %q", reserved)
			Expect(rule.Message).To(ContainSubstring(reserved),
				"deviceSelectionLabels message does not name reserved key %q", reserved)
		}
		// A rule that only listed keys would still pass the checks above if it
		// never actually looked at the selector's keys.
		Expect(rule.Rule).To(ContainSubstring("self.exists("),
			"deviceSelectionLabels rule does not test the selector's own keys")
	})

	// Nothing consumes the reserved list unless it is complete, so pin it here
	// too: this is the list the CEL rule above and the controller filter share.
	It("reserves exactly the identity labels", func() {
		Expect(scalev1alpha1.ReservedDeviceLabelKeys).To(Equal([]string{
			"scale.dpu.nvidia.com/fleet-name",
			"scale.dpu.nvidia.com/fleet-uid",
			"scale.dpu.nvidia.com/ordinal",
			"provisioning.dpu.nvidia.com/dpunode-name",
		}))
	})

	It("defaults the dpuAgent schema and makes it immutable", func() {
		spec := simFleetSpecSchema()

		Expect(hasValidationRule(spec.XValidations, dpuAgentImmutableRule, "spec.dpuAgent is immutable")).To(BeTrue(),
			"spec schema missing dpuAgent immutability validation")

		Expect(spec.Properties).To(HaveKey("dpuAgent"), "spec schema missing dpuAgent property")
		dpuAgent := spec.Properties["dpuAgent"]
		Expect(dpuAgent.Properties).NotTo(BeNil(), "dpuAgent schema missing properties")
		props := dpuAgent.Properties

		assertDefaultJSON(props, "enabled", `true`)
		assertDefaultJSON(props, "dpuType", `"bf3"`)
		assertDefaultJSON(props, "rebootMethod", `"NoAction"`)

		const rebootMethodPattern = `^(NoAction|PowerCycle|SLR)(,(NoAction|PowerCycle|SLR))*$`
		Expect(props["rebootMethod"].Pattern).To(Equal(rebootMethodPattern), "dpuAgent.rebootMethod pattern")
		re := regexp.MustCompile(props["rebootMethod"].Pattern)
		for _, value := range []string{"NoAction", "PowerCycle", "SLR", "NoAction,PowerCycle,SLR"} {
			Expect(re.MatchString(value)).To(BeTrue(), "rebootMethod pattern rejects valid value %q", value)
		}
		for _, value := range []string{"", "External", "NoAction,", "NoAction,SLRPowerCycle", " NoAction"} {
			Expect(re.MatchString(value)).To(BeFalse(), "rebootMethod pattern accepts invalid value %q", value)
		}

		for _, removed := range []string{
			"controlPlaneMTU", "nicDeviceCount", "extraArgs", "skipSysctl",
			"skipNetworkConfig", "skipDNSConfig", "skipContainerdConfig",
			"skipSFConfig", "skipVFMac", "skipOVSRawScript", "skipKernelCmdLine",
			"skipRemoveBuiltinKubelet", "skipConfigureKubelet", "skipStartKubelet",
			"skipRebootMethodDiscovery", "skipNodeLabeling", "skipAstra",
			"skipDPUMode", "skipNVConfig", "skipReboot",
		} {
			Expect(props).NotTo(HaveKey(removed), "dpuAgent schema still contains removed property %q", removed)
		}
	})
})
