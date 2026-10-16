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

package dpf

import (
	provisioningv1 "github.com/nvidia/doca-platform/api/provisioning/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	scalev1alpha1 "github.com/NVIDIA/doca-platform-scale-sim/api/v1alpha1"
	"github.com/NVIDIA/doca-platform-scale-sim/internal/naming"
)

// nodeFleet has two hosts of two devices each.
func nodeFleet() *scalev1alpha1.SimFleet {
	return &scalev1alpha1.SimFleet{
		ObjectMeta: metav1.ObjectMeta{Name: "scale-3000", Namespace: "ns", UID: types.UID("uid")},
		Spec:       scalev1alpha1.SimFleetSpec{Replicas: 4, DevicesPerDPUNode: 2},
	}
}

func dpuRefs(names ...string) []provisioningv1.DPURef {
	refs := make([]provisioningv1.DPURef, 0, len(names))
	for _, name := range names {
		refs = append(refs, provisioningv1.DPURef{Name: name})
	}
	return refs
}

var _ = Describe("DesiredHostNode", func() {
	It("uses the DPUNode template", func() {
		fleet := nodeFleet()

		method := DesiredHostNode(fleet, 0, 4).Spec.NodeRebootMethod
		Expect(method).NotTo(BeNil(), "want External without a template")
		Expect(method.External).NotTo(BeNil(), "nodeRebootMethod = %+v, want External without a template", method)

		fleet.Spec.DPUNodeTemplate = &scalev1alpha1.DPUNodeTemplate{
			Metadata: scalev1alpha1.TemplateMetadata{Labels: map[string]string{
				"team":              "scale",
				naming.LabelOrdinal: "9999",
			}},
			Spec: scalev1alpha1.DPUNodeTemplateSpec{
				NodeRebootMethod: &provisioningv1.NodeRebootMethod{HostAgent: &provisioningv1.HostAgent{}},
			},
		}
		node := DesiredHostNode(fleet, 1, 4)

		Expect(node.Labels).To(HaveKeyWithValue("team", "scale"), "want the template label")
		Expect(node.Labels).To(HaveKeyWithValue(naming.LabelOrdinal, "0001"), "want the host index over the template")
		method = node.Spec.NodeRebootMethod
		Expect(method).NotTo(BeNil(), "want the template's HostAgent")
		Expect(method.HostAgent).NotTo(BeNil(), "nodeRebootMethod = %+v, want the template's HostAgent", method)
		Expect(method.External).To(BeNil(), "nodeRebootMethod = %+v, want the template's HostAgent", method)
		Expect(method).NotTo(BeIdenticalTo(fleet.Spec.DPUNodeTemplate.Spec.NodeRebootMethod),
			"nodeRebootMethod aliases the fleet template")
	})
})

// A node missing any desired ref has to be one the patch path can bring back
// into line, without dropping a ref someone else added.
var _ = Describe("AppendMissingDPUs", func() {
	DescribeTable("brings a node's refs into line without dropping any",
		func(existing, desired, want []provisioningv1.DPURef) {
			Expect(AppendMissingDPUs(existing, desired)).To(Equal(want))
		},
		Entry("appends every missing ref to an empty node", nil, dpuRefs("d0", "d1"), dpuRefs("d0", "d1")),
		Entry("leaves a node that already lists every desired ref",
			dpuRefs("d0", "d1"), dpuRefs("d0", "d1"), dpuRefs("d0", "d1")),
		Entry("keeps refs the fleet no longer desires",
			dpuRefs("d0", "stale"), dpuRefs("d0", "d1"), dpuRefs("d0", "stale", "d1")),
		Entry("restores a ref replaced by an equal number of foreign refs",
			dpuRefs("foreign"), dpuRefs("d0"), dpuRefs("foreign", "d0")),
	)
})
