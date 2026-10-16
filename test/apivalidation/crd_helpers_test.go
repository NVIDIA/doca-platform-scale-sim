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

func simFleetCRDPath() string {
	GinkgoHelper()
	_, file, _, ok := runtime.Caller(0)
	Expect(ok).To(BeTrue(), "runtime.Caller failed")
	return filepath.Join(filepath.Dir(file), "..", "..", "config", "crd", "bases", "scale.dpu.nvidia.com_simfleets.yaml")
}

func loadSimFleetCRD() *apiextensionsv1.CustomResourceDefinition {
	GinkgoHelper()
	data, err := os.ReadFile(simFleetCRDPath())
	Expect(err).NotTo(HaveOccurred(), "read SimFleet CRD")
	var crd apiextensionsv1.CustomResourceDefinition
	Expect(yaml.Unmarshal(data, &crd)).To(Succeed(), "unmarshal SimFleet CRD")
	return &crd
}

func simFleetSpecSchema() *apiextensionsv1.JSONSchemaProps {
	GinkgoHelper()
	crd := loadSimFleetCRD()
	Expect(crd.Spec.Versions).NotTo(BeEmpty(), "SimFleet CRD has no versions")
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	Expect(schema).NotTo(BeNil(), "SimFleet CRD schema is missing properties")
	Expect(schema.Properties).NotTo(BeNil(), "SimFleet CRD schema is missing properties")
	Expect(schema.Properties).To(HaveKey("spec"), "SimFleet CRD schema is missing spec")
	spec := schema.Properties["spec"]
	return &spec
}

func simFleetStatusSchema() *apiextensionsv1.JSONSchemaProps {
	GinkgoHelper()
	crd := loadSimFleetCRD()
	Expect(crd.Spec.Versions).NotTo(BeEmpty(), "SimFleet CRD has no versions")
	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	Expect(schema).NotTo(BeNil(), "SimFleet CRD schema is missing properties")
	Expect(schema.Properties).NotTo(BeNil(), "SimFleet CRD schema is missing properties")
	Expect(schema.Properties).To(HaveKey("status"), "SimFleet CRD schema is missing status")
	status := schema.Properties["status"]
	return &status
}

func hasValidationRule(rules []apiextensionsv1.ValidationRule, rule, message string) bool {
	for _, r := range rules {
		if r.Rule == rule && r.Message == message {
			return true
		}
	}
	return false
}

func propertyDefaultRaw(props map[string]apiextensionsv1.JSONSchemaProps, name string) ([]byte, bool) {
	p, ok := props[name]
	if !ok || p.Default == nil {
		return nil, false
	}
	return p.Default.Raw, true
}
