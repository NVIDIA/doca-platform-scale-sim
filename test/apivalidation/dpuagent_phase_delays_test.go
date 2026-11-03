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
	"context"
	"slices"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

var _ = Describe("SimFleet spec.dpuAgent.phaseDelays schema", func() {
	phaseDelays := func() apiextensionsv1.JSONSchemaProps {
		GinkgoHelper()
		dpuAgent := simFleetSpecSchema().Properties["dpuAgent"]
		Expect(dpuAgent.Properties).To(HaveKey("phaseDelays"), "dpuAgent schema missing phaseDelays")
		return dpuAgent.Properties["phaseDelays"]
	}

	It("is a list keyed by phase with bounded, required fields", func() {
		list := phaseDelays()
		Expect(list.XListType).To(HaveValue(Equal("map")), "phaseDelays list type")
		Expect(list.XListMapKeys).To(Equal([]string{"phase"}), "phaseDelays list map keys")
		Expect(list.Default).To(BeNil(), "phaseDelays must not default")
		Expect(list.Items).NotTo(BeNil(), "phaseDelays has no item schema")
		Expect(list.Items.Schema).NotTo(BeNil(), "phaseDelays has no item schema")

		item := *list.Items.Schema
		Expect(item.Required).To(ConsistOf("phase", "minSeconds", "maxSeconds"))
		for _, name := range []string{"minSeconds", "maxSeconds"} {
			s := item.Properties[name]
			Expect(s.Type).To(Equal("integer"), "phaseDelays[].%s type", name)
			Expect(s.Format).To(Equal("int32"), "phaseDelays[].%s format", name)
			Expect(s.Minimum).To(HaveValue(BeZero()), "phaseDelays[].%s minimum", name)
			Expect(s.Maximum).To(HaveValue(BeEquivalentTo(3600)), "phaseDelays[].%s maximum", name)
		}
	})

	It("enumerates exactly MockDelayablePhases", func() {
		item := *phaseDelays().Items.Schema
		Expect(enumStrings(item.Properties["phase"].Enum)).To(Equal(slices.Sorted(slices.Values(scalev1alpha1.MockDelayablePhases))),
			"phaseDelays[].phase enum")
	})
})

var _ = Describe("SimFleet spec.dpuAgent.phaseDelays admission", func() {
	ctx := context.Background()
	var namespace string

	// No manager runs here: these specs only exercise API server admission.
	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "phase-delays-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name
	})

	createFleet := func(delays ...scalev1alpha1.DPUAgentPhaseDelay) (*scalev1alpha1.SimFleet, error) {
		fleet := &scalev1alpha1.SimFleet{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "phase-delays-", Namespace: namespace},
			Spec: scalev1alpha1.SimFleetSpec{
				Replicas:              0,
				DevicesPerDPUNode:     1,
				DeviceSelectionLabels: map[string]string{"scale.dpu.nvidia.com/pool": "phase-delays"},
				DPUAgent:              &scalev1alpha1.DPUAgentSpec{PhaseDelays: delays},
			},
		}
		err := k8sClient.Create(ctx, fleet)
		if err == nil {
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, fleet))).To(Succeed())
			})
		}
		return fleet, err
	}

	create := func(delays ...scalev1alpha1.DPUAgentPhaseDelay) error {
		_, err := createFleet(delays...)
		return err
	}

	delay := func(phase string, minSeconds, maxSeconds int32) scalev1alpha1.DPUAgentPhaseDelay {
		return scalev1alpha1.DPUAgentPhaseDelay{Phase: phase, MinSeconds: minSeconds, MaxSeconds: maxSeconds}
	}

	It("accepts a delay for every phase the mock can delay", func() {
		var delays []scalev1alpha1.DPUAgentPhaseDelay
		for _, phase := range scalev1alpha1.MockDelayablePhases {
			delays = append(delays, delay(phase, 0, 3600))
		}
		Expect(create(delays...)).To(Succeed())
	})

	DescribeTable("rejects",
		func(message string, delays ...scalev1alpha1.DPUAgentPhaseDelay) {
			err := create(delays...)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error: %v", err)
			Expect(err.Error()).To(ContainSubstring(message))
		},
		Entry("an unknown phase", "Unsupported value", delay("Installing", 0, 1)),
		Entry("a phase the mock cannot lengthen", "Unsupported value", delay("Perform ARM Force Restart", 0, 1)),
		Entry("a phase that only makes Redfish calls", "Unsupported value", delay("Initialize Interface", 0, 1)),
		Entry("the same phase twice", "Duplicate value", delay("Rebooting", 0, 1), delay("Rebooting", 1, 2)),
		Entry("a minimum above the maximum", "minSeconds must not exceed maxSeconds", delay("Rebooting", 2, 1)),
		Entry("a negative minimum", "should be greater than or equal to 0", delay("Rebooting", -1, 1)),
		Entry("a maximum above an hour", "should be less than or equal to 3600", delay("OS Installing", 0, 3601)),
	)

	// The list is keyed by phase, so the immutability rule compares it as a
	// map: reordering is not a change, a new range is.
	It("rejects changing a phase delay after creation but not reordering", func() {
		fleet, err := createFleet(delay("OS Installing", 0, 1), delay("Rebooting", 0, 1))
		Expect(err).NotTo(HaveOccurred())

		fleet.Spec.DPUAgent.PhaseDelays = []scalev1alpha1.DPUAgentPhaseDelay{
			delay("Rebooting", 0, 1), delay("OS Installing", 0, 1),
		}
		Expect(k8sClient.Update(ctx, fleet)).To(Succeed())

		fleet.Spec.DPUAgent.PhaseDelays[0].MaxSeconds = 2
		err = k8sClient.Update(ctx, fleet)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.dpuAgent is immutable"))
	})
})
