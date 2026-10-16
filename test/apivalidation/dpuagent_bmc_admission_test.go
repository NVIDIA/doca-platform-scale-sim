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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
)

var _ = Describe("SimFleet spec.dpuAgent.bmc admission", func() {
	ctx := context.Background()
	var namespace string

	// No manager runs here: these specs only exercise API server admission.
	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "bmc-admission-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name
	})

	override := func(name string, methods []string, minSeconds, maxSeconds int) scalev1alpha1.DPUAgentResponseDelayOverride {
		return scalev1alpha1.DPUAgentResponseDelayOverride{
			Name: name, Methods: methods, MinSeconds: minSeconds, MaxSeconds: maxSeconds,
		}
	}

	create := func(bmc *scalev1alpha1.DPUAgentBMCSpec) (*scalev1alpha1.SimFleet, error) {
		fleet := &scalev1alpha1.SimFleet{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "bmc-admission-", Namespace: namespace},
			Spec: scalev1alpha1.SimFleetSpec{
				Replicas:              0,
				DevicesPerDPUNode:     1,
				DeviceSelectionLabels: map[string]string{"scale.dpu.nvidia.com/pool": "bmc-admission"},
				DPUAgent:              &scalev1alpha1.DPUAgentSpec{BMC: bmc},
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

	It("accepts a fleet that sets every field", func() {
		_, err := create(&scalev1alpha1.DPUAgentBMCSpec{
			PSID: ptr.To("MT_0000001775"),
			Firmware: &scalev1alpha1.DPUAgentFirmware{
				BMC: ptr.To("BF-24.07-10"), ERoT: ptr.To("mock-0.0"),
				UEFI: ptr.To("mock-0.0"), NIC: ptr.To("mock-0.0"),
			},
			ResponseDelay: &scalev1alpha1.DPUAgentResponseDelay{MinSeconds: 0, MaxSeconds: 1},
			ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
				override("System", nil, 1, 2),
				override("SecureBoot", []string{"GET"}, 1, 2),
				override("SecureBoot", []string{"PATCH"}, 5, 8),
				override("Task", []string{"*"}, 0, 0),
			},
		})
		Expect(err).NotTo(HaveOccurred())
	})

	DescribeTable("rejects",
		func(bmc *scalev1alpha1.DPUAgentBMCSpec, message string) {
			_, err := create(bmc)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error: %v", err)
			Expect(err.Error()).To(ContainSubstring(message))
		},
		Entry("a responseDelay whose minimum exceeds its maximum",
			&scalev1alpha1.DPUAgentBMCSpec{
				ResponseDelay: &scalev1alpha1.DPUAgentResponseDelay{MinSeconds: 3, MaxSeconds: 1},
			},
			"minSeconds must not exceed maxSeconds"),
		Entry("an override whose minimum exceeds its maximum",
			&scalev1alpha1.DPUAgentBMCSpec{ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
				override("Task", nil, 2, 1),
			}},
			"minSeconds must not exceed maxSeconds"),
		Entry("* combined with another method",
			&scalev1alpha1.DPUAgentBMCSpec{ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
				override("SecureBoot", []string{"*", "GET"}, 0, 1),
			}},
			`"*" cannot be combined with other methods`),
		Entry("a method the operation does not serve",
			&scalev1alpha1.DPUAgentBMCSpec{ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
				override("Task", []string{"POST"}, 0, 1),
			}},
			"methods must be served by the named Redfish operation"),
		Entry("one operation twice with every method",
			&scalev1alpha1.DPUAgentBMCSpec{ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
				override("System", nil, 0, 1),
				override("System", nil, 1, 2),
			}},
			"responseDelayOverrides must not cover the same operation and method twice"),
		Entry("a wildcard next to a single method of the same operation",
			&scalev1alpha1.DPUAgentBMCSpec{ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
				override("SecureBoot", []string{"*"}, 0, 1),
				override("SecureBoot", []string{"GET"}, 1, 2),
			}},
			"responseDelayOverrides must not cover the same operation and method twice"),
		Entry("two overrides sharing one method",
			&scalev1alpha1.DPUAgentBMCSpec{ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
				override("TruststoreCertificates", []string{"GET", "POST"}, 0, 1),
				override("TruststoreCertificates", []string{"POST"}, 1, 2),
			}},
			"responseDelayOverrides must not cover the same operation and method twice"),
		Entry("an unknown operation",
			&scalev1alpha1.DPUAgentBMCSpec{ResponseDelayOverrides: []scalev1alpha1.DPUAgentResponseDelayOverride{
				override("NoSuchOperation", nil, 0, 1),
			}},
			"Unsupported value"),
	)

	It("rejects changing spec.dpuAgent.bmc after creation", func() {
		fleet, err := create(&scalev1alpha1.DPUAgentBMCSpec{PSID: ptr.To("MT_0000001775")})
		Expect(err).NotTo(HaveOccurred())

		fleet.Spec.DPUAgent.BMC.PSID = ptr.To("MT_0000009999")
		err = k8sClient.Update(ctx, fleet)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "error: %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.dpuAgent is immutable"))
	})
})
