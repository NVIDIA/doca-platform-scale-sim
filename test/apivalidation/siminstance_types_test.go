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
	"os"
	"path/filepath"
	"runtime"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func loadSimInstanceCRD() *apiextensionsv1.CustomResourceDefinition {
	GinkgoHelper()
	_, file, _, ok := runtime.Caller(0)
	Expect(ok).To(BeTrue(), "runtime.Caller failed")
	path := filepath.Join(filepath.Dir(file), "..", "..", "config", "crd", "bases", "scale.dpu.nvidia.com_siminstances.yaml")
	data, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred(), "read SimInstance CRD")
	var crd apiextensionsv1.CustomResourceDefinition
	Expect(yaml.Unmarshal(data, &crd)).To(Succeed(), "unmarshal SimInstance CRD")
	Expect(crd.Spec.Versions).NotTo(BeEmpty(), "SimInstance CRD has no versions")
	Expect(crd.Spec.Versions[0].Schema).NotTo(BeNil(), "SimInstance CRD has no schema")
	Expect(crd.Spec.Versions[0].Schema.OpenAPIV3Schema).NotTo(BeNil(), "SimInstance CRD has no schema")
	return &crd
}

var _ = Describe("SimInstance CRD", func() {
	It("registers the smi short name", func() {
		crd := loadSimInstanceCRD()
		Expect(crd.Spec.Names.ShortNames).To(ContainElement("smi"))
	})

	// The ordinal and host index are the instance's identity: the name, the
	// children, and the DPUNode grouping all derive from them, so a changed value
	// would silently point the instance at someone else's inventory.
	DescribeTable("requires spec identity fields and makes them immutable",
		func(field string) {
			crd := loadSimInstanceCRD()
			Expect(crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties).To(HaveKey("spec"),
				"SimInstance CRD schema is missing spec")
			spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
			Expect(spec.Required).To(ContainElement(field))
			Expect(spec.Properties).To(HaveKey(field), "spec schema is missing %s", field)
			prop := spec.Properties[field]
			Expect(prop.Minimum).NotTo(BeNil(), "%s minimum", field)
			Expect(*prop.Minimum).To(BeZero(), "%s minimum", field)
			Expect(hasValidationRule(prop.XValidations, "self == oldSelf", field+" is immutable")).To(BeTrue(),
				"%s is missing the immutability rule: %v", field, prop.XValidations)
		},
		Entry("ordinal", "ordinal"),
		Entry("hostIndex", "hostIndex"),
	)

	It("enables the status subresource", func() {
		crd := loadSimInstanceCRD()
		sub := crd.Spec.Versions[0].Subresources
		Expect(sub).NotTo(BeNil(), "SimInstance CRD does not enable the status subresource")
		Expect(sub.Status).NotTo(BeNil(), "SimInstance CRD does not enable the status subresource")
	})

	It("prints the ordinal, host, phase, ready and age columns", func() {
		crd := loadSimInstanceCRD()
		var got []string
		for _, c := range crd.Spec.Versions[0].AdditionalPrinterColumns {
			got = append(got, c.Name)
		}
		Expect(got).To(Equal([]string{"Ordinal", "Host", "Phase", "Ready", "Age"}))
	})
})
